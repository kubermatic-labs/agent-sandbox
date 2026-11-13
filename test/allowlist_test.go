//go:build integration

package test

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

const allowlistConfig = "network = \"allowlist\"\nallow_domains = [\"example.com\", \"*.iana.org\"]\n"

// curl runs curl in the sandbox and returns its output and exit code. The
// session's https_proxy/http_proxy point at the allowlist proxy.
func (e *sandboxEnv) curl(args ...string) (string, int) {
	e.t.Helper()
	r := e.run(e.root, "", append([]string{"--", "curl", "-sS", "--max-time", "20"}, args...)...)
	return r.stdout + r.stderr, r.code
}

func TestAllowlistAllowsListedDomains(t *testing.T) {
	e := newProject(t, allowlistConfig)
	for _, u := range []string{"https://example.com/", "https://www.iana.org/"} {
		if out, code := e.curl("-o", "/dev/null", "-w", "%{http_code}", u); code != 0 || out != "200" {
			t.Errorf("%s: exit %d, %q", u, code, out)
		}
	}
	// A built-in default: any HTTP status means the tunnel worked.
	if out, code := e.curl("-o", "/dev/null", "-w", "%{http_code}", "https://api.anthropic.com/"); code != 0 || out == "000" {
		t.Errorf("api.anthropic.com: exit %d, %q", code, out)
	}
}

func TestAllowlistDeniesOtherDomains(t *testing.T) {
	e := newProject(t, allowlistConfig)
	for _, u := range []string{"https://www.wikipedia.org/", "https://iana.org/", "https://example.com.evil.test/"} {
		if out, code := e.curl("-o", "/dev/null", u); code == 0 || !strings.Contains(out, "403") {
			t.Errorf("%s: exit %d, %q", u, code, out)
		}
	}
	if out, code := e.curl("-o", "/dev/null", "-w", "%{http_code}", "http://neverssl.com/"); code != 0 || out != "403" {
		t.Errorf("plain http: exit %d, %q", code, out)
	}
}

func TestAllowlistNoDirectEgress(t *testing.T) {
	e := newProject(t, allowlistConfig)
	if out, code := e.curl("--noproxy", "*", "-o", "/dev/null", "https://example.com/"); code == 0 {
		t.Errorf("bypassing the proxy worked: %q", out)
	}
	if r := e.run(e.root, "", "--", "getent", "hosts", "example.com"); r.code == 0 {
		t.Error("sessions must not resolve names directly")
	}
	e.mustBeBlocked("1.1.1.1", "443")
	e.mustBeBlocked("169.254.1.1", "53") // pasta's DNS: proxy only
}

func TestAllowlistProxyCannotReachHost(t *testing.T) {
	// An allowed name that resolves to the host is still unreachable: the
	// proxy's own traffic is held to the restricted rules.
	e := newProject(t, "network = \"allowlist\"\nallow_domains = [\"host.containers.internal\"]\n")
	port := hostListener(t)
	if out, code := e.curl("-o", "/dev/null", "-w", "%{http_code}", "http://host.containers.internal:"+port+"/"); code != 0 || out != "502" {
		t.Errorf("allowed name resolving to the host: exit %d, %q", code, out)
	}
	if out, code := e.curl("-o", "/dev/null", "-w", "%{http_code}", "http://169.254.1.2:"+port+"/"); code != 0 || out != "403" {
		t.Errorf("IP literal not in allow_cidrs: exit %d, %q", code, out)
	}
}

func TestSessionCannotKillProxy(t *testing.T) {
	e := newProject(t, allowlistConfig)
	r := e.run(e.root, "", "--", "sh", "-c", "pid=$(pgrep -u 65533) && test -n \"$pid\" && echo found && kill -9 $pid")
	if r.code == 0 || !strings.Contains(r.stdout, "found") || !strings.Contains(r.stderr, "Operation not permitted") {
		t.Errorf("killing the proxy as the session user: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if out, code := e.curl("-o", "/dev/null", "-w", "%{http_code}", "https://example.com/"); code != 0 || out != "200" {
		t.Errorf("proxy gone: exit %d, %q", code, out)
	}
}

func TestProxyDeathStopsSandbox(t *testing.T) {
	e := newProject(t, allowlistConfig)
	e.mustRun(e.root, "", "--", "true")
	id, _ := e.container()
	// Only the proxy's own uid can kill it.
	if out, err := exec.Command("podman", "exec", "--user", "65533:65533", id, "pkill", "-f", "agent-sandbox _proxy").CombinedOutput(); err != nil {
		t.Fatalf("pkill as the proxy uid: %v: %s", err, out)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, state := e.container(); state == "exited" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sandbox kept running without its proxy")
		}
		time.Sleep(500 * time.Millisecond)
	}
	r := e.run(e.root, "", "--", "true")
	if r.code != 0 || !strings.Contains(r.stderr, "the proxy exited") {
		t.Errorf("restart: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestProxyIsUnprivileged(t *testing.T) {
	e := newProject(t, allowlistConfig)
	got := e.mustRun(e.root, "", "--", "sh", "-c",
		`grep -E '^(Uid|Gid|Groups|CapPrm|CapEff|CapAmb|NoNewPrivs):' /proc/$(pgrep -u 65533)/status`)
	want := "Uid:\t65533\t65533\t65533\t65533\n" +
		"Gid:\t65533\t65533\t65533\t65533\n" +
		"Groups:\t \n" +
		"CapPrm:\t0000000000000000\n" +
		"CapEff:\t0000000000000000\n" +
		"CapAmb:\t0000000000000000\n" +
		"NoNewPrivs:\t1\n"
	if got != want {
		t.Errorf("proxy status:\n%s\nwant:\n%s", got, want)
	}
}
