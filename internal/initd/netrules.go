package initd

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// Security-critical: the network rules (spec 4.4). This file is pure; the
// netlink side is in nftapply.go.

// Verdict is a rule's action.
type Verdict string

const (
	Accept Verdict = "accept"
	// Reject blocks with an ICMP admin-prohibited error, so connect() fails
	// at once. A plain drop would leave agents hanging on every blocked
	// connection until the TCP timeout.
	Reject Verdict = "reject with icmpx admin-prohibited"
)

// Rule is one rule in the output chain of table inet asb. All set fields
// must match.
type Rule struct {
	OIF        string       // output interface name
	Family     int          // 4 or 6; implied by Dst when that is set
	Dst        netip.Prefix // destination prefix
	Proto      uint8        // L4 protocol
	DPort      uint16       // destination port (needs Proto tcp or udp)
	ICMPv6Type uint8        // ICMPv6 type (needs Proto ipv6-icmp)
	Verdict    Verdict
}

const (
	protoTCP    = 6
	protoUDP    = 17
	protoICMPv6 = 58
)

// NetInputs are what _init reads from the container before building rules.
type NetInputs struct {
	Resolvers  []netip.Addr   // nameservers from /etc/resolv.conf
	OnLink     []netip.Prefix // prefixes of every interface except lo
	AllowCIDRs []netip.Prefix // allow_cidrs
}

// privateSet is always rejected in restricted mode, on top of the on-link
// prefixes.
var privateSet = mustPrefixes(
	"0.0.0.0/8", "127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"100.64.0.0/10", "169.254.0.0/16", "224.0.0.0/4", "255.255.255.255/32",
	"::1/128", "fc00::/7", "fe80::/10", "ff00::/8",
	"::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48",
)

// pastaDNS is where pasta forwards DNS; only resolvers in it are reachable.
var pastaDNS = netip.MustParsePrefix("169.254.0.0/16")

// ndpTypes are the ICMPv6 neighbor discovery messages IPv6 needs to work.
// Dropping ff00::/8 and fe80::/10 would otherwise block them. They cannot
// carry data to the LAN, and pasta answers them itself.
var ndpTypes = []uint8{133, 135, 136} // router solicit, neighbor solicit, neighbor advert

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// RestrictedRules builds the restricted-mode rules, in order:
// lo, NDP, DNS to pasta's resolver, allow_cidrs, then reject the private
// set and the on-link prefixes. The chain policy accepts everything else.
func RestrictedRules(in NetInputs) ([]Rule, error) {
	var dns []netip.Addr
	for _, r := range in.Resolvers {
		if r.Is4() && pastaDNS.Contains(r) && !slices.Contains(dns, r) {
			dns = append(dns, r)
		}
	}
	if len(dns) == 0 {
		return nil, fmt.Errorf("no link-local nameserver (pasta's DNS forwarder) in resolv.conf among %v", in.Resolvers)
	}

	rules := []Rule{{OIF: "lo", Verdict: Accept}}
	for _, t := range ndpTypes {
		rules = append(rules, Rule{Family: 6, Proto: protoICMPv6, ICMPv6Type: t, Verdict: Accept})
	}
	for _, r := range dns {
		dst := netip.PrefixFrom(r, r.BitLen())
		rules = append(rules,
			Rule{Dst: dst, Proto: protoUDP, DPort: 53, Verdict: Accept},
			Rule{Dst: dst, Proto: protoTCP, DPort: 53, Verdict: Accept})
	}
	for _, p := range dedupe(in.AllowCIDRs) {
		rules = append(rules, Rule{Dst: p, Verdict: Accept})
	}
	for _, p := range privateSet {
		rules = append(rules, Rule{Dst: p, Verdict: Reject})
	}
	for _, p := range dedupe(in.OnLink) {
		if covered(p, privateSet) {
			continue
		}
		rules = append(rules, Rule{Dst: p, Verdict: Reject})
	}
	return rules, nil
}

// dedupe masks, unmaps and deduplicates prefixes, keeping order.
func dedupe(ps []netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range ps {
		p = normalize(p)
		if p.IsValid() && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// normalize turns an IPv4-mapped IPv6 prefix into the IPv4 prefix (pasta
// and net.Interfaces may report either form) and masks it.
func normalize(p netip.Prefix) netip.Prefix {
	if p.Addr().Is4In6() && p.Bits() >= 96 {
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
	}
	return p.Masked()
}

// covered reports whether p lies inside one of set.
func covered(p netip.Prefix, set []netip.Prefix) bool {
	for _, q := range set {
		if q.Bits() <= p.Bits() && q.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

func (r Rule) family() int {
	if r.Dst.IsValid() {
		if r.Dst.Addr().Is4() {
			return 4
		}
		return 6
	}
	return r.Family
}

var icmpv6Names = map[uint8]string{133: "nd-router-solicit", 135: "nd-neighbor-solicit", 136: "nd-neighbor-advert"}

// String renders the rule in nft syntax.
func (r Rule) String() string {
	var parts []string
	if r.OIF != "" {
		parts = append(parts, fmt.Sprintf("oifname %q", r.OIF))
	}
	switch {
	case r.Dst.IsValid():
		kw := "ip"
		if r.family() == 6 {
			kw = "ip6"
		}
		dst := r.Dst.String()
		if r.Dst.IsSingleIP() {
			dst = r.Dst.Addr().String()
		}
		parts = append(parts, kw+" daddr "+dst)
	case r.Family == 4:
		parts = append(parts, "meta nfproto ipv4")
	case r.Family == 6:
		parts = append(parts, "meta nfproto ipv6")
	}
	switch {
	case r.DPort != 0:
		parts = append(parts, fmt.Sprintf("%s dport %d", map[uint8]string{protoTCP: "tcp", protoUDP: "udp"}[r.Proto], r.DPort))
	case r.ICMPv6Type != 0:
		name := icmpv6Names[r.ICMPv6Type]
		if name == "" {
			name = fmt.Sprint(r.ICMPv6Type)
		}
		parts = append(parts, "icmpv6 type "+name)
	case r.Proto != 0:
		parts = append(parts, fmt.Sprintf("meta l4proto %d", r.Proto))
	}
	return strings.Join(append(parts, string(r.Verdict)), " ")
}

// Render renders the whole ruleset in nft syntax, as _init applies it.
func Render(rules []Rule) string {
	var b strings.Builder
	b.WriteString("table inet asb {\n\tchain output {\n\t\ttype filter hook output priority filter; policy accept;\n")
	for _, r := range rules {
		b.WriteString("\t\t" + r.String() + "\n")
	}
	b.WriteString("\t}\n}\n")
	return b.String()
}
