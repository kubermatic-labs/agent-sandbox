package initd

import (
	"encoding/binary"
	"fmt"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// applyRules replaces the network namespace's whole ruleset with table inet
// asb holding rules, in one netlink transaction.
func applyRules(rules []Rule) error {
	exprs := make([][]expr.Any, len(rules))
	for i, r := range rules {
		e, err := ruleExprs(r)
		if err != nil {
			return fmt.Errorf("rule %q: %w", r, err)
		}
		exprs[i] = e
	}

	c, err := nftables.New()
	if err != nil {
		return err
	}
	c.FlushRuleset()
	t := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: "asb"})
	policy := nftables.ChainPolicyAccept
	ch := c.AddChain(&nftables.Chain{
		Name:     "output",
		Table:    t,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookOutput,
		Priority: nftables.ChainPriorityFilter,
		Policy:   &policy,
	})
	for _, e := range exprs {
		c.AddRule(&nftables.Rule{Table: t, Chain: ch, Exprs: e})
	}
	return c.Flush()
}

func cmpEq(data []byte) *expr.Cmp {
	return &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: data}
}

// ruleExprs translates a Rule into nftables expressions.
func ruleExprs(r Rule) ([]expr.Any, error) {
	var e []expr.Any
	if r.OIF != "" {
		if len(r.OIF) >= unix.IFNAMSIZ {
			return nil, fmt.Errorf("interface name %q too long", r.OIF)
		}
		name := make([]byte, unix.IFNAMSIZ)
		copy(name, r.OIF)
		e = append(e, &expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1}, cmpEq(name))
	}

	addrLen, addrOff := uint32(4), uint32(16) // IPv4 daddr
	switch r.family() {
	case 4:
		e = append(e, &expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1}, cmpEq([]byte{unix.NFPROTO_IPV4}))
	case 6:
		e = append(e, &expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1}, cmpEq([]byte{unix.NFPROTO_IPV6}))
		addrLen, addrOff = 16, 24 // IPv6 daddr
	case 0:
		if r.Dst.IsValid() || r.ICMPv6Type != 0 {
			return nil, fmt.Errorf("address family required")
		}
	}

	if r.Dst.IsValid() {
		e = append(e, &expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: addrOff, Len: addrLen})
		if bits := r.Dst.Bits(); bits < int(addrLen*8) {
			e = append(e, &expr.Bitwise{
				SourceRegister: 1, DestRegister: 1, Len: addrLen,
				Mask: prefixMask(bits, int(addrLen)), Xor: make([]byte, addrLen),
			})
		}
		e = append(e, cmpEq(r.Dst.Masked().Addr().AsSlice()))
	}

	if r.Proto != 0 {
		e = append(e, &expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1}, cmpEq([]byte{r.Proto}))
	}
	if r.DPort != 0 {
		if r.Proto != protoTCP && r.Proto != protoUDP {
			return nil, fmt.Errorf("port match needs tcp or udp")
		}
		port := make([]byte, 2)
		binary.BigEndian.PutUint16(port, r.DPort)
		e = append(e, &expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2}, cmpEq(port))
	}
	if r.ICMPv6Type != 0 {
		if r.Proto != protoICMPv6 {
			return nil, fmt.Errorf("icmpv6 type match needs ipv6-icmp")
		}
		e = append(e, &expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 1}, cmpEq([]byte{r.ICMPv6Type}))
	}

	switch r.Verdict {
	case Accept:
		e = append(e, &expr.Verdict{Kind: expr.VerdictAccept})
	case Reject:
		e = append(e, &expr.Reject{Type: unix.NFT_REJECT_ICMPX_UNREACH, Code: unix.NFT_REJECT_ICMPX_ADMIN_PROHIBITED})
	default:
		return nil, fmt.Errorf("unknown verdict %q", r.Verdict)
	}
	return e, nil
}

// prefixMask returns a byte mask with the first bits bits set.
func prefixMask(bits, n int) []byte {
	m := make([]byte, n)
	for i := range m {
		switch {
		case bits >= 8:
			m[i] = 0xff
			bits -= 8
		case bits > 0:
			m[i] = byte(0xff << (8 - bits))
			bits = 0
		}
	}
	return m
}
