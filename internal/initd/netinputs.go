package initd

import (
	"bufio"
	"bytes"
	"net"
	"net/netip"
	"strings"
)

// ParseResolvConf returns the nameserver addresses in a resolv.conf.
func ParseResolvConf(b []byte) []netip.Addr {
	var out []netip.Addr
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		a, err := netip.ParseAddr(f[1])
		if err != nil {
			continue
		}
		out = append(out, a.WithZone("").Unmap())
	}
	return out
}

// OnLinkPrefixes returns the address prefixes of every interface except
// loopback. Under pasta these are the host's on-link subnets.
func OnLinkPrefixes() ([]netip.Prefix, error) {
	addrs, err := InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]netip.Prefix, len(addrs))
	for i, a := range addrs {
		out[i] = a.Masked()
	}
	return out, nil
}

// InterfaceAddrs returns the address of every interface except loopback,
// with its prefix length and host bits kept (192.0.2.7/24). The wrapper
// calls it on the host for --host-addr.
func InterfaceAddrs() ([]netip.Prefix, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			addr, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			ones, _ := ipn.Mask.Size()
			if addr.Is4In6() {
				addr = addr.Unmap()
			}
			out = append(out, netip.PrefixFrom(addr.WithZone(""), ones))
		}
	}
	return out, nil
}
