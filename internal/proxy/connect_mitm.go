package proxy

// The MITM CONNECT path (add-tls-mitm-credential-injection.md §4): terminate TLS on the host
// using the run's ephemeral CA, forward the decrypted request through the SAME guarded
// transport the plain-tunnel/plain-HTTP paths use (so checkDialAddr's SSRF guard is never
// bypassed), and apply header injection after Rewrite so a guest header can never smuggle a
// second value past the strip step.
//
// Lint-visible note (§6 "never log request or response bodies; headers may be logged name-only"):
// every log.Printf in this file names only the CONNECT authority (already approved by the
// allowlist, so not secret) and Go error values — never a header, a body, or any other
// guest-controlled byte. Keep it that way when touching this file.

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxMITMHeaderBytes bounds the inner (decrypted) request the guest can send through one MITM'd
// connection (§6 "bound the request").
const maxMITMHeaderBytes = 1 << 20 // 1 MiB

// refreshBodyCap bounds how large a request body connectMITM will buffer to support one retry
// after a host-side credential refresh (§4.6). A body larger than this skips the retry rather
// than risk unbounded memory use — the 401 is then surfaced as-is, same as if no refresh were
// configured at all.
const refreshBodyCap = 4 << 20 // 4 MiB

// connectMITM handles an (already allowlisted, already known non-passthrough) CONNECT by
// terminating TLS on the host and serving HTTP/1.1 over the decrypted connection (§4.3).
//
// Any setup failure here — a malformed authority, a hijack failure, a TLS handshake failure —
// fails the connection outright rather than falling back to a plain tunnel (§7): a silent
// fallback would drop injection and send the agent out unauthenticated, a confusing failure far
// from its cause.
func (h *handler) connectMITM(w http.ResponseWriter, r *http.Request) {
	authority := r.Host
	if err := validateConnectAuthority(authority); err != nil {
		http.Error(w, "krayt: "+err.Error(), http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "krayt: hijacking unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		_ = client.Close()
		return
	}

	sni := hostOnly(authority)
	tlsConn := tls.Server(client, h.ca.tlsConfigFor(sni))
	// Bound the handshake so a client that never speaks TLS after CONNECT can't hold this
	// goroutine (and the underlying fd) open forever.
	_ = tlsConn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		log.Printf("krayt-egress-proxy: MITM %s: TLS handshake failed: %v", authority, err)
		_ = tlsConn.Close()
		return
	}
	_ = tlsConn.SetDeadline(time.Time{}) // handshake bound only; request/response streaming is unbounded (SSE, §6)

	rule, hasRule := h.inject[strings.ToLower(sni)]

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// The upstream target is the CONNECT authority the allowlist already approved —
			// NEVER a guest-supplied Host header (§4.4, §6): a plain forward proxy's inner
			// request Host is attacker-controlled, and trusting it here would let a guest smuggle
			// a request to an unapproved host through an approved CONNECT tunnel.
			pr.SetURL(&url.URL{Scheme: "https", Host: authority})
			pr.Out.Host = authority
			// Strip/set MUST run here, after ReverseProxy's own hop-by-hop header cleanup (which
			// runs on pr.Out BEFORE Rewrite is called) — never earlier. Otherwise a guest-supplied
			// `Connection: X-Api-Key` would make ReverseProxy delete the credential we just
			// injected as a "hop-by-hop" header (RFC 7230 §6.1), forwarding the request upstream
			// unauthenticated. Stripping before setting is what makes the guest unable to
			// influence or smuggle a second credential (§4.5).
			if hasRule {
				for _, name := range rule.Strip {
					pr.Out.Header.Del(name)
				}
				for name, val := range rule.Set {
					pr.Out.Header.Set(name, val)
				}
				for name, val := range rule.SetLiteral {
					pr.Out.Header.Set(name, val)
				}
			}
		},
		Transport: &refreshingTransport{
			base: h.transport, refresh: h.refresh, rule: rule, hasRule: hasRule && rule.Refresh != nil,
		},
		FlushInterval: -1, // ReverseProxy only auto-flushes text/event-stream; NDJSON/long-poll need this too
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			log.Printf("krayt-egress-proxy: MITM %s %s: upstream request failed: %v", req.Method, authority, err)
			if errors.Is(err, errBlockedAddr) {
				http.Error(w, blockedAddrMsg(hostOnly(authority)), http.StatusForbidden)
				return
			}
			http.Error(w, "krayt: upstream request failed", http.StatusBadGateway)
		},
	}
	if h.obs != nil {
		// Status-and-header-names only (observe.go). ModifyResponse must return nil here: a
		// non-nil error would route the response into ErrorHandler and fail the guest's request,
		// which an observation hook must never be able to do.
		rp.ModifyResponse = func(resp *http.Response) error {
			h.obs.response("mitm", authority, resp)
			return nil
		}
	}

	inner := &http.Server{
		Handler:           injectingHandler(authority, rule, hasRule, h.obs, rp),
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    maxMITMHeaderBytes,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          log.Default(),
	}
	_ = inner.Serve(newOnceListener(tlsConn))
}

