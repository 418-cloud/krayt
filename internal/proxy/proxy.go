// Package proxy is krayt's egress allowlist proxy (§6.6): a small HTTP/HTTPS CONNECT forward
// proxy that checks each request's host against the per-task policy. Since the
// move-egress-proxy-to-host task it runs as its own process on the HOST — `krayt
// __egress-proxy` (internal/cli) execs the binary named here and hands it a fd-3 listener —
// not inside the guest VM. The guest's only egress-side component is
// cmd/krayt-vsock-forward, a dumb TCP<->vsock pipe that parses nothing; this package is the
// entire L7 decision engine.
//
// The package is deliberately OS-agnostic and build-tag-free even though it now runs
// host-side only: it must still cross-compile for linux/arm64 (it is a dependency of nothing
// in the guest, but the module as a whole is), and keeping it free of host-specific imports
// keeps the swap seam below honest.
//
// The logic here is unit-tested directly. The implementation is deliberately behind the
// Factory seam so it can be swapped for elazarl/goproxy, or a memory-safe reimplementation in
// another language honoring the same flags/fd-3/log contract (§6.6), without touching the
// process that execs it — see KRAYT_EGRESS_PROXY_BIN in internal/orchestrator.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
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
// canceled. It is what the `krayt __egress-proxy` hidden subcommand runs on its fd-3
// listener (§6.6, internal/cli).
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

// HandRolled is the default allowlist forward proxy: it tunnels CONNECT (HTTPS) and
// forwards plain HTTP, allowing a request only if its host passes the policy (§6.6). It
// resolves through the host's system resolver (respecting the user's VPN/split-horizon/
// corporate DNS), same as any ordinary process on the machine it runs on.
func HandRolled(p Policy) http.Handler {
	return HandRolledDNS(p, "")
}

// HandRolledDNS is HandRolled with an explicit DNS server override (the `--dns` flag on
// `krayt __egress-proxy`). An empty dnsServer means "use the system resolver" — that is the
// default; DNS no longer resolves in the VM's network context, but in the host's (§6.6).
func HandRolledDNS(p Policy, dnsServer string) http.Handler {
	// Control fires once per resolved address the dialer tries, just before connect, so the
	// post-resolution SSRF guard (checkDialAddr, §6.6) covers every A/AAAA answer and every
	// Happy-Eyeballs attempt, closing the DNS-rebinding window (the resolved IP is checked,
	// not the name). This one dialer backs both the CONNECT tunnel dial and the HTTP
	// transport dial, so both paths are guarded.
	d := &net.Dialer{
		Timeout:  dialTimeout,
		Resolver: resolverVia(dnsServer),
		Control: func(_, address string, _ syscall.RawConn) error {
			return checkDialAddr(address)
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

// dialFunc dials an upstream address.
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

// resolverVia returns a *net.Resolver that dials dnsServer for every lookup, or nil (the
// system resolver) when dnsServer is empty — the default since this package moved host-side
// (§6.6): there is no longer a nftables uid lock to route DNS around, so the ordinary system
// resolver is both simpler and more correct (it respects the user's VPN/split-horizon/
// corporate DNS, which a hardcoded server would not).
func resolverVia(dnsServer string) *net.Resolver {
	if dnsServer == "" {
		return nil
	}
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
// does not cover; it is treated like a private range (always blocked, §6.6).
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// metadataIP is the cloud instance-metadata address, always refused (also caught by the
// link-local check, but named explicitly to make the intent unmissable).
var metadataIP = netip.MustParseAddr("169.254.169.254")

// blockedAddrMsg is the operator-facing 403 body for a target checkDialAddr refused (§6.6) —
// worded generically since the block covers loopback/link-local/multicast/unspecified/
// metadata/private/CGNAT ranges, and can fire for a request host that was already an IP
// literal (no resolution involved).
func blockedAddrMsg(host string) string {
	return "krayt: egress to " + host + " targets a blocked address range"
}

// checkDialAddr is the post-resolution SSRF guard (§6.6). It runs on the *resolved* ip:port
// of every upstream dial and refuses, in EVERY mode including full — no exceptions: loopback,
// link-local (uni/multicast), the cloud metadata IP, the unspecified address, multicast, and
// RFC 1918 / RFC 4193 (ULA) private ranges plus the RFC 6598 CGNAT range.
//
// This is unconditional (no mode carve-out) because the proxy now runs on the HOST: with the
// dialer inside a VM, "allow mode=full to reach RFC1918" meant "the VM's own NAT segment is
// reachable"; with the dialer on the host, the identical carve-out would mean "the sandbox can
// reach the user's real LAN and loopback services from a trusted host process" — a materially
// worse trade, so it is refused unconditionally instead of widened (§6.6, move-egress-proxy-
// to-host.md §2). A local Ollama/LM Studio on 127.0.0.1 or a LAN package mirror is therefore
// unreachable from the sandbox in every mode; that is a deliberate, documented casualty, not
// an oversight — a purpose-built named-forward-target mechanism is the follow-up, not a range
// unblock.
//
// It is fail-closed: an unparseable address is refused. It does not consult the host-string
// allowlist — that check already ran in the handler; this guard is strictly additional.
func checkDialAddr(address string) error {
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
	if ip.IsPrivate() || cgnat.Contains(ip) {
		return fmt.Errorf("%w: %s (private/LAN range, blocked in every mode — host/LAN "+
			"services are not a supported egress target, §6.6)", errBlockedAddr, ip)
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
		// net/http's CONNECT-proxy client path (what every proxy-aware caller, including
		// hack/netprobe, uses) discards the response body on a non-2xx CONNECT reply — the
		// caller only ever sees the status text ("Bad Gateway"), never this message. Log it
		// server-side so the real reason (DNS failure, connection refused, timeout, …) is
		// visible in proxy.log (§6.6, §9 of move-egress-proxy-to-host.md), the only place a
		// denial reason survives.
		log.Printf("krayt-egress-proxy: CONNECT %s: upstream dial failed: %v", r.Host, err)
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
		log.Printf("krayt-egress-proxy: %s %s: upstream request failed: %v", r.Method, requestHost(r), err)
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
