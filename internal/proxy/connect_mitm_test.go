package proxy

// Tests for the MITM CONNECT path (add-tls-mitm-credential-injection.md §4). All offline,
// httptest-based, no VM and no real internet — the "upstream" is always either a fake
// http.RoundTripper (captures/echoes the request the ReverseProxy built, no real network) or,
// where the test specifically needs a REAL TLS session to inspect (the passthrough case), a
// loopback httptest.NewTLSServer.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// roundTripFunc adapts a plain function to http.RoundTripper, for refreshingTransport unit
// tests that don't need the full MITM harness.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// echoTransport is a fake upstream: it records the last request it received (method, URL, and a
// deep copy of headers, since ReverseProxy/the caller may mutate/close the original afterward)
// and returns a canned response, so injection/rewrite tests need no real network.
type echoTransport struct {
	mu   sync.Mutex
	last *http.Request

	status int
	body   string
	header http.Header

	// bodyFn, if set, overrides body/status entirely — used by the streaming tests to hand back
	// a live, incrementally-written body instead of a static string.
	bodyFn func(w *io.PipeWriter)
}

func (e *echoTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	e.mu.Lock()
	clone := r.Clone(r.Context())
	e.last = clone
	e.mu.Unlock()

	status := e.status
	if status == 0 {
		status = http.StatusOK
	}
	hdr := http.Header{}
	for k, vs := range e.header {
		hdr[k] = append([]string(nil), vs...)
	}
	if hdr.Get("Content-Type") == "" && e.bodyFn == nil {
		hdr.Set("Content-Type", "text/plain")
	}
	// Request is set for the same reason net/http's own transport sets it (persistConn.readResponse):
	// it is the request that OBTAINED this response — here the post-Rewrite outbound one — which is
	// what lets ReverseProxy's ModifyResponse hook see what actually went upstream (observe.go's
	// `sent=` fragment). A fake that left it nil would hide a real production behavior.
	if e.bodyFn != nil {
		pr, pw := io.Pipe()
		go e.bodyFn(pw)
		return &http.Response{StatusCode: status, Header: hdr, Body: pr, Request: r, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}, nil
	}
	return &http.Response{
		StatusCode: status, Header: hdr, Request: r, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Body: io.NopCloser(strings.NewReader(e.body)),
	}, nil
}

func (e *echoTransport) lastRequest() *http.Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.last
}

// mustLastRequest fails the test if the upstream never received a request, otherwise returns it.
// Isolating the nil check here (rather than inline at each call site) keeps staticcheck's SA5011
// from misreading the surrounding multi-statement test bodies as a possible nil dereference — see
// https://github.com/dominikh/go-tools/issues/656 (a known false positive around t.Fatal).
func mustLastRequest(t *testing.T, upstream *echoTransport) *http.Request {
	t.Helper()
	req := upstream.lastRequest()
	if req == nil {
		t.Fatal("upstream never received a request")
	}
	return req
}

// blockedDialTransport simulates what h.transport does when the SSRF guard's Control hook
// refuses the resolved address — mirrors the existing blockingTransport used for the
// tunnel/forward paths (proxy_internal_test.go), applied to the MITM upstream dial.
type blockedDialTransport struct{ dialed *bool }

func (b *blockedDialTransport) RoundTrip(*http.Request) (*http.Response, error) {
	if b.dialed != nil {
		*b.dialed = true
	}
	return nil, &net.OpError{Op: "dial", Err: checkDialAddr("127.0.0.1:443")}
}

// mitmHarness wires a real listening HTTP server around a MITM-capable handler and a real
// http.Client that speaks CONNECT + TLS through it — the same machinery any proxy-aware HTTPS
// client uses — so tests exercise the genuine hijack/handshake/ReverseProxy path, not a mock of
// it. Only the upstream (rt) is faked; the CA/TLS handshake and header rewriting are real.
type mitmHarness struct {
	t      *testing.T
	ca     *CA
	srv    *httptest.Server
	client *http.Client
	pool   *x509.CertPool
}

