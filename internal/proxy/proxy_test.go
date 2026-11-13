package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func TestPolicy(t *testing.T) {
	p := Policy{
		Domains: []string{"api.anthropic.com", "*.example.com"},
		CIDRs:   []netip.Prefix{netip.MustParsePrefix("10.0.0.5/32"), netip.MustParsePrefix("2001:db8::/32")},
	}
	for host, want := range map[string]bool{
		"api.anthropic.com":      true,
		"API.Anthropic.COM":      true,
		"api.anthropic.com.":     true,
		"anthropic.com":          false,
		"evil-api.anthropic.com": false,
		"api.anthropic.com.evil": false,
		"www.example.com":        true,
		"a.b.example.com":        true,
		"example.com":            false,
		"notexample.com":         false,
		"10.0.0.5":               true,
		"10.0.0.6":               false,
		"[2001:db8::1]":          true,
		"::ffff:10.0.0.5":        true,
		"1.1.1.1":                false,
		"":                       false,
	} {
		if got := p.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// newProxy starts the proxy with every allowed connection dialed to target.
func newProxy(t *testing.T, domains []string, target string) (*httptest.Server, *strings.Builder) {
	t.Helper()
	log := &strings.Builder{}
	s := &Server{
		Policy: Policy{Domains: domains},
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, target)
		},
		Log: log,
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv, log
}

func client(proxyURL string, tlsCfg *tls.Config) *http.Client {
	u, _ := url.Parse(proxyURL)
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u), TLSClientConfig: tlsCfg}}
}

func TestConnectTunnel(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "secure hello")
	}))
	defer upstream.Close()
	proxySrv, _ := newProxy(t, []string{"allowed.test"}, upstream.Listener.Addr().String())

	c := client(proxySrv.URL, &tls.Config{InsecureSkipVerify: true})
	resp, err := c.Get("https://allowed.test/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "secure hello" {
		t.Errorf("body = %q", b)
	}

	_, err = c.Get("https://denied.test/")
	if err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("denied CONNECT: %v", err)
	}
}

func TestPlainHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Connection") != "" {
			t.Error("hop-by-hop header forwarded")
		}
		w.Header().Set("X-Up", "1")
		_, _ = io.WriteString(w, "plain "+r.URL.Path)
	}))
	defer upstream.Close()
	proxySrv, log := newProxy(t, []string{"allowed.test"}, upstream.Listener.Addr().String())
	c := client(proxySrv.URL, nil)

	resp, err := c.Get("http://allowed.test/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "plain /x" || resp.Header.Get("X-Up") != "1" {
		t.Errorf("body %q, headers %v", b, resp.Header)
	}

	resp, err = c.Get("http://denied.test/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "denied.test is not in allow_domains") {
		t.Errorf("denied: %d %q", resp.StatusCode, b)
	}
	if !strings.Contains(log.String(), "denied GET denied.test") {
		t.Errorf("denial not logged: %q", log.String())
	}
}

func TestBadRequests(t *testing.T) {
	proxySrv, _ := newProxy(t, []string{"allowed.test"}, "127.0.0.1:1")
	// An origin-form request is not a proxy request.
	resp, err := http.Get(proxySrv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("origin-form: %d", resp.StatusCode)
	}

	// CONNECT to an allowed host whose dial fails: 502.
	conn, err := net.Dial("tcp", proxySrv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "CONNECT allowed.test:443 HTTP/1.1\r\nHost: allowed.test:443\r\n\r\n")
	r, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusBadGateway {
		t.Errorf("failed dial: %d", r.StatusCode)
	}
}
