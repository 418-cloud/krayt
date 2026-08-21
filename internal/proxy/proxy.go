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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Policy modes (§6.6). Mirrors the proto NetworkPolicy.Mode / task.NetworkMode as strings.
const (
	ModeAllowlist = "allowlist" // default: only listed hosts may be reached
	ModeFull      = "full"      // allow everything (nftables also opened; explicit opt-in)
	ModeNone      = "none"      // deny everything
)

// Policy is the per-task egress policy the proxy enforces (§6.6,
// add-tls-mitm-credential-injection.md). MITM/Passthrough/Inject are all host-only — they never
// ride the guest protocol; only the resulting CA certificate does (§5).
type Policy struct {
	Mode  string
	Allow []string

	MITM        bool         // terminate TLS and allow header injection; default false (§1)
	Passthrough []string     // hosts tunneled (never MITM'd) even when MITM is on
	Inject      []InjectRule // per-host header injection rules; requires MITM

	// LogRequests turns on the request-observation log (observe.go): one line per handled
	// request carrying the request line, host, and header NAMES only. Off by default — without
	// it proxy.log records only failures and denials, so a successful run leaves it empty.
	LogRequests bool

	// LogHeaderValues names headers whose VALUES may also be logged (implies LogRequests). For
	// recording an API's required non-secret opt-in headers exactly rather than guessing them; a
	// credential-bearing name is reduced to its shape instead (observe.go's credentialShape).
	LogHeaderValues []string
}

// InjectRule is one resolved network.inject[] rule (§1, §4.5): for a MITM'd request to Host,
// delete every header named in Strip, then set every header in Set and SetLiteral. Set/SetLiteral
// values here are already the real header values to attach — any secrets-file key name in the
// user's config (task.InjectRule.Set) is resolved to its value host-side, before this type is
// built, so this package never has any notion of a "secrets file".
type InjectRule struct {
	Host       string
	Strip      []string
	Set        map[string]string // header -> resolved value
	SetLiteral map[string]string // header -> literal value
	Refresh    *RefreshRule      // optional host-side credential refresh (plumbing only, §4.6)
}

// RefreshRule declaratively names an upstream credential-refresh endpoint for one InjectRule.
// The proxy stays generic: it provides only the mechanism (RefreshFunc, one refresh + one retry
// on a 401, §4.6) — constructing the actual refresh request and parsing its response is
// vendor-specific and belongs in a per-agent adapter (§6.14); this task ships the plumbing only.
type RefreshRule struct {
	Host                string
	PathPrefix          string
	ResponseTokenFields []string
}

// RefreshFunc performs one InjectRule's host-side credential refresh exactly once and returns
// the replacement header values to retry the request with. nil (the default built by
// BuildHandler) means no refresh capability is wired: a 401 is then surfaced to the agent as-is.
type RefreshFunc func(ctx context.Context, rule InjectRule) (map[string]string, error)

// StdinConfig is the JSON document the run supervisor writes to the `krayt __egress-proxy`
// child's stdin, once, then closes (§2b). Secret material — resolved header values for
// network.inject[].set — rides here, never on argv or in env: flags land in the process table
// and env is readable from /proc/<pid>/environ; stdin, read to EOF then closed, is neither.
// Non-secret policy (--mode/--allow/--dns/--mitm) stays on flags, unchanged from before this
// task; only config that can carry a secret value moved here.
type StdinConfig struct {
	Passthrough []string     `json:"passthrough"`
	Inject      []InjectRule `json:"inject"`
}

// ReadStdinConfig reads and parses r (the child's stdin) to EOF. An empty stream (no bytes at
// all — the parent always writes at least "{}", but a hand-invoked or older caller might not)
// decodes as the zero value rather than an error, so this never blocks a run over an empty
// config.
func ReadStdinConfig(r io.Reader) (StdinConfig, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return StdinConfig{}, fmt.Errorf("proxy: read stdin config: %w", err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return StdinConfig{}, nil
	}
	var cfg StdinConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return StdinConfig{}, fmt.Errorf("proxy: parse stdin config: %w", err)
	}
	return cfg, nil
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
	return ServeHandler(ctx, lis, factory(p))
}