func newMITMHarness(t *testing.T, pol Policy, rt http.RoundTripper, dial dialFunc) *mitmHarness {
	t.Helper()
	ca, err := newCA("test-run")
	if err != nil {
		t.Fatalf("newCA: %v", err)
	}
	pol.MITM = true
	h := newHandler(pol, rt, dial, ca, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	proxyURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}
	return &mitmHarness{t: t, ca: ca, srv: srv, client: client, pool: pool}
}

func TestMITMInjectionReplacesAndStrips(t *testing.T) {
	upstream := &echoTransport{}
	rule := InjectRule{
		Host:  "api.example.com",
		Strip: []string{"x-api-key", "authorization"},
		Set:   map[string]string{"x-api-key": "resolved-secret-value"},
	}
	h := newMITMHarness(t, Policy{Mode: ModeAllowlist, Allow: []string{"api.example.com"}, Inject: []InjectRule{rule}}, upstream, nil)

	req, _ := http.NewRequest(http.MethodGet, "https://api.example.com/v1/messages", nil)
	req.Header.Set("X-Api-Key", "guest-supplied-value")
	req.Header.Set("Authorization", "Bearer guest-token")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request through MITM proxy: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	upstreamReq := mustLastRequest(t, upstream)
	if got := upstreamReq.Header.Values("X-Api-Key"); len(got) != 1 || got[0] != "resolved-secret-value" {
		t.Errorf("upstream X-Api-Key = %v, want exactly [resolved-secret-value] (replaced, not appended)", got)
	}
	if got := upstreamReq.Header.Get("Authorization"); got != "" {
		t.Errorf("upstream Authorization = %q, want stripped entirely", got)
	}
	if upstreamReq.Host != "api.example.com:443" {
		t.Errorf("upstream Host = %q, want api.example.com:443 (the CONNECT authority)", upstreamReq.Host)
	}
}

func TestMITMNonMatchingHostGetsNoInjection(t *testing.T) {
	upstream := &echoTransport{}
	rule := InjectRule{Host: "api.example.com", Set: map[string]string{"x-api-key": "resolved-secret-value"}}
	h := newMITMHarness(t, Policy{Mode: ModeAllowlist, Allow: []string{"api.example.com", "other.example.com"}, Inject: []InjectRule{rule}}, upstream, nil)

	req, _ := http.NewRequest(http.MethodGet, "https://other.example.com/", nil)
	req.Header.Set("X-Api-Key", "guest-value-unchanged")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	upstreamReq := mustLastRequest(t, upstream)
	if got := upstreamReq.Header.Get("X-Api-Key"); got != "guest-value-unchanged" {
		t.Errorf("X-Api-Key = %q, want untouched guest value (no matching inject rule)", got)
	}
}

// TestMITMInjectRuleNotSelectedForLookalikeHost is the permanent regression for the credential
// half of the Unicode-folding bug: the rule lookup was h.inject[strings.ToLower(sni)], which
// folds U+0130 onto ASCII 'i', so a CONNECT to lookalikeHost selected the rule written for
// api.anthropic.com — Rewrite would have attached the real credential and SetURL would have sent
// it to a domain the attacker registered.
//
// It did not fire before only by accident: x509.CreateCertificate refuses the non-ASCII DNSName
// generateLeaf puts in the leaf, so the handshake died first. That accident is not the check any
// more — the host is refused at the handler's choke point, and validateConnectAuthority refuses
// it again deliberately. Both are asserted here, because the first one passing is what makes the
// second look unnecessary.
func TestMITMInjectRuleNotSelectedForLookalikeHost(t *testing.T) {
	upstream := &echoTransport{}
	rule := InjectRule{Host: "api.anthropic.com", Set: map[string]string{"x-api-key": "real-secret-value"}}
	dialed := ""
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = addr
		return nil, fmt.Errorf("dial must not be reached")
	}
	pol := Policy{Mode: ModeAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true, Inject: []InjectRule{rule}}
	h := newHandler(pol, upstream, dial, mustCA(t), nil)

	req := httptest.NewRequest(http.MethodConnect, "//"+lookalikeHost+":443", nil)
	req.Host = lookalikeHost + ":443"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if dialed != "" {
		t.Errorf("dialed %q for a host with no rule and no allowlist entry", dialed)
	}
	if got := upstream.lastRequest(); got != nil {
		t.Errorf("upstream reached with %v", got.URL)
	}
	// The rule map itself cannot be reached with an unfolded key: there is no entry under any
	// spelling but the normalized one.
	if _, hasRule := h.inject[lookalikeHost]; hasRule {
		t.Error("inject rule is reachable under the lookalike spelling")
	}
	// And the deliberate check that replaced the x509 encoder's accidental one.
	if err := validateConnectAuthority(lookalikeHost + ":443"); err == nil {
		t.Error("validateConnectAuthority accepted a non-ASCII authority")
	}
}

