// Package egressproxy implements the runner-embedded default-deny egress
// proxy that contains the acceptance agent's lethal-trifecta shape
// (ADR-050 / #1532, decision #1 of the acceptance security posture).
//
// The acceptance agent is the one Fishhawk agent that deliberately holds
// code execution + network access + credentials at once, and the running
// instance it drives can render attacker-controlled data, so the agent is
// treated as potentially prompt-injected. This proxy is the containment:
// the acceptance invocation is forced through it (HTTP(S)_PROXY, composed
// by runner/internal/acceptenv), and the proxy default-denies every
// destination except the composed allow-list — exactly (1) the workflow
// spec's declared egress target host(s), (2) the model API endpoint, and
// (3) the Fishhawk backend for the signature-authed evidence ship.
//
// Enforcement notes, honest about the boundary:
//   - HTTP(S)_PROXY constrains cooperating HTTP clients. A hostile child
//     process could open a raw socket and bypass any userspace proxy;
//     socket-level denial needs an OS sandbox and is tracked as the
//     documented residual (see docs/ARCHITECTURE.md §6, and the same
//     residual noted for gate subprocesses in #611).
//   - CONNECT tunnels are opaque TLS: the proxy admits or denies by
//     host:port only and never sees plaintext.
//   - DNS rebinding is countered two ways: each allow-listed hostname's
//     resolution is PINNED at first use for the proxy's lifetime, and a
//     public hostname that resolves to a loopback/private/link-local
//     address is refused outright (a rebinding-shaped answer). Literal IP
//     and localhost entries are dialed as declared — a dev target on
//     localhost is a legitimate, operator-declared destination.
package egressproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultModelHosts are the model API endpoints admitted for the acceptance
// agent (allow-list class 2, ADR-050 decision #1). Ports default to 443/80
// per the host-only entry rule.
var DefaultModelHosts = []string{"api.anthropic.com", "api.openai.com"}

// BuildAllowlist composes the full acceptance egress allow-list from its
// three ADR-050 classes: the spec-declared target hosts (the only
// customer-controlled entries), the model API endpoints, and the Fishhawk
// backend (parsed from its base URL so the evidence ship stays reachable).
// An unparseable or hostless backend URL is skipped rather than admitting
// a malformed entry.
func BuildAllowlist(targetHosts []string, backendURL string) []string {
	out := make([]string, 0, len(targetHosts)+len(DefaultModelHosts)+1)
	out = append(out, targetHosts...)
	out = append(out, DefaultModelHosts...)
	if u, err := url.Parse(backendURL); err == nil && u.Host != "" {
		out = append(out, u.Host)
	}
	return out
}

// Config configures Start.
type Config struct {
	// AllowHosts is the default-deny allow-list. Each entry is host or
	// host:port (no scheme, no path). A host-only entry permits the default
	// HTTP/HTTPS ports (80, 443) only; a host:port entry permits exactly
	// that port. Matching is case-insensitive and exact — no wildcards, no
	// subdomain expansion.
	AllowHosts []string
	// Logf receives one line per admitted/denied decision (never request
	// bodies). Nil discards.
	Logf func(format string, args ...any)
	// LookupIP overrides DNS resolution (tests). Nil uses net.DefaultResolver.
	LookupIP func(ctx context.Context, host string) ([]net.IP, error)
	// DialTimeout bounds each upstream dial. Zero means 10s.
	DialTimeout time.Duration
}

// entry is one parsed allow-list entry.
type entry struct {
	host    string // lowercase host (or IP literal)
	port    string // "" for a host-only entry (default ports 80/443)
	literal bool   // host is an IP literal or a localhost name
}

// Proxy is a running egress proxy. Close it when the acceptance invocation
// exits.
type Proxy struct {
	ln      net.Listener
	srv     *http.Server
	entries []entry
	logf    func(string, ...any)
	lookup  func(ctx context.Context, host string) ([]net.IP, error)
	dialTO  time.Duration

	mu     sync.Mutex
	pinned map[string][]net.IP // hostname -> first successful resolution, for the proxy's lifetime

	closeOnce sync.Once
	closeErr  error
}