// ServeHandler runs h as a forward-proxy handler on lis until ctx is canceled — the same server
// loop Serve uses, exposed directly for a caller that built its handler via BuildHandler (the
// `krayt __egress-proxy` child, so it can retain the *CA reference for the fd-4 handoff, §2b/§5).
func ServeHandler(ctx context.Context, lis net.Listener, h http.Handler) error {
	return serveHandler(ctx, lis, h, outerIdleTimeout, maxAcceptedConns)
}

// Outer-server resource bounds. The guest is the only client, but it is the untrusted side: it
// chooses how many connections to open and how long to leave them idle, and every accepted
// connection costs the HOST an fd and a goroutine for the rest of the run (krayt-vsock-forward
// opens one vsock connection per guest-side TCP connection, by design, so each becomes one accept
// here).
const (
	// outerIdleTimeout closes a keep-alive connection the guest is not using. Matches the inner
	// MITM server (connect_mitm.go). Without it Go sets NO read deadline at all on an idle
	// connection once its first request has been read — ReadHeaderTimeout stops applying then —
	// so one completed request (even a 403 under mode: none) pinned an fd until teardown.
	outerIdleTimeout = 120 * time.Second

	// maxOuterHeaderBytes bounds one request's headers, as maxMITMHeaderBytes does for the
	// decrypted inner request. Go's 1 MiB default is the same number; setting it explicitly keeps
	// the bound a decision rather than an inherited default.
	maxOuterHeaderBytes = 1 << 20

	// maxAcceptedConns caps concurrently accepted connections. Sized well above what a real agent
	// run needs — a busy agent holds a handful of keep-alive connections plus a few CONNECT
	// tunnels, and the transport pools upstream connections separately (MaxIdleConns: 100) — while
	// still bounding a hostile guest to a fixed cost. Excess connections wait in the kernel's
	// accept backlog rather than being refused, so a legitimate burst is delayed, never dropped.
	maxAcceptedConns = 256
)

// serveHandler is ServeHandler with the resource bounds injectable, so tests can assert them
// without waiting out outerIdleTimeout or opening maxAcceptedConns sockets.
func serveHandler(ctx context.Context, lis net.Listener, h http.Handler, idle time.Duration, maxConns int) error {
	// Reject rather than clamp an unusable cap. Both bad values fail silently or violently inside
	// make(chan): maxConns < 0 panics ("makechan: size out of range"), and maxConns == 0 leaves an
	// unbuffered sem no Accept can ever send on, so the proxy would listen while serving nothing.
	// Clamping to 1 instead would trade that for an equally puzzling fully serialized proxy.
	if maxConns < 1 {
		return fmt.Errorf("proxy: serve: maxConns must be >= 1, got %d", maxConns)
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       idle,
		MaxHeaderBytes:    maxOuterHeaderBytes,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(newLimitListener(lis, maxConns)); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("proxy: serve: %w", err)
	}
	return nil
}

// limitListener caps the number of connections accepted from an underlying listener at once. It
// is a local ~30 lines rather than golang.org/x/net/netutil because that module is not in the
// pinned dependency list (§9.1) and this is all of it that is needed.
//
// A slot is taken before Accept and released on the accepted conn's Close — which is what makes
// this correct on the hijacked CONNECT/MITM paths too: http.Server stops tracking a hijacked
// connection, but tunnel() and connectMITM() still Close it (directly, or via the inner
// http.Server), and Close is the only release path.
type limitListener struct {
	net.Listener
	sem       chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// newLimitListener requires n >= 1; serveHandler, its only caller, rejects anything less.
func newLimitListener(l net.Listener, n int) *limitListener {
	return &limitListener{Listener: l, sem: make(chan struct{}, n), done: make(chan struct{})}
}

func (l *limitListener) Accept() (net.Conn, error) {
	// Waiting on done as well as on a free slot is what lets Close unblock a listener sitting at
	// its cap: without it, ctx cancellation could not stop a Serve loop whose every slot is held
	// by a hijacked tunnel http.Server will never close for us.
	select {
	case l.sem <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.sem }}, nil
}

func (l *limitListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return l.Listener.Close()
}