func TestMITMSetLiteral(t *testing.T) {
	upstream := &echoTransport{}
	rule := InjectRule{Host: "api.example.com", SetLiteral: map[string]string{"x-krayt-mitm": "1"}}
	h := newMITMHarness(t, Policy{Mode: ModeAllowlist, Allow: []string{"api.example.com"}, Inject: []InjectRule{rule}}, upstream, nil)

	resp, err := h.client.Get("https://api.example.com/")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()
	if got := upstream.lastRequest().Header.Get("X-Krayt-Mitm"); got != "1" {
		t.Errorf("literal header = %q, want 1", got)
	}
}

// TestMITMFullModeInterceptsUnlistedHost proves mode: full + mitm intercepts a host in no
// allowlist at all, and that injection still only fires for explicitly named hosts.
func TestMITMFullModeInterceptsUnlistedHost(t *testing.T) {
	upstream := &echoTransport{}
	rule := InjectRule{Host: "api.example.com", Set: map[string]string{"x-api-key": "resolved-secret-value"}}
	h := newMITMHarness(t, Policy{Mode: ModeFull, Inject: []InjectRule{rule}}, upstream, nil)

	resp, err := h.client.Get("https://totally-unlisted.example.net/anything")
	if err != nil {
		t.Fatalf("request to an unlisted host under mode:full: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (mode:full intercepts and forwards)", resp.StatusCode)
	}
	if got := upstream.lastRequest().Header.Get("X-Api-Key"); got != "" {
		t.Errorf("X-Api-Key = %q, want empty — this host has no inject rule", got)
	}
}

// TestMITMPassthroughTunnelsUnmodified proves a passthrough host is tunneled byte-for-byte: the
// upstream sees the CLIENT's own original TLS session (its own leaf cert, not krayt's ephemeral
// CA), and no header is injected — asserted via a real TLS-terminating server and cert
// introspection, since a tunneled connection is by definition opaque to the proxy.
func TestMITMPassthroughTunnelsUnmodified(t *testing.T) {
	var gotHeader string
	upstreamTLS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstreamTLS.Close()
	upstreamAddr := upstreamTLS.Listener.Addr().String()

	rule := InjectRule{Host: "pinned.example.com", Set: map[string]string{"x-api-key": "resolved-secret-value"}}
	dial := func(_ context.Context, _, _ string) (net.Conn, error) {
		// Stand in for the real dialer: redirect whatever CONNECT authority was requested to the
		// real TLS upstream, so the client's TLS ClientHello reaches it directly (untouched) —
		// exactly what a real tunnel does, just without needing DNS for "pinned.example.com".
		return net.Dial("tcp", upstreamAddr)
	}
	h := newMITMHarness(t, Policy{
		Mode: ModeAllowlist, Allow: []string{"pinned.example.com"}, Passthrough: []string{"pinned.example.com"},
		Inject: []InjectRule{rule},
	}, &echoTransport{}, dial)

	// The client must trust the REAL upstream's cert here, not krayt's ephemeral CA — proving
	// the tunnel is not intercepted. Use the upstream server's own certificate.
	realPool := x509.NewCertPool()
	realPool.AddCert(upstreamTLS.Certificate())
	proxyURL, _ := url.Parse(h.srv.URL)
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: realPool},
		},
	}
	req, _ := http.NewRequest(http.MethodGet, "https://pinned.example.com/", nil)
	req.Header.Set("X-Api-Key", "guest-value-should-survive")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request through passthrough tunnel: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if gotHeader != "guest-value-should-survive" {
		t.Errorf("upstream saw X-Api-Key = %q, want the guest's original value (passthrough = no injection)", gotHeader)
	}
}

