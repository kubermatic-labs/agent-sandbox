// Package proxy is the allowlist mode's HTTP proxy (spec 4.4), run as
// `agent-sandbox _proxy` under its own uid. It tunnels CONNECT requests and
// forwards plain-HTTP absolute-URI requests to allowed hosts only. There is
// no TLS interception.
//
// The proxy does the DNS lookups. Its own traffic is held to the restricted
// rules by nftables, so an allowed name that resolves to a private or LAN
// address still cannot reach it.
package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// Policy decides which targets are allowed.
type Policy struct {
	Domains []string       // exact names, or *.suffix (subdomains only)
	CIDRs   []netip.Prefix // IP literals are allowed only inside these
}

// Allowed reports whether host (a name or an IP literal, without port) may
// be reached.
func (p Policy) Allowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "" {
		return false
	}
	if a, err := netip.ParseAddr(host); err == nil {
		a = a.Unmap().WithZone("")
		for _, c := range p.CIDRs {
			if c.Contains(a) {
				return true
			}
		}
		return false
	}
	for _, d := range p.Domains {
		d = strings.ToLower(d)
		if suffix, ok := strings.CutPrefix(d, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
		} else if host == d {
			return true
		}
	}
	return false
}

// Server is the proxy handler. Create it with New.
type Server struct {
	Policy Policy
	Dial   func(ctx context.Context, network, addr string) (net.Conn, error)
	Log    io.Writer

	tr *http.Transport // plain-HTTP forwarding, shared so connections are reused
}

// New returns a server that dials with a 30 s connect timeout. Plain-HTTP
// requests share one transport, so upstream connections are reused: pooled
// connections are keyed by host:port, and every request is checked against
// the policy first, so reuse never reaches a host the request was not
// allowed to. The transport dials through s.Dial at dial time, so Dial may
// be replaced after New.
func New(p Policy, log io.Writer) *Server {
	d := &net.Dialer{Timeout: 30 * time.Second}
	s := &Server{
		Policy: p,
		Dial:   d.DialContext,
		Log:    log,
		tr: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return d.DialContext(ctx, network, addr)
			},
			ResponseHeaderTimeout: 2 * time.Minute,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
		},
	}

	return s
}

// hopHeaders are removed when forwarding plain HTTP.
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func (s *Server) deny(w http.ResponseWriter, what, host string) {
	fmt.Fprintf(s.Log, "agent-sandbox _proxy: denied %s %s\n", what, host)
	http.Error(w, fmt.Sprintf("agent-sandbox: %s is not in allow_domains", host), http.StatusForbidden)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		s.connect(w, r)
		return
	}
	if !r.URL.IsAbs() || (r.URL.Scheme != "http") {
		http.Error(w, "agent-sandbox: only CONNECT and absolute http:// requests are proxied", http.StatusBadRequest)
		return
	}
	if !s.Policy.Allowed(r.URL.Hostname()) {
		s.deny(w, r.Method, r.URL.Hostname())
		return
	}
	s.forward(w, r)
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil || port == "" {
		http.Error(w, "agent-sandbox: CONNECT needs host:port", http.StatusBadRequest)
		return
	}
	if !s.Policy.Allowed(host) {
		s.deny(w, "CONNECT", host)
		return
	}
	upstream, err := s.Dial(r.Context(), "tcp", r.Host)
	if err != nil {
		fmt.Fprintf(s.Log, "agent-sandbox _proxy: CONNECT %s: %v\n", r.Host, err)
		http.Error(w, "agent-sandbox: "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	// Bytes the client sent after the CONNECT request (a TLS ClientHello)
	// may already sit in the buffer.
	if n := buf.Reader.Buffered(); n > 0 {
		b, _ := buf.Peek(n)
		if _, err := upstream.Write(b); err != nil {
			_ = client.Close()
			_ = upstream.Close()
			return
		}
	}
	pipe(client, upstream)
}

// pipe copies both ways until both directions are done.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	_ = a.Close()
	_ = b.Close()
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request) {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	resp, err := s.tr.RoundTrip(out)
	if err != nil {
		fmt.Fprintf(s.Log, "agent-sandbox _proxy: %s %s: %v\n", r.Method, r.URL.Host, err)
		http.Error(w, "agent-sandbox: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// ListenAndServe serves the proxy on addr.
func ListenAndServe(addr string, s *Server) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s,
		ReadHeaderTimeout: 30 * time.Second,
	}
	return srv.ListenAndServe()
}