// limitConn releases its listener slot exactly once, however many times it is closed (both the
// tunnel path and http.Server's own cleanup can close the same connection).
type limitConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
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
	h, _, err := buildHandler(p, dnsServer, "", nil)
	if err != nil {
		// newCA only fails on a crypto/rand read error — effectively never in practice.
		// HandRolledDNS's signature (unchanged since before this task) has no error return, so
		// degrade to a handler that always fails closed rather than panic or silently run
		// without MITM.
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "krayt: mitm CA initialization failed: "+err.Error(), http.StatusInternalServerError)
		})
	}
	return h
}

// BuildHandler is HandRolledDNS's superset: it also returns the run's ephemeral MITM CA (nil
// when p.MITM is false), for a caller that needs the CA's public cert — the `krayt
// __egress-proxy` child, to hand it back to the parent over fd 4 (§2b, §5). runID (may be
// empty) is folded into the CA's CN for operator legibility only.
func BuildHandler(p Policy, dnsServer, runID string) (http.Handler, *CA, error) {
	return buildHandler(p, dnsServer, runID, nil)
}

// BuildHandlerWithRefresh is BuildHandler plus a RefreshFunc seam (§4.6) for a caller — a future
// per-agent adapter, step 3 — that can actually perform a rule's host-side credential refresh.
// nil behaves exactly like BuildHandler (no refresh capability; a 401 is surfaced as-is).
func BuildHandlerWithRefresh(p Policy, dnsServer, runID string, refresh RefreshFunc) (http.Handler, *CA, error) {
	return buildHandler(p, dnsServer, runID, refresh)
}

func buildHandler(p Policy, dnsServer, runID string, refresh RefreshFunc) (*handler, *CA, error) {
	// Control fires once per resolved address the dialer tries, just before connect, so the
	// post-resolution SSRF guard (checkDialAddr, §6.6) covers every A/AAAA answer and every
	// Happy-Eyeballs attempt, closing the DNS-rebinding window (the resolved IP is checked,
	// not the name). This one dialer backs the CONNECT tunnel dial, the plain-HTTP transport
	// dial, AND (add-tls-mitm-credential-injection.md §4) the MITM reverse proxy's upstream
	// dial, so all three paths are guarded identically.
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
	var ca *CA
	if p.MITM {
		var err error
		ca, err = newCA(runID)
		if err != nil {
			return nil, nil, err
		}
	}
	return newHandler(p, tr, d.DialContext, ca, refresh), ca, nil
}