// TestMITMSSRFGuardStillApplies proves a MITM'd host whose resolved address the shared
// checkDialAddr guard would refuse is blocked with 403 and never actually dialed — the
// ReverseProxy's Transport is the SAME guarded transport the tunnel/forward paths use.
func TestMITMSSRFGuardStillApplies(t *testing.T) {
	dialed := false
	upstream := &blockedDialTransport{dialed: &dialed}
	h := newMITMHarness(t, Policy{Mode: ModeAllowlist, Allow: []string{"rebind.example.com"}}, upstream, nil)

	resp, err := h.client.Get("https://rebind.example.com/")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "blocked address range") {
		t.Errorf("body = %q, want the SSRF-guard message", b)
	}
}

// TestMITMStreamingFlushesIncrementally proves an SSE-shaped upstream streams through with
// per-chunk delivery, not full-response buffering — the whole point of FlushInterval: -1.
func TestMITMStreamingFlushesIncrementally(t *testing.T) {
	release := make(chan struct{})
	upstream := &echoTransport{
		header: http.Header{"Content-Type": []string{"text/event-stream"}},
		bodyFn: func(w *io.PipeWriter) {
			defer func() { _ = w.Close() }()
			_, _ = w.Write([]byte("data: first\n\n"))
			<-release // held open until the test has observed the first chunk
			_, _ = w.Write([]byte("data: second\n\n"))
		},
	}
	h := newMITMHarness(t, Policy{Mode: ModeAllowlist, Allow: []string{"stream.example.com"}}, upstream, nil)

	resp, err := h.client.Get("https://stream.example.com/events")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	r := bufio.NewReader(resp.Body)
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read first SSE line: %v", err)
	}
	if !strings.Contains(line, "first") {
		t.Fatalf("first chunk = %q, want it to contain 'first'", line)
	}
	if _, err := r.ReadString('\n'); err != nil { // the event's trailing blank line
		t.Fatalf("read first SSE blank line: %v", err)
	}
	// The first chunk arrived even though the upstream handler is still blocked on `release` —
	// proof the proxy flushed it immediately rather than buffering until the whole body closed.
	close(release)
	line2, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read second SSE line: %v", err)
	}
	if !strings.Contains(line2, "second") {
		t.Errorf("second chunk = %q, want it to contain 'second'", line2)
	}
}

// TestMITMChunkedNDJSONStreams is the streaming test's chunked-NDJSON counterpart (§ tests):
// FlushInterval: -1 also flushes a plain (non-SSE) content type per write.
func TestMITMChunkedNDJSONStreams(t *testing.T) {
	release := make(chan struct{})
	upstream := &echoTransport{
		header: http.Header{"Content-Type": []string{"application/x-ndjson"}},
		bodyFn: func(w *io.PipeWriter) {
			defer func() { _ = w.Close() }()
			_, _ = w.Write([]byte(`{"n":1}` + "\n"))
			<-release
			_, _ = w.Write([]byte(`{"n":2}` + "\n"))
		},
	}
	h := newMITMHarness(t, Policy{Mode: ModeAllowlist, Allow: []string{"ndjson.example.com"}}, upstream, nil)

	resp, err := h.client.Get("https://ndjson.example.com/stream")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	r := bufio.NewReader(resp.Body)
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read first line: %v", err)
	}
	if !strings.Contains(line, `"n":1`) {
		t.Fatalf("first line = %q", line)
	}
	close(release)
	line2, err := r.ReadString('\n')
	if err != nil || !strings.Contains(line2, `"n":2`) {
		t.Fatalf("second line = %q, err=%v", line2, err)
	}
}