// Start parses the allow-list, binds 127.0.0.1:0, and serves the proxy.
func Start(cfg Config) (*Proxy, error) {
	if len(cfg.AllowHosts) == 0 {
		return nil, errors.New("egressproxy: empty allow-list — a default-deny proxy with nothing allowed cannot serve an acceptance invocation")
	}
	entries := make([]entry, 0, len(cfg.AllowHosts))
	for _, raw := range cfg.AllowHosts {
		e, err := parseEntry(raw)
		if err != nil {
			return nil, fmt.Errorf("egressproxy: allow-list entry %q: %w", raw, err)
		}
		entries = append(entries, e)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("egressproxy: listen: %w", err)
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	lookup := cfg.LookupIP
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			ips := make([]net.IP, 0, len(addrs))
			for _, a := range addrs {
				ips = append(ips, a.IP)
			}
			return ips, nil
		}
	}
	dialTO := cfg.DialTimeout
	if dialTO == 0 {
		dialTO = 10 * time.Second
	}
	p := &Proxy{
		ln:      ln,
		entries: entries,
		logf:    logf,
		lookup:  lookup,
		dialTO:  dialTO,
		pinned:  make(map[string][]net.IP),
	}
	p.srv = &http.Server{
		Handler:           http.HandlerFunc(p.handle),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// URL returns the proxy address in the form HTTP(S)_PROXY expects.
func (p *Proxy) URL() string {
	return "http://" + p.ln.Addr().String()
}

// Close shuts the listener down. Idempotent.
func (p *Proxy) Close() error {
	p.closeOnce.Do(func() { p.closeErr = p.srv.Close() })
	return p.closeErr
}

// parseEntry validates one allow-list entry as host or host:port.
func parseEntry(raw string) (entry, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return entry{}, errors.New("empty")
	}
	if strings.Contains(s, "/") || strings.Contains(s, "://") {
		return entry{}, errors.New("must be host or host:port, not a URL")
	}
	host, port := s, ""
	if h, pt, err := net.SplitHostPort(s); err == nil {
		host, port = h, pt
	}
	if host == "" {
		return entry{}, errors.New("empty host")
	}
	return entry{host: host, port: port, literal: isLocalName(host) || net.ParseIP(host) != nil}, nil
}

// isLocalName reports whether host names the local loopback by convention.
func isLocalName(host string) bool {
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

// match returns the allow-list entry admitting host:port, if any.
func (p *Proxy) match(host, port string) (entry, bool) {
	host = strings.ToLower(host)
	for _, e := range p.entries {
		if e.host != host {
			continue
		}
		if e.port == "" {
			if port == "80" || port == "443" {
				return e, true
			}
			continue
		}
		if e.port == port {
			return e, true
		}
	}
	return entry{}, false
}

// handle routes CONNECT tunnels and absolute-form plain-HTTP forwards.
func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleForward(w, r)
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		p.deny(w, r.Host, "CONNECT target must be host:port")
		return
	}
	e, ok := p.match(host, port)
	if !ok {
		p.deny(w, r.Host, "host is not on the acceptance egress allow-list")
		return
	}
	upstream, err := p.dial(r.Context(), e, host, port)
	if err != nil {
		p.logf("egress dial %s failed: %v", r.Host, err)
		http.Error(w, "egress dial failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "proxy cannot hijack connection", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		http.Error(w, "hijack failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = client.Close() }()
	_, _ = buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	_ = buf.Flush()
	p.logf("egress CONNECT %s allowed", r.Host)

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, buf); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

func (p *Proxy) handleForward(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() {
		http.Error(w, "egress proxy requires absolute-form requests", http.StatusBadRequest)
		return
	}
	host := r.URL.Hostname()
	port := r.URL.Port()
	if port == "" {
		if r.URL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	e, ok := p.match(host, port)
	if !ok {
		p.deny(w, r.URL.Host, "host is not on the acceptance egress allow-list")
		return
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return p.dial(ctx, e, host, port)
		},
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	resp, err := transport.RoundTrip(out)
	if err != nil {
		http.Error(w, "egress forward failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	p.logf("egress %s %s allowed", r.Method, r.URL.Host)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// deny writes the 403 with a precise reason naming the denied destination.
func (p *Proxy) deny(w http.ResponseWriter, target, reason string) {
	p.logf("egress to %s DENIED: %s", target, reason)
	http.Error(w,
		fmt.Sprintf("egress to %s denied: %s (ADR-050 default-deny; permitted destinations are the spec-declared target hosts, the model API endpoint, and the Fishhawk backend)", target, reason),
		http.StatusForbidden)
}

// dial connects to the admitted host:port, pinning DNS for hostnames.
func (p *Proxy) dial(ctx context.Context, e entry, host, port string) (net.Conn, error) {
	d := &net.Dialer{Timeout: p.dialTO}
	if e.literal {
		// Operator-declared IP literal or localhost name: dial as declared.
		return d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	}
	ips, err := p.pinnedIPs(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, ip := range ips {
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// pinnedIPs returns host's pinned resolution, resolving (and pinning) on
// first use. A public hostname resolving to loopback/private/link-local
// space is refused — the DNS-rebinding shape ADR-050 names.
func (p *Proxy) pinnedIPs(ctx context.Context, host string) ([]net.IP, error) {
	p.mu.Lock()
	if ips, ok := p.pinned[host]; ok {
		p.mu.Unlock()
		return ips, nil
	}
	p.mu.Unlock()

	resolved, err := p.lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	ips := make([]net.IP, 0, len(resolved))
	for _, ip := range resolved {
		if rebindingShaped(ip) {
			return nil, fmt.Errorf("resolve %s: answer %s is loopback/private/link-local — refusing a rebinding-shaped resolution for a public hostname", host, ip)
		}
		ips = append(ips, ip)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve %s: no usable addresses", host)
	}

	p.mu.Lock()
	// First resolution wins; a concurrent racer's answer is equivalent
	// (both passed the rebinding check before pinning).
	if prior, ok := p.pinned[host]; ok {
		ips = prior
	} else {
		p.pinned[host] = ips
	}
	p.mu.Unlock()
	return ips, nil
}

// rebindingShaped reports whether ip is in address space a public hostname
// has no business resolving into.
func rebindingShaped(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}
