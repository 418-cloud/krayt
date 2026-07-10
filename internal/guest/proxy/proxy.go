// Package proxy is the in-guest egress allowlist proxy of §6.6: a small HTTP/HTTPS CONNECT
// forward proxy that checks each request's host against the per-task policy. It is the L7
// half of the egress control; the L3 nftables lock (firewall_linux.go) makes it
// unbypassable by dropping all egress except loopback and the proxy's own uid.
//
// The proxy logic here is OS-agnostic and unit-tested directly. The implementation is
// deliberately behind the Factory seam so it can be swapped for elazarl/goproxy or any
// other backend without touching the guest wiring (the user chose hand-rolled for v1).
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// Policy modes (§6.6). Mirrors the proto NetworkPolicy.Mode / task.NetworkMode as strings.
const (
	ModeAllowlist = "allowlist" // default: only listed hosts may be reached
	ModeFull      = "full"      // allow everything (nftables also opened; explicit opt-in)
	ModeNone      = "none"      // deny everything
)

// Policy is the per-task egress policy the proxy enforces (§6.6).
type Policy struct {
	Mode  string
	Allow []string
}

// Factory builds the forward-proxy handler for a policy. This is the swap seam: HandRolled
// is the default; a goproxy-based factory (or any other) can replace it by matching this
// signature and being passed to Serve.
type Factory func(Policy) http.Handler

// dialTimeout bounds how long the proxy waits to connect to an allowed upstream.
const dialTimeout = 30 * time.Second

// Serve runs a forward proxy built by factory (HandRolled if nil) on lis until ctx is
// canceled. It is what cmd/krayt-proxy runs as the dedicated proxyd uid (§6.6).
func Serve(ctx context.Context, lis net.Listener, p Policy, factory Factory) error {
	if factory == nil {
		factory = HandRolled
	}
	srv := &http.Server{Handler: factory(p), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(lis); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("proxy: serve: %w", err)
	}
	return nil
}

// DefaultDNSServer is where the proxy resolves names. Resolution is done by the proxy
// itself (dialed as proxyd), not the system stub resolver — the stub runs as a different
// uid and its upstream queries are dropped by the nftables egress lock, so a stub-based
// lookup fails under `allowlist`/`none` (§6.6).
const DefaultDNSServer = "1.1.1.1:53"

// HandRolled is the default allowlist forward proxy: it tunnels CONNECT (HTTPS) and
// forwards plain HTTP, allowing a request only if its host passes the policy (§6.6). It
// resolves via DefaultDNSServer.
func HandRolled(p Policy) http.Handler {
	return HandRolledDNS(p, DefaultDNSServer)
}

// HandRolledDNS is HandRolled with an explicit DNS server (the krayt-proxy --dns flag).
func HandRolledDNS(p Policy, dnsServer string) http.Handler {
	// Control fires once per resolved address the dialer tries, just before connect, with
	// the resolved ip:port — so the post-resolution SSRF guard (checkDialAddr, §6.6) covers
	// every A/AAAA answer and every Happy-Eyeballs attempt, closing the DNS-rebinding window
	// (the resolved IP is checked, not the name). This one dialer backs both the CONNECT
	// tunnel dial and the HTTP transport dial, so both paths are guarded.
	d := &net.Dialer{
		Timeout:  dialTimeout,
		Resolver: resolverVia(dnsServer),
		Control: func(_, address string, _ syscall.RawConn) error {
			return checkDialAddr(p.Mode, address)
		},
	}
	tr := &http.Transport{
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return newHandler(p, tr, d.DialContext)
}

// dialFunc dials an upstream address (resolving as proxyd, §6.6).
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// newHandler builds the proxy with an injectable transport + dialer (tests pass fakes so no
// real network is needed).
func newHandler(p Policy, rt http.RoundTripper, dial dialFunc) *handler {
	allow := make(map[string]bool, len(p.Allow))
	for _, a := range p.Allow {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			allow[a] = true
		}
	}
	return &handler{mode: p.Mode, allow: allow, transport: rt, dial: dial}
}

// resolverVia forces DNS through dnsServer, dialed by the proxy (proxyd) so the query is
// permitted by the nftables lock rather than routed through a stub resolver on another uid.
func resolverVia(dnsServer string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, dnsServer)
		},
	}
}