// TestMITMFalseByteIdenticalToTunnel proves mitm:false never leaves the plain tunnel — a
// listener with no CA at all behaves identically to before this task (connect() dispatch,
// verified structurally: connectMITM is simply unreachable when ca==nil).
func TestMITMFalseByteIdenticalToTunnel(t *testing.T) {
	pol := Policy{Mode: ModeAllowlist, Allow: []string{"api.example.com"}} // MITM left false
	dialed := false
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = true
		return nil, fmt.Errorf("dial refused in test: %s", addr)
	}
	h := newHandler(pol, http.DefaultTransport, dial, nil, nil) // ca == nil
	req := httptest.NewRequest(http.MethodConnect, "//api.example.com:443", nil)
	req.Host = "api.example.com:443"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !dialed {
		t.Error("expected the plain tunnel dial path to run when MITM is off")
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (tunnel dial failure), got body %q", rec.Code, rec.Body.String())
	}
}

// --- Hostile-input tests (§6): these need raw control over the inner HTTP/1.1 bytes sent after
// the TLS handshake, which net/http.Client's normal API can't produce (it always sends a Host
// matching the URL it dialed) — so they speak the wire protocol directly.

// rawMITMConn performs a real CONNECT + TLS handshake against a MITM harness and returns the
// established TLS connection, positioned to write a raw inner HTTP/1.1 request.
func rawMITMConn(t *testing.T, h *mitmHarness, authority, sni string) *tls.Conn {
	t.Helper()
	proxyURL, err := url.Parse(h.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(raw)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT response = %q, want 200", status)
	}
	for { // drain the rest of the CONNECT response headers (just the blank line, in practice)
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read CONNECT headers: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	tlsConn := tls.Client(raw, &tls.Config{RootCAs: h.pool, ServerName: sni})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake: %v", err)
	}
	return tlsConn
}

func TestMITMInnerHostMismatchRejected(t *testing.T) {
	upstream := &echoTransport{}
	h := newMITMHarness(t, Policy{Mode: ModeAllowlist, Allow: []string{"api.example.com"}}, upstream, nil)
	conn := rawMITMConn(t, h, "api.example.com:443", "api.example.com")

	req := "GET / HTTP/1.1\r\nHost: evil-smuggled.example.com\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write inner request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read inner response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (Host/authority mismatch)", resp.StatusCode)
	}
	if upstream.lastRequest() != nil {
		t.Error("a Host-mismatched request must never reach upstream")
	}
}

