package initd

import (
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/nftables/expr"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(string(want), got); diff != "" {
		t.Errorf("%s mismatch (-want +got):\n%s", path, diff)
	}
}

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

func TestRestrictedRulesGolden(t *testing.T) {
	rules, err := RestrictedRules(NetInputs{
		Resolvers: addrs("169.254.1.1", "192.168.18.1", "169.254.1.1"),
		OnLink: mustPrefixes(
			"192.168.18.0/24",      // covered by 192.168.0.0/16
			"203.0.113.0/24",       // public IPv4 LAN
			"2001:db8:1:2::/64",    // global IPv6 LAN
			"2001:db8:1:2::abc/64", // same, unmasked
			"fc00:f853:ccd:e793::/64",
			"fe80::/64",
		),
		AllowCIDRs: mustPrefixes("10.0.0.5/32", "192.168.1.0/24", "fd00::1/128"),
	})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "restricted", Render(rules))
}

func TestRestrictedRulesNeedsPastaResolver(t *testing.T) {
	if _, err := RestrictedRules(NetInputs{Resolvers: addrs("192.168.18.1", "fe80::1")}); err == nil {
		t.Error("rules built without a link-local IPv4 resolver")
	}
}

func TestParseResolvConf(t *testing.T) {
	got := ParseResolvConf([]byte("# podman\nsearch lan\nnameserver 169.254.1.1\nnameserver 192.168.18.1\nnameserver fe80::1%eth0\nnameserver bogus\noptions edns0\n"))
	want := addrs("169.254.1.1", "192.168.18.1", "fe80::1")
	if diff := cmp.Diff(want, got, cmp.Comparer(func(a, b netip.Addr) bool { return a == b })); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestNormalize(t *testing.T) {
	if got := normalize(netip.MustParsePrefix("::ffff:192.168.1.7/120")); got != netip.MustParsePrefix("192.168.1.0/24") {
		t.Errorf("4in6 = %v", got)
	}
}

func TestPrefixMask(t *testing.T) {
	for _, tt := range []struct {
		bits, n int
		want    []byte
	}{
		{8, 4, []byte{0xff, 0, 0, 0}},
		{12, 4, []byte{0xff, 0xf0, 0, 0}},
		{10, 4, []byte{0xff, 0xc0, 0, 0}},
		{32, 4, []byte{0xff, 0xff, 0xff, 0xff}},
		{7, 16, append([]byte{0xfe}, make([]byte, 15)...)},
	} {
		if got := prefixMask(tt.bits, tt.n); !cmp.Equal(got, tt.want) {
			t.Errorf("prefixMask(%d, %d) = %x, want %x", tt.bits, tt.n, got, tt.want)
		}
	}
}

func TestRuleExprs(t *testing.T) {
	// 172.16.0.0/12 reject: nfproto ipv4, daddr at offset 16, /12 mask, compare, reject.
	got, err := ruleExprs(Rule{Dst: netip.MustParsePrefix("172.16.0.0/12"), Verdict: Reject})
	if err != nil {
		t.Fatal(err)
	}
	want := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{2}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: []byte{0xff, 0xf0, 0, 0}, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{172, 16, 0, 0}},
		&expr.Reject{Type: 2, Code: 3},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}

	// DNS to a single IPv4 address: no bitwise mask, udp port at transport offset 2.
	got, err = ruleExprs(Rule{Dst: netip.MustParsePrefix("169.254.1.1/32"), Proto: protoUDP, DPort: 53, Verdict: Accept})
	if err != nil {
		t.Fatal(err)
	}
	want = []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{2}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{169, 254, 1, 1}},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{17}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{0, 53}},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}

	// IPv6 /64: offset 24, 16 bytes.
	got, err = ruleExprs(Rule{Dst: netip.MustParsePrefix("2001:db8:1:2::/64"), Verdict: Reject})
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := got[2].(*expr.Payload); !ok || p.Offset != 24 || p.Len != 16 {
		t.Errorf("IPv6 payload = %#v", got[2])
	}
	if c, ok := got[1].(*expr.Cmp); !ok || c.Data[0] != 10 {
		t.Errorf("IPv6 nfproto = %#v", got[1])
	}

	// lo: interface name NUL-padded to IFNAMSIZ.
	got, err = ruleExprs(Rule{OIF: "lo", Verdict: Accept})
	if err != nil {
		t.Fatal(err)
	}
	if c := got[1].(*expr.Cmp); len(c.Data) != 16 || string(c.Data[:3]) != "lo\x00" {
		t.Errorf("oifname data = %q", c.Data)
	}

	for name, bad := range map[string]Rule{
		"port without proto": {Dst: netip.MustParsePrefix("1.1.1.1/32"), DPort: 53, Verdict: Accept},
		"no verdict":         {OIF: "lo"},
		"icmp without proto": {Family: 6, ICMPv6Type: 135, Verdict: Accept},
	} {
		if _, err := ruleExprs(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAllRestrictedRulesTranslate(t *testing.T) {
	rules, err := RestrictedRules(NetInputs{
		Resolvers:  addrs("169.254.1.1"),
		OnLink:     mustPrefixes("203.0.113.0/24", "2001:db8::/64"),
		AllowCIDRs: mustPrefixes("10.0.0.5/32"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if _, err := ruleExprs(r); err != nil {
			t.Errorf("%s: %v", r, err)
		}
	}
}