// errBlockedAddr marks a dial refused by the post-resolution SSRF guard (§6.6). Wrapped so
// the address is preserved for logs while the handler can errors.Is it to a clear 403.
var errBlockedAddr = errors.New("krayt: dial target resolves to a blocked address")

// cgnat is the RFC 6598 shared-address (carrier-grade NAT) range, which netip's IsPrivate
// does not cover; it is treated like a private range (blocked unless mode == full).
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// metadataIP is the cloud instance-metadata address, always refused (also caught by the
// link-local check, but named explicitly to make the intent unmissable).
var metadataIP = netip.MustParseAddr("169.254.169.254")

// blockedAddrMsg is the operator-facing 403 body for a target checkDialAddr refused (§6.6) —
// worded generically since the block covers loopback/link-local/multicast/unspecified/metadata
// and (mode-dependent) private/CGNAT ranges, and can fire for a request host that was already an
// IP literal (no resolution involved).
func blockedAddrMsg(host string) string {
	return "krayt: egress to " + host + " targets a blocked address range"
}

// checkDialAddr is the post-resolution SSRF guard (§6.6). It runs on the *resolved* ip:port
// of every upstream dial and refuses:
//   - always, in every mode: loopback, link-local (uni/multicast), the cloud metadata IP,
//     the unspecified address, and multicast;
//   - unless mode == full: RFC 1918 / RFC 4193 (ULA) private ranges and the RFC 6598 CGNAT
//     range.
//
// It is fail-closed: an unparseable address is refused. It does not consult the host-string
// allowlist — that check already ran in the handler; this guard is strictly additional.
func checkDialAddr(mode, address string) error {
	host := address
	if h, _, err := net.SplitHostPort(address); err == nil {
		host = h
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: unparseable address %q", errBlockedAddr, address)
	}
	ip = ip.Unmap() // treat IPv4-mapped IPv6 (::ffff:a.b.c.d) as its IPv4 form
	switch {
	case ip.IsLoopback(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsMulticast(), ip.IsUnspecified(), ip == metadataIP:
		return fmt.Errorf("%w: %s (loopback/link-local/metadata)", errBlockedAddr, ip)
	}
	if mode != ModeFull && (ip.IsPrivate() || cgnat.Contains(ip)) {
		return fmt.Errorf("%w: %s (private range, allowed only in full mode)", errBlockedAddr, ip)
	}
	return nil
}

type handler struct {
	mode      string
	allow     map[string]bool
	transport http.RoundTripper
	dial      dialFunc
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := requestHost(r)
	if !h.allowed(host) {
		http.Error(w, "krayt: egress to "+host+" is blocked by the network policy", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodConnect {
		h.connect(w, r)
		return
	}
	h.forward(w, r)
}

// allowed applies the policy to a bare host (no port).
func (h *handler) allowed(host string) bool {
	switch h.mode {
	case ModeFull:
		return true
	case ModeNone:
		return false
	default: // allowlist
		return h.allow[strings.ToLower(host)]
	}
}

// connect tunnels an HTTPS CONNECT to the (already allowed) target, copying bytes both ways.
func (h *handler) connect(w http.ResponseWriter, r *http.Request) {
	upstream, err := h.dial(r.Context(), "tcp", r.Host)
	if err != nil {
		if errors.Is(err, errBlockedAddr) {
			http.Error(w, blockedAddrMsg(requestHost(r)), http.StatusForbidden)
			return
		}
		http.Error(w, "krayt: upstream dial failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "krayt: hijacking unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		_ = upstream.Close()
		_ = client.Close()
		return
	}
	go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close() }()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
}

// forward proxies a plain-HTTP request to the (already allowed) target.
func (h *handler) forward(w http.ResponseWriter, r *http.Request) {
	r.RequestURI = "" // must be cleared before re-sending as a client request
	resp, err := h.transport.RoundTrip(r)
	if err != nil {
		if errors.Is(err, errBlockedAddr) {
			http.Error(w, blockedAddrMsg(requestHost(r)), http.StatusForbidden)
			return
		}
		http.Error(w, "krayt: upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// requestHost extracts the bare hostname (no port) a request targets. For CONNECT the
// authority is in r.Host; for a plain proxied request it is in the absolute r.URL.
func requestHost(r *http.Request) string {
	host := r.Host
	if r.Method != http.MethodConnect && r.URL != nil && r.URL.Host != "" {
		host = r.URL.Host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