// TestHostsEquivalentUsesTheOneHostFold pins the inner-Host guard to normalizeHost. It used to
// compare with strings.EqualFold, a UNICODE fold under which U+212A KELVIN SIGN equals 'K' — so
// the guard accepted a spelling every other decision in this package refuses. Nothing was
// exploitable through it (the upstream target is the CONNECT authority regardless of the inner
// Host), but it was the package's last rune-folds-onto-ASCII comparison, which is the primitive
// the allowlist bypass was built out of.
func TestHostsEquivalentUsesTheOneHostFold(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"api.example.com", "api.example.com:443", true},   // port present on one side only (RFC 7230)
		{"API.Example.COM:443", "api.example.com", true},   // ASCII case is the same name
		{"evil.example.com:443", "api.example.com", false}, // the smuggled-Host case
		{"key.example.com", "Key.example.com", false},      // KELVIN SIGN is not 'K' here
		{"api.anthropic.com", lookalikeHost, false},        // nor is U+0130 an 'i'
		{"", "", false}, // an unusable host matches nothing, itself included
		{"Key.example.com", "Key.example.com", false},
	}
	for _, tc := range cases {
		if got := hostsEquivalent(tc.a, tc.b); got != tc.want {
			t.Errorf("hostsEquivalent(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestMITMSmuggledDuplicateInjectedHeaderStripped(t *testing.T) {
	upstream := &echoTransport{}
	rule := InjectRule{Host: "api.example.com", Strip: []string{"x-api-key"}, Set: map[string]string{"x-api-key": "resolved-secret-value"}}
	h := newMITMHarness(t, Policy{Mode: ModeAllowlist, Allow: []string{"api.example.com"}, Inject: []InjectRule{rule}}, upstream, nil)
	conn := rawMITMConn(t, h, "api.example.com:443", "api.example.com")

	req := "GET / HTTP/1.1\r\nHost: api.example.com\r\nX-Api-Key: evil-one\r\nX-Api-Key: evil-two\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write inner request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read inner response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := upstream.lastRequest().Header.Values("X-Api-Key")
	if len(got) != 1 || got[0] != "resolved-secret-value" {
		t.Errorf("upstream X-Api-Key = %v, want exactly one resolved value (smuggled duplicates stripped)", got)
	}
}

// TestMITMConnectionHeaderCannotStripInjectedCredential proves a guest cannot use a
// `Connection: X-Api-Key` header to make ReverseProxy's own hop-by-hop cleanup (RFC 7230 §6.1)
// delete the credential injectingHandler/Rewrite just attached, which would otherwise forward
// the request upstream unauthenticated.
func TestMITMConnectionHeaderCannotStripInjectedCredential(t *testing.T) {
	upstream := &echoTransport{}
	rule := InjectRule{Host: "api.example.com", Strip: []string{"x-api-key"}, Set: map[string]string{"x-api-key": "resolved-secret-value"}}
	h := newMITMHarness(t, Policy{Mode: ModeAllowlist, Allow: []string{"api.example.com"}, Inject: []InjectRule{rule}}, upstream, nil)
	conn := rawMITMConn(t, h, "api.example.com:443", "api.example.com")

	req := "GET / HTTP/1.1\r\nHost: api.example.com\r\nConnection: X-Api-Key\r\nX-Api-Key: evil\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write inner request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read inner response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	upstreamReq := mustLastRequest(t, upstream)
	if got := upstreamReq.Header.Get("X-Api-Key"); got != "resolved-secret-value" {
		t.Errorf("upstream X-Api-Key = %q, want resolved-secret-value (a smuggled Connection header must not strip the injected credential)", got)
	}
}

func TestMITMOversizedHeaderRejected(t *testing.T) {
	upstream := &echoTransport{}
	h := newMITMHarness(t, Policy{Mode: ModeAllowlist, Allow: []string{"api.example.com"}}, upstream, nil)
	conn := rawMITMConn(t, h, "api.example.com:443", "api.example.com")

	big := strings.Repeat("a", maxMITMHeaderBytes*3)
	req := "GET / HTTP/1.1\r\nHost: api.example.com\r\nX-Big: " + big + "\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		// A write failure (reset by peer mid-write) is an acceptable way for this to surface too.
		return
	}
	// A connection dropped without a well-formed response is also an acceptable rejection shape
	// for an oversized header (net/http's server behavior here isn't a clean 4xx in every Go
	// version); the key property asserted below is that upstream never saw it either way.
	if resp, err := http.ReadResponse(bufio.NewReader(conn), nil); err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge && resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want a 4xx rejection for an oversized header", resp.StatusCode)
		}
	}
	if upstream.lastRequest() != nil {
		t.Error("an oversized-header request must never reach upstream")
	}
}

func TestMITMInvalidConnectAuthorityRejected(t *testing.T) {
	upstream := &echoTransport{}
	h := newMITMHarness(t, Policy{Mode: ModeFull}, upstream, nil)
	proxyURL, _ := url.Parse(h.srv.URL)
	raw, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := fmt.Fprintf(raw, "CONNECT not-a-valid-authority HTTP/1.1\r\nHost: not-a-valid-authority\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(raw), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a malformed CONNECT authority", resp.StatusCode)
	}
}