// dialFunc dials an upstream address.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// newHandler builds the proxy with an injectable transport + dialer (tests pass fakes so no
// real network is needed) and an optional CA (nil disables the MITM path entirely, §4).
func newHandler(p Policy, rt http.RoundTripper, dial dialFunc, ca *CA, refresh RefreshFunc) *handler {
	// Every map key is folded with normalizeHost — the SAME fold ServeHTTP applies to the
	// request host. Both sides of a lookup must agree byte for byte: folding config entries
	// with a different function (strings.ToLower, say) is exactly the divergence that made the
	// allowlist approve one host while the transport dialed another. An entry normalizeHost
	// refuses (non-ASCII, so never equal to any request host — those are refused at the choke
	// point) is dropped rather than stored under a key nothing can ever match. Such an entry
	// never reaches here in a real run: task.ValidateNetworkPolicy applies the same acceptance
	// rule at pre-flight and fails the run before any VM boots, so the two stages cannot disagree
	// about the effective policy. This drop is the belt-and-braces half of that pair, for a
	// Policy built by some other route (a test, a future caller).
	allow := make(map[string]bool, len(p.Allow))
	for _, a := range p.Allow {
		if a, ok := normalizeHost(a); ok {
			allow[a] = true
		}
	}
	passthrough := make(map[string]bool, len(p.Passthrough))
	for _, h := range p.Passthrough {
		if h, ok := normalizeHost(h); ok {
			passthrough[h] = true
		}
	}
	inject := make(map[string]InjectRule, len(p.Inject))
	for _, r := range p.Inject {
		if h, ok := normalizeHost(r.Host); ok {
			inject[h] = r
		}
	}
	return &handler{
		mode: p.Mode, allow: allow, transport: rt, dial: dial,
		mitm: p.MITM, passthrough: passthrough, inject: inject, ca: ca, refresh: refresh,
		obs: newObserver(p),
	}
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

	// MITM state (add-tls-mitm-credential-injection.md). mitm==false or ca==nil means the
	// CONNECT path never leaves the plain tunnel below, unconditionally — mitm:false runs are
	// byte-identical to before this task.
	mitm        bool
	passthrough map[string]bool       // hosts tunneled (never MITM'd) even when mitm is on
	inject      map[string]InjectRule // by normalizeHost'd host
	ca          *CA
	refresh     RefreshFunc

	// obs is the request-observation log's configuration, or nil when it is off (observe.go).
	obs *observer
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// ONE normalization, here, before any policy decision — and the result is what every step
	// below (allowed, the passthrough/inject lookups, and both dial paths) sees. A request host
	// that is not already ASCII LDH is refused outright: see normalizeHost for why.
	raw := requestHost(r)
	host, ok := normalizeHost(raw)
	if !ok {
		// %q because this is guest-controlled text and, by construction, is non-ASCII or
		// otherwise unprintable — the whole point of the refusal.
		log.Printf("krayt-egress-proxy: %s %q: refused, host is not an ASCII hostname", r.Method, raw)
		http.Error(w, "krayt: egress to a non-ASCII or malformed host is refused", http.StatusForbidden)
		return
	}
	if !h.allowed(host) {
		// Logged unconditionally, not just under logRequests: on a CONNECT the guest's client
		// discards this 403's body (see tunnel's comment below), so without this line a
		// policy denial — the single most likely reason a run fails — leaves NO trace anywhere,
		// which is the exact gap proxy.log exists to close (§6.6, §9). %q because a blocked host
		// is guest-supplied text.
		log.Printf("krayt-egress-proxy: %s %q: blocked by the network policy (mode=%s)", r.Method, host, h.mode)
		http.Error(w, "krayt: egress to "+host+" is blocked by the network policy", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodConnect {
		h.connect(w, r, host)
		return
	}
	// Injection targets HTTPS only (§1, §4.5): the MITM path is the only place a credential is
	// attached, and a plain-HTTP request never goes through it. Refuse outright for a host with
	// an injection rule rather than silently forwarding it unauthenticated or attaching a
	// credential to a cleartext request.
	if _, ok := h.inject[host]; ok {
		http.Error(w, "krayt: "+host+" requires HTTPS (credential injection configured); plain HTTP is refused", http.StatusBadRequest)
		return
	}
	h.forward(w, r, host)
}

// allowed applies the policy to a bare host (no port). ServeHTTP has already put its argument
// through normalizeHost — the enforcement point still folds it again with foldHostASCII, the
// SAME fold, so the lookup is total rather than merely correct-by-caller-discipline: an unfolded
// or non-ASCII string handed to it by any future caller is answered on the one definition of "the
// same hostname" this package has, not silently missed as a map key.
//
// Re-folding is a no-op on a normalized host, so this changes no decision the proxy makes today;
// it only removes the assumption. foldHostASCII, not normalizeHost, because the enforcement point
// must not be the place that tolerates surrounding whitespace: trimming belongs at ingest (see
// normalizeHost), and a host that reaches here still carrying spaces is a caller bug, not a name
// to be cleaned up and approved.
func (h *handler) allowed(host string) bool {
	switch h.mode {
	case ModeFull:
		return true
	case ModeNone:
		return false
	default: // allowlist
		key, ok := foldHostASCII(host)
		return ok && h.allow[key]
	}
}

// connect dispatches an (already allowed) CONNECT to the MITM path or the plain tunnel (§4.2).
// A passthrough host, or MITM being off entirely, always gets the tunnel — pinned clients and
// non-HTTP-over-TLS (git+ssh on 443) survive only because this fallback exists, so it must stay
// reachable no matter what the MITM path does.
// host is the normalizeHost'd bare host ServeHTTP approved; authority re-attaches the request's
// port to it, so the tunnel dials (and the MITM path certifies) the exact bytes the policy saw.
func (h *handler) connect(w http.ResponseWriter, r *http.Request, host string) {
	authority := normalizedAuthority(r, host)
	if !h.mitm || h.ca == nil || h.passthrough[host] {
		h.obs.connect(authority, false)
		h.tunnel(w, r, host, authority)
		return
	}
	h.obs.connect(authority, true)
	h.connectMITM(w, r, host, authority)
}

// tunnel is the original CONNECT behavior, preserved verbatim (§4.2): dial the (already
// allowed) target and splice bytes both ways with no inspection. This is the fallback when
// anything about MITM is inapplicable or misbehaves, so it must never be modified to depend on
// MITM state.
func (h *handler) tunnel(w http.ResponseWriter, r *http.Request, host, authority string) {
	upstream, err := h.dial(r.Context(), "tcp", authority)
	if err != nil {
		if errors.Is(err, errBlockedAddr) {
			http.Error(w, blockedAddrMsg(host), http.StatusForbidden)
			return
		}
		// net/http's CONNECT-proxy client path (what every proxy-aware caller, including
		// hack/netprobe, uses) discards the response body on a non-2xx CONNECT reply — the
		// caller only ever sees the status text ("Bad Gateway"), never this message. Log it
		// server-side so the real reason (DNS failure, connection refused, timeout, …) is
		// visible in proxy.log (§6.6, §9 of move-egress-proxy-to-host.md), the only place a
		// denial reason survives.
		// %q on the authority because it is guest-controlled text (see ServeHTTP). The %v error
		// embeds that same already-validated authority and nothing else guest-derived, so it needs
		// no quoting of its own.
		log.Printf("krayt-egress-proxy: CONNECT %q: upstream dial failed: %v", authority, err)
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

// forward proxies a plain-HTTP request to the (already allowed) target. host is the
// normalizeHost'd bare host ServeHTTP approved.
func (h *handler) forward(w http.ResponseWriter, r *http.Request, host string) {
	h.obs.request("http", host, r, false) // before RequestURI is cleared, while r still carries the guest's request line
	r.RequestURI = ""                     // must be cleared before re-sending as a client request
	// Re-point the outbound request at the approved bytes. The transport resolves r.URL.Host,
	// not the string the allowlist checked, so leaving the guest's spelling here is what let the
	// two diverge; overwriting it makes "approved" and "dialed" the same string by construction.
	authority := normalizedAuthority(r, host)
	r.URL.Host = authority
	r.Host = authority
	// RFC 7230 §6.1 cleanup, in BOTH directions. The MITM path gets this for free from
	// httputil.ReverseProxy; this path is hand-rolled, so it must do it itself. The one header
	// that matters beyond correctness is Proxy-Authorization: it is a credential for THIS hop, and
	// forwarding it would leak a guest's proxy credentials to every allowlisted host it talks to.
	stripHopByHop(r.Header)
	resp, err := h.transport.RoundTrip(r)
	if err != nil {
		if errors.Is(err, errBlockedAddr) {
			http.Error(w, blockedAddrMsg(host), http.StatusForbidden)
			return
		}
		// %q on the host because it is guest-controlled text (see ServeHTTP). The %v error is
		// safe unquoted for the same reason the quoting exists: the only guest-derived string it
		// can embed is this same already-validated host.
		log.Printf("krayt-egress-proxy: %s %q: upstream request failed: %v", r.Method, host, err)
		http.Error(w, "krayt: upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	h.obs.response("http", host, resp)
	stripHopByHop(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// hopByHopHeaders is the fixed RFC 7230 §6.1 set: headers that are meaningful for one
// transport-level hop only and must never be forwarded to the next one. Proxy-Authorization and
// Proxy-Authenticate are credentials/challenges for the proxy hop itself; the rest describe this
// connection's framing, which the next connection redefines for itself.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection", // non-standard, but universally sent and universally stripped
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// stripHopByHop deletes every hop-by-hop header from h: first the ones the Connection field
// itself names (RFC 7230 §6.1 lets a hop declare additional per-connection headers), then the
// fixed set above. Order matters — Connection must be read before it is deleted.
//
// Header.Del canonicalizes its argument, so "TE"/"te"/"Te" and a Connection token in any case all
// hit the same map key.
func stripHopByHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
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

// normalizeHost folds a request host (or a policy entry) into the single canonical form this
// package makes every decision on, and reports whether it is usable at all.
//
// It exists because two definitions of "the same hostname" used to sit on either side of the
// policy decision: allowed() folded with strings.ToLower (Unicode simple case folding, under
// which U+0130 'İ' becomes ASCII 'i'), while every http.Transport dial folds with UTS-46
// (under which U+0130 becomes the punycode label "xn--..."). For an allowlist entry
// "api.example.com" a guest could therefore send `http://api.examplİ.com/` — reachable through a
// proxy at all because net/url percent-decodes the host — have the allowlist approve it as the
// listed host, and have the transport dial the attacker's registered "xn--" domain instead: a
// complete egress-allowlist bypass in the default configuration.
// The same primitive selected an inject rule (and therefore a real credential) for a host the
// rule was never written for.
//
// The fix is to refuse rather than to translate: krayt's allow/inject entries are ASCII by
// construction, so an IDN request host can only ever be a mismatch, and refusing it needs no
// IDNA library (adding one would need a spec change, CLAUDE.md / §9.1). Folding is therefore
// byte-wise ASCII and NEVER strings.ToLower — no Unicode rune may fold into an ASCII letter
// here. Anything outside [a-z0-9.-], plus ':' so IPv6 literals survive, is refused; so is the
// empty string.
//
// Brackets are URL *authority* syntax, not part of the address: "[2001:db8::1]" and
// "2001:db8::1" name the same target, so the canonical form is the bare one and a bracketed
// input is unwrapped here. Otherwise the same literal took two different keys depending on how
// it arrived — a CONNECT authority always carries a port, so net.SplitHostPort strips its
// brackets in requestHost, while a plain "http://[2001:db8::1]/" has no port to split and kept
// them — and one spelling matched an allowlist entry while the other did not. Brackets are
// re-attached only where an authority is built, in normalizedAuthority.
//
// internal/task's lower()/validateHostEntry (network.go) are the pre-flight-validation
// counterpart and are deliberately ASCII-only for the same reason. The two must stay in
// agreement: if one starts accepting or mapping a byte the other does not, config validation and
// the running proxy no longer agree on what host a rule names.
func normalizeHost(host string) (string, bool) {
	return foldHostASCII(strings.TrimSpace(host))
}

// foldHostASCII is normalizeHost's fold with no whitespace trimming: the equality half of
// "the same hostname", separated from the ingest half so both callers use one definition of the
// former. Trimming exists only because a policy entry is hand-written config (internal/task's
// pre-flight validation trims too — hostagreement_test.go pins that agreement); it is not part of
// what makes two host strings the same name, and the enforcement point (handler.allowed) uses this
// stricter function for that reason.
func foldHostASCII(host string) (string, bool) {
	if host == "" {
		return "", false
	}
	if inner, ok := strings.CutPrefix(host, "["); ok {
		// A bracketed host is an IPv6 literal or it is malformed — there is no third case
		// (RFC 3986 §3.2.2), so anything else is refused rather than folded into a key that
		// could never match. Is4 excludes "[1.2.3.4]", which is not legal bracketed syntax.
		inner, ok := strings.CutSuffix(inner, "]")
		if !ok {
			return "", false
		}
		if addr, err := netip.ParseAddr(inner); err != nil || addr.Is4() {
			return "", false
		}
		host = inner
	}
	b := []byte(host)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z':
			b[i] = c + ('a' - 'A')
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '-',
			c == ':': // ':': IPv6 literals (unbracketed by the time we get here)
		default:
			return "", false
		}
	}
	return string(b), true
}

// normalizedAuthority rebuilds r's authority from an already-normalized host, keeping the port
// the request asked for. This is what makes the string dialed byte-identical to the string the
// policy approved, rather than merely equivalent under some folding.
//
// This is the one place brackets go back on: normalizeHost keeps IPv6 in its bare canonical
// form, but an authority (a URL host, a dial address) needs them so the colons are not read as a
// port separator. JoinHostPort already does it for the with-port case.
func normalizedAuthority(r *http.Request, host string) string {
	raw := r.Host
	if r.Method != http.MethodConnect && r.URL != nil && r.URL.Host != "" {
		raw = r.URL.Host
	}
	if _, port, err := net.SplitHostPort(raw); err == nil {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}