// injectingHandler wraps rp with the hostile-input guard (inner Host must agree with the
// CONNECT authority, §6) and a pre-flight check that every rule.Set value actually resolved
// (§7) — the strip/set mutation itself happens later, inside rp's Rewrite, after ReverseProxy's
// own hop-by-hop header cleanup (see the comment there for why the order matters).
//
// Every log.Printf in this function names a header/host by KEY or by the (already-approved)
// CONNECT authority only — never a header VALUE or the guest-supplied Host string, which is
// attacker-controlled text on the untrusted side of this boundary (§6 "never log bodies;
// headers may be logged name-only").
func injectingHandler(authority string, rule InjectRule, hasRule bool, obs *observer, rp http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Host != "" && !hostsEquivalent(req.Host, authority) {
			log.Printf("krayt-egress-proxy: MITM %s: inner request Host does not match the CONNECT authority", authority)
			http.Error(w, "krayt: request Host does not match the CONNECT authority", http.StatusBadRequest)
			return
		}
		// Logged here — as the GUEST sent it, before Rewrite's strip/set runs — because what the
		// client itself chose (host, path, auth header name) is precisely what a wire-format probe
		// is trying to learn. Before the pre-flight check below, too, so a request that fails to
		// resolve its credential is still observed. No-op when obs is nil.
		obs.request("mitm", authority, req, hasRule)
		if hasRule {
			for name, val := range rule.Set {
				if val == "" {
					// Pre-flight validation (task.ValidateNetworkPolicy) only confirms the
					// secrets-file KEY exists, not that its value is non-empty — an empty value
					// reaching here is a programming/config error, not a guest action. Fail
					// closed rather than send the request upstream unauthenticated (§7).
					log.Printf("krayt-egress-proxy: MITM %s: injected header %q resolved to an empty value", authority, name)
					http.Error(w, "krayt: injected credential unavailable", http.StatusInternalServerError)
					return
				}
			}
		}
		rp.ServeHTTP(w, req)
	})
}

// refreshingTransport wraps the shared, SSRF-guarded transport with the optional §4.6 "one
// refresh, one retry on 401" mechanism. With refresh==nil or hasRule==false (the default in this
// task — no adapter has registered a RefreshFunc yet) it is a zero-overhead passthrough.
type refreshingTransport struct {
	base    http.RoundTripper
	refresh RefreshFunc
	rule    InjectRule
	hasRule bool // rule has a non-nil Refresh block AND a RefreshFunc is wired
}

func (t *refreshingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.hasRule || t.refresh == nil {
		return t.base.RoundTrip(req)
	}

	// Only buffer (and thus become retry-capable) when the body's full size is already known
	// AND within the cap — this is the ONLY safe way to pre-read part of a body without risking
	// silently truncating what actually reaches upstream on the FIRST attempt. A body of unknown
	// length (chunked, ContentLength == -1) or one that exceeds the cap is left completely
	// untouched: it streams to upstream exactly as it would with no refresh rule at all, and this
	// request simply isn't eligible for a retry (a later 401 is then surfaced as-is, same as
	// having no refresh configured).
	var bodyCopy []byte
	canRetry := req.Body == nil || req.Body == http.NoBody
	if !canRetry && req.ContentLength >= 0 && req.ContentLength <= refreshBodyCap {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		bodyCopy = b
		req.Body = io.NopCloser(bytes.NewReader(bodyCopy))
		canRetry = true
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || !canRetry {
		return resp, err
	}

	fresh, rerr := t.refresh(req.Context(), t.rule)
	if rerr != nil || len(fresh) == 0 {
		return resp, err // no usable refresh — surface the 401 as-is (§4.6: never loop)
	}
	_ = resp.Body.Close()

	req2 := req.Clone(req.Context())
	if bodyCopy != nil {
		req2.Body = io.NopCloser(bytes.NewReader(bodyCopy))
	}
	for name, val := range fresh {
		req2.Header.Set(name, val)
	}
	return t.base.RoundTrip(req2) // exactly one retry; a second 401 here is returned as-is
}

// validateConnectAuthority rejects a CONNECT authority that is not a valid host:port (§6).
func validateConnectAuthority(authority string) error {
	h, p, err := net.SplitHostPort(authority)
	if err != nil {
		return fmt.Errorf("invalid CONNECT authority %q: %w", authority, err)
	}
	if h == "" {
		return fmt.Errorf("invalid CONNECT authority %q: empty host", authority)
	}
	if _, err := strconv.Atoi(p); err != nil {
		return fmt.Errorf("invalid CONNECT authority %q: invalid port %q", authority, p)
	}
	return nil
}

// hostsEquivalent compares two host[:port]/host strings by bare hostname only, case-insensitive
// — an inner request's Host header legitimately may (RFC 7230) or may not carry the port the
// CONNECT authority does.
func hostsEquivalent(a, b string) bool {
	return strings.EqualFold(hostOnly(a), hostOnly(b))
}

// hostOnly strips a trailing :port, if present.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// onceListener adapts a single already-established net.Conn (the just-handshaken TLS
// connection) into a net.Listener so http.Server's full request-handling machinery — keep-alive,
// pipelining, chunked transfer, timeouts — can serve HTTP/1.1 over it (§4.3). Accept yields the
// connection exactly once; every subsequent call returns io.EOF, which is what makes
// http.Server.Serve return once that one connection's lifecycle ends, instead of looping forever
// waiting for a second connection that will never come.
type onceListener struct {
	conn net.Conn
	mu   sync.Mutex
	used bool
}

func newOnceListener(c net.Conn) *onceListener { return &onceListener{conn: c} }

func (l *onceListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used {
		return nil, io.EOF
	}
	l.used = true
	return l.conn, nil
}

func (l *onceListener) Close() error   { return nil } // the conn's own lifecycle owns closing
func (l *onceListener) Addr() net.Addr { return l.conn.LocalAddr() }