// TestPlainHTTPRefusedForInjectedHost proves injection targets HTTPS only (§1, §4.5): a
// plain-HTTP request to a host with an inject rule is refused outright, never forwarded
// unauthenticated and never given the credential over cleartext.
func TestPlainHTTPRefusedForInjectedHost(t *testing.T) {
	rule := InjectRule{Host: "api.example.com", Set: map[string]string{"x-api-key": "resolved-secret-value"}}
	h := newHandler(Policy{Mode: ModeAllowlist, Allow: []string{"api.example.com"}, MITM: true, Inject: []InjectRule{rule}},
		&echoTransport{}, nil, mustCA(t), nil)
	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (plain HTTP refused for an injected host)", rec.Code)
	}
}

func mustCA(t *testing.T) *CA {
	t.Helper()
	ca, err := newCA("test")
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// TestMITMEmptyInjectedValueFails500s proves an injected secret that resolves to an empty
// value at request time is treated as a programming/config error — 500, never sent
// unauthenticated (§7).
func TestMITMEmptyInjectedValueFails500(t *testing.T) {
	upstream := &echoTransport{}
	rule := InjectRule{Host: "api.example.com", Set: map[string]string{"x-api-key": ""}}
	h := newMITMHarness(t, Policy{Mode: ModeAllowlist, Allow: []string{"api.example.com"}, Inject: []InjectRule{rule}}, upstream, nil)

	resp, err := h.client.Get("https://api.example.com/")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	if upstream.lastRequest() != nil {
		t.Error("a request with an unresolved injected credential must never reach upstream")
	}
}

// TestMITMSetupFailureDoesNotFallBackToTunnel proves a TLS handshake failure on the MITM path
// fails the connection outright rather than silently degrading to a plain tunnel (§7) — a
// client that doesn't speak TLS at all after CONNECT never gets a working tunnel instead.
func TestMITMSetupFailureDoesNotFallBackToTunnel(t *testing.T) {
	upstream := &echoTransport{}
	h := newMITMHarness(t, Policy{Mode: ModeAllowlist, Allow: []string{"api.example.com"}}, upstream, nil)
	proxyURL, _ := url.Parse(h.srv.URL)
	raw, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := fmt.Fprintf(raw, "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(raw)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		t.Fatalf("CONNECT response = %q, err=%v", status, err)
	}
	// Send plaintext instead of a TLS ClientHello — the handshake must fail, and the connection
	// must be closed, not silently spliced through as a tunnel.
	if _, err := raw.Write([]byte("GET / HTTP/1.1\r\nHost: api.example.com\r\n\r\n")); err != nil {
		return // a write error here is also an acceptable sign the connection was torn down
	}
	_ = raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, err := raw.Read(buf)
	if err == nil && n > 0 && strings.Contains(string(buf[:n]), "HTTP/1.1 200") {
		t.Errorf("got a plain-HTTP 200 response after a failed TLS handshake — must not fall back to a tunnel: %q", buf[:n])
	}
	if upstream.lastRequest() != nil {
		t.Error("upstream must never see a request from a failed MITM handshake")
	}
}

// --- refreshingTransport unit tests (§4.6): direct, not through the full MITM harness, since
// no production code wires a non-nil RefreshFunc yet (step 3's job) — these are the only tests
// exercising the mechanism itself.

func refreshableRule() InjectRule {
	return InjectRule{
		Host:    "api.example.com",
		Refresh: &RefreshRule{Host: "api.example.com", PathPrefix: "/oauth", ResponseTokenFields: []string{"access_token"}},
	}
}

// TestRefreshingTransportOversizedBodyNotTruncated proves an oversized request body reaches
// upstream COMPLETE on the first attempt — never silently truncated just because it's too large
// to safely buffer for a possible retry. Retry eligibility and body integrity are independent.
func TestRefreshingTransportOversizedBodyNotTruncated(t *testing.T) {
	var gotBody []byte
	refreshCalled := false
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		gotBody = b
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	rt := &refreshingTransport{
		base: base,
		refresh: func(context.Context, InjectRule) (map[string]string, error) {
			refreshCalled = true
			return map[string]string{"x-api-key": "new"}, nil
		},
		rule: refreshableRule(), hasRule: true,
	}

	big := bytes.Repeat([]byte("a"), refreshBodyCap+1024)
	req := httptest.NewRequest(http.MethodPost, "https://api.example.com/v1", bytes.NewReader(big))
	req.ContentLength = int64(len(big))

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()
	if len(gotBody) != len(big) || !bytes.Equal(gotBody, big) {
		t.Errorf("upstream received %d bytes (want %d), content match=%v — an oversized body must never be truncated",
			len(gotBody), len(big), bytes.Equal(gotBody, big))
	}
	if refreshCalled {
		t.Error("refresh must not fire when there was no 401")
	}
}

// TestRefreshingTransportRetriesOnceOn401 proves the generic mechanism: a 401 triggers exactly
// one refresh and one retry with the refreshed header value, for a body small enough to buffer.
func TestRefreshingTransportRetriesOnceOn401(t *testing.T) {
	attempt := 0
	var seenKeys []string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempt++
		seenKeys = append(seenKeys, r.Header.Get("X-Api-Key"))
		status := http.StatusUnauthorized
		if attempt > 1 {
			status = http.StatusOK
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	rt := &refreshingTransport{
		base: base,
		refresh: func(context.Context, InjectRule) (map[string]string, error) {
			return map[string]string{"x-api-key": "refreshed-value"}, nil
		},
		rule: refreshableRule(), hasRule: true,
	}
	req := httptest.NewRequest(http.MethodGet, "https://api.example.com/v1", nil)
	req.Header.Set("X-Api-Key", "stale-value")

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 after the retry", resp.StatusCode)
	}
	if attempt != 2 {
		t.Fatalf("upstream attempts = %d, want exactly 2 (one refresh, one retry)", attempt)
	}
	if seenKeys[0] != "stale-value" || seenKeys[1] != "refreshed-value" {
		t.Errorf("seen X-Api-Key values = %v, want [stale-value refreshed-value]", seenKeys)
	}
}

// TestRefreshingTransportNeverLoopsOnRepeated401 proves a second 401 after the retry is
// surfaced as-is — never a second refresh attempt.
func TestRefreshingTransportNeverLoopsOnRepeated401(t *testing.T) {
	attempt := 0
	refreshCalls := 0
	base := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		attempt++
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	rt := &refreshingTransport{
		base: base,
		refresh: func(context.Context, InjectRule) (map[string]string, error) {
			refreshCalls++
			return map[string]string{"x-api-key": "refreshed-value"}, nil
		},
		rule: refreshableRule(), hasRule: true,
	}
	req := httptest.NewRequest(http.MethodGet, "https://api.example.com/v1", nil)

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 surfaced as-is", resp.StatusCode)
	}
	if attempt != 2 {
		t.Errorf("upstream attempts = %d, want exactly 2 (never loop)", attempt)
	}
	if refreshCalls != 1 {
		t.Errorf("refresh calls = %d, want exactly 1", refreshCalls)
	}
}

// TestRefreshingTransportPassthroughWithoutRefreshFunc proves the zero-overhead no-op path:
// with no rule/RefreshFunc wired, a 401 is returned untouched and refresh is never consulted.
func TestRefreshingTransportPassthroughWithoutRefreshFunc(t *testing.T) {
	attempt := 0
	base := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		attempt++
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	rt := &refreshingTransport{base: base} // hasRule: false, refresh: nil — the production default today
	req := httptest.NewRequest(http.MethodGet, "https://api.example.com/v1", nil)

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 passed through untouched", resp.StatusCode)
	}
	if attempt != 1 {
		t.Errorf("upstream attempts = %d, want exactly 1 (no refresh capability wired)", attempt)
	}
}
