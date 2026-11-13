//go:build integration

package test

import (
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// hostListener accepts TCP connections on every host address and returns
// its port. Connections are closed immediately.
func hostListener(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// connect tries a TCP connection from inside the sandbox and returns the
// combined output and whether it connected.
func (e *sandboxEnv) connect(host, port string) (string, bool) {
	e.t.Helper()
	r := e.run(e.root, "", "--", "timeout", "5", "bash", "-c", `exec 3<>/dev/tcp/$0/$1`, host, port)
	return r.stdout + r.stderr, r.code == 0
}

// mustBeBlocked asserts the connection is refused by the sandbox's own
// rules: the reject in the output hook fails connect() at once with "No
// route to host" (ICMP admin-prohibited), unlike a missing service
// ("Connection refused") or a remote drop (a timeout).
func (e *sandboxEnv) mustBeBlocked(host, port string) {
	e.t.Helper()
	out, ok := e.connect(host, port)
	if ok || !strings.Contains(out, blockedError) {
		e.t.Errorf("%s:%s: want %q from the sandbox rules, got ok=%v: %q", host, port, blockedError, ok, out)
	}
}

const blockedError = "No route to host"

// defaultGateway returns the host's IPv4 default gateway, or "".
func defaultGateway(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("ip", "-4", "route", "show", "default").Output()
	if err != nil {
		return ""
	}
	f := strings.Fields(string(out))
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "via" {
			return f[i+1]
		}
	}
	return ""
}

func TestOpenNetworkReachesHost(t *testing.T) {
	// Control for the restricted tests: the listener is reachable at all.
	e := newProject(t, openNetwork)
	if out, ok := e.connect("host.containers.internal", hostListener(t)); !ok {
		t.Fatalf("open mode cannot reach the host listener; the restricted tests would prove nothing: %s", out)
	}
}

func TestRestrictedBlocksHostAndLAN(t *testing.T) {
	e := newProject(t, "") // restricted is the default
	port := hostListener(t)
	e.mustBeBlocked("host.containers.internal", port)
	e.mustBeBlocked("169.254.1.2", port) // the same, by address
	e.mustBeBlocked("169.254.1.1", "80") // pasta's DNS forwarder, other port
	if gw := defaultGateway(t); gw != "" {
		e.mustBeBlocked(gw, "53") // the LAN router, which is also the LAN resolver here
		e.mustBeBlocked(gw, "80")
	} else {
		t.Log("no IPv4 default gateway; skipping LAN gateway checks")
	}
	// LAN resolvers podman copies into resolv.conf are blocked too. Public
	// ones (8.8.8.8) are internet hosts, which restricted mode allows.
	rc := e.mustRun(e.root, "", "--", "cat", "/etc/resolv.conf")
	for _, line := range strings.Split(rc, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || f[0] != "nameserver" {
			continue
		}
		a, err := netip.ParseAddr(f[1])
		if err != nil || !a.Is4() || a.IsLinkLocalUnicast() {
			continue
		}
		if a.IsPrivate() || netip.MustParsePrefix("100.64.0.0/10").Contains(a) {
			e.mustBeBlocked(f[1], "53")
		}
	}
}

func TestRestrictedAllowsDNSAndInternet(t *testing.T) {
	e := newProject(t, "")
	if r := e.run(e.root, "", "--", "getent", "hosts", "example.com"); r.code != 0 {
		t.Fatalf("DNS through pasta failed: %s", r.stderr)
	}
	if out, ok := e.connect("example.com", "443"); !ok {
		t.Errorf("internet unreachable in restricted mode: %s", out)
	}
}

func TestRestrictedAllowCIDR(t *testing.T) {
	e := newProject(t, "allow_cidrs = [\"169.254.1.2\"]\n")
	port := hostListener(t)
	if out, ok := e.connect("host.containers.internal", port); !ok {
		t.Errorf("allow_cidrs did not open host.containers.internal: %s", out)
	}
	e.mustBeBlocked("169.254.1.1", "80")
}

func TestSessionCannotFlushRules(t *testing.T) {
	e := newProject(t, "")
	b, err := os.ReadFile(nftflushBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.root, "nftflush"), b, 0o755); err != nil {
		t.Fatal(err)
	}
	r := e.run(e.root, "", "--", "./nftflush")
	if r.code == 0 || !strings.Contains(r.stdout, "operation not permitted") {
		t.Errorf("flushing the ruleset as the session user: exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	e.mustBeBlocked("host.containers.internal", hostListener(t))
}

func TestRestrictedRulesAreLogged(t *testing.T) {
	e := newProject(t, "")
	e.mustRun(e.root, "", "--", "true")
	id, _ := e.container()
	out, err := exec.Command("podman", "logs", id).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"table inet asb", "ip daddr 169.254.0.0/16 reject", "udp dport 53 accept"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("logs lack %q:\n%s", want, out)
		}
	}
}
