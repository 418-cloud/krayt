package proxy

// Tests for the opt-in request-observation log (observe.go). Same offline shape as
// connect_mitm_test.go: a real CONNECT+TLS client through a real handler, a fake upstream, and
// log output captured from the actual log.Printf calls the child process would write to
// proxy.log — never a stub logger, since what this file is really asserting is what ends up in
// that artifact.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// safeBuf collects log output written from the proxy's own goroutines (the inner MITM server runs
// on one), so the assertions below never race the writer.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog redirects the standard logger — the one every log.Printf in this package uses, and
// the one whose stdout/stderr the run supervisor persists as proxy.log — for the duration of a test.
func captureLog(t *testing.T) *safeBuf {
	t.Helper()
	buf := &safeBuf{}
	prevOut, prevFlags, prevPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})
	return buf
}

// pipedMITMClient is newMITMHarness's twin over an in-memory net.Pipe instead of a loopback
// listener: the handler, the ephemeral CA, the hijack, the TLS handshake and the ReverseProxy are
// all the real ones — only the socket is replaced. The package's own onceListener (connect_mitm.go)
// does the same one-connection-as-a-listener trick, so this needs no new machinery.
//
// Worth the small duplication because it needs no bind(2): these tests then run in restricted
// sandboxes (agent environments, hardened CI) where httptest's loopback listener is refused, which
// is exactly where an observation-log regression would otherwise go unnoticed. One request per
// client, since a pipe conn and a onceListener are each single-use.
func pipedMITMClient(t *testing.T, pol Policy, rt http.RoundTripper) *http.Client { //nolint:unparam // rt is always a fake upstream; kept explicit for readability at call sites
	t.Helper()
	ca, err := newCA("test-run")
	if err != nil {
		t.Fatalf("newCA: %v", err)
	}
	pol.MITM = true
	srvConn, cliConn := net.Pipe()
	go func() { _ = (&http.Server{Handler: newHandler(pol, rt, nil, ca, nil)}).Serve(newOnceListener(srvConn)) }()

	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			// Any authority will do — DialContext hands back the pipe regardless; what matters is
			// that the client takes the CONNECT path, as a real proxy-aware client does.
			Proxy:           http.ProxyURL(&url.URL{Scheme: "http", Host: "proxy.invalid:3128"}),
			DialContext:     func(context.Context, string, string) (net.Conn, error) { return cliConn, nil },
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}
}

// TestObserveLogsRequestLineAndHeaderNamesOnly is the probe instrument's core contract: with
// LogRequests on, one MITM'd request produces a line naming the host, the path, and the header
// NAMES the guest sent — and no header value, no injected credential, and no query-parameter
// value anywhere in the log.
func TestObserveLogsRequestLineAndHeaderNamesOnly(t *testing.T) {
	buf := captureLog(t)
	upstream := &echoTransport{}
	rule := InjectRule{
		Host:  "api.example.com",
		Strip: []string{"x-api-key", "authorization"},
		Set:   map[string]string{"x-api-key": "resolved-secret-value"},
	}
	client := pipedMITMClient(t, Policy{
		Mode: ModeAllowlist, Allow: []string{"api.example.com"},
		Inject: []InjectRule{rule}, LogRequests: true,
	}, upstream)

	req, _ := http.NewRequest(http.MethodPost, "https://api.example.com/v1/messages?beta=true&token=secret-in-url", nil)
	req.Header.Set("Authorization", "Bearer guest-token-value")
	req.Header.Set("X-Model-Version", "2023-06-01")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request through MITM proxy: %v", err)
	}
	_ = resp.Body.Close()

	got := buf.String()
	for _, want := range []string{
		`observe CONNECT "api.example.com:443" via=mitm`,
		`observe mitm POST host="api.example.com:443" path="/v1/messages"`,
		`query=["beta","token"]`,
		`inject=true`,
		`observe mitm response host="api.example.com:443"`,
		`status=200`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("observation log missing %q; got:\n%s", want, got)
		}
	}
	// The header the guest sent must appear by NAME…
	if !strings.Contains(got, "authorization") || !strings.Contains(got, "x-model-version") {
		t.Errorf("observation log does not name the request's headers; got:\n%s", got)
	}
	// …and nothing that carried a value may appear at all (§6.6.1).
	for _, forbidden := range []string{"guest-token-value", "resolved-secret-value", "secret-in-url", "2023-06-01"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("observation log leaked the value %q; got:\n%s", forbidden, got)
		}
	}
}

// TestObserveOffByDefaultLeavesSuccessfulRunSilent pins the behavior that made the probe procedure
// look broken: without LogRequests, a fully successful run writes NOTHING, so proxy.log is
// legitimately empty. Changing that is a deliberate decision, not a bug fix — this test is here to
// make it a deliberate one.
func TestObserveOffByDefaultLeavesSuccessfulRunSilent(t *testing.T) {
	buf := captureLog(t)
	upstream := &echoTransport{}
	client := pipedMITMClient(t, Policy{Mode: ModeAllowlist, Allow: []string{"api.example.com"}}, upstream)

	resp, err := client.Get("https://api.example.com/v1/messages")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	if got := buf.String(); got != "" {
		t.Errorf("a successful run with LogRequests off must log nothing; got:\n%s", got)
	}
}

// TestBlockedHostAlwaysLogsDenialReason covers the one thing that is logged regardless of
// LogRequests: a policy denial. On a CONNECT the guest's client discards the 403 body, so without
// this line the single most likely cause of a failed run leaves no trace in proxy.log (§6.6, §9).
func TestBlockedHostAlwaysLogsDenialReason(t *testing.T) {
	buf := captureLog(t)
	h := newHandler(Policy{Mode: ModeAllowlist, Allow: []string{"api.example.com"}}, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodConnect, "http://blocked.example.com:443", nil)
	req.Host = "blocked.example.com:443"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	got := buf.String()
	if !strings.Contains(got, `"blocked.example.com"`) || !strings.Contains(got, "blocked by the network policy") {
		t.Errorf("denial not logged with its reason; got:\n%s", got)
	}
	if !strings.Contains(got, "mode=allowlist") {
		t.Errorf("denial log omits the policy mode that caused it; got:\n%s", got)
	}
}

// TestObservedHostIsNeverTheGuestsHostHeader proves the logged host is the CONNECT authority the
// allowlist already approved, not the guest's inner Host header — the same rule the upstream target
// itself follows (§4.4, §6). A guest that could steer this string could forge misleading proxy.log
// entries about which host it contacted.
func TestObservedHostIsNeverTheGuestsHostHeader(t *testing.T) {
	buf := captureLog(t)
	obs := newObserver(Policy{LogRequests: true})
	obs.request("mitm", "api.example.com:443",
		&http.Request{Method: http.MethodGet, Host: "evil.example.com", URL: &url.URL{Path: "/v1"}}, false)

	got := buf.String()
	if !strings.Contains(got, `host="api.example.com:443"`) {
		t.Errorf("observation must log the approved authority; got:\n%s", got)
	}
	if strings.Contains(got, "evil.example.com") {
		t.Errorf("observation logged the guest-supplied Host header; got:\n%s", got)
	}
}

// TestObserveQuotesHostileRequestText proves a guest cannot forge a log line: a path carrying a
// newline is quoted, so the injected text cannot start a new record in proxy.log.
func TestObserveQuotesHostileRequestText(t *testing.T) {
	buf := captureLog(t)
	obs := newObserver(Policy{LogRequests: true})
	obs.request("mitm", "api.example.com:443",
		&http.Request{Method: http.MethodGet, URL: &url.URL{
			Path: "/v1\nkrayt-egress-proxy: observe mitm GET host=\"forged.example.com\"",
		}}, false)

	got := strings.TrimSuffix(buf.String(), "\n")
	if strings.Contains(got, "\n") {
		t.Errorf("hostile path broke the log record onto a second line; got:\n%s", got)
	}
	if !strings.Contains(got, `\n`) {
		t.Errorf("newline in the path was not escaped; got:\n%s", got)
	}
}

// TestObserveHeaderValuesRecordsOptInFlagsButNeverCredentials is the LogHeaderValues contract: a
// named non-secret header is recorded verbatim (a probe must not have to guess an API's required
// opt-in flag), while a credential-bearing name — even when the operator names it explicitly — is
// reduced to scheme + length. The credential itself must not appear, in any header.
func TestObserveHeaderValuesRecordsOptInFlagsButNeverCredentials(t *testing.T) {
	buf := captureLog(t)
	upstream := &echoTransport{}
	client := pipedMITMClient(t, Policy{
		Mode: ModeAllowlist, Allow: []string{"api.example.com"},
		LogHeaderValues: []string{"authorization", "x-beta-flags", "x-api-key", "x-absent"},
	}, upstream)

	req, _ := http.NewRequest(http.MethodPost, "https://api.example.com/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer oat-0123456789")    // 14 bytes after the scheme
	req.Header.Set("X-Api-Key", "sk-key-value-not-for-logging") // credential-bearing, shape only
	req.Header.Set("X-Beta-Flags", "shape-translation-2026-08-17")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	got := buf.String()
	for _, want := range []string{
		`authorization=<scheme="Bearer" credential_len=14>`,
		`x-beta-flags="shape-translation-2026-08-17"`, // the whole point: opt-in flags recorded exactly
		`x-api-key=<scheme=none credential_len=28>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("value log missing %q; got:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"oat-0123456789", "sk-key-value-not-for-logging"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("value log leaked the credential %q; got:\n%s", forbidden, got)
		}
	}
	if strings.Contains(got, "x-absent") {
		t.Errorf("a named-but-absent header must not be reported at all; got:\n%s", got)
	}
	// LogHeaderValues alone implies observation is on — no separate LogRequests needed.
	if !strings.Contains(got, "observe mitm POST") {
		t.Errorf("LogHeaderValues did not imply LogRequests; got:\n%s", got)
	}
}

// TestObserveInjectedHeaderValueIsNeverLogged closes the loophole that matters most: a header this
// run's own inject rules touch carries the credential krayt itself attached, so naming it in
// LogHeaderValues must yield a shape, not a value — even though the name (x-vendor-token here) is
// on no built-in credential list.
func TestObserveInjectedHeaderValueIsNeverLogged(t *testing.T) {
	buf := captureLog(t)
	rule := InjectRule{Host: "api.example.com", Set: map[string]string{"x-vendor-token": "real-secret-value"}}
	obs := newObserver(Policy{LogHeaderValues: []string{"x-vendor-token"}, Inject: []InjectRule{rule}})

	hdr := http.Header{}
	hdr.Set("X-Vendor-Token", "real-secret-value")
	obs.request("mitm", "api.example.com:443",
		&http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/v1"}, Header: hdr}, true)

	got := buf.String()
	if strings.Contains(got, "real-secret-value") {
		t.Errorf("an inject-rule header's value was logged; got:\n%s", got)
	}
	if !strings.Contains(got, `x-vendor-token=<scheme=none credential_len=17>`) {
		t.Errorf("inject-rule header not reduced to its shape; got:\n%s", got)
	}
}

func TestCredentialShape(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{"Bearer abc123", `<scheme="Bearer" credential_len=6>`},
		{"Basic dXNlcjpwdw==", `<scheme="Basic" credential_len=12>`},
		{"sk-ant-oat01-raw-token", `<scheme=none credential_len=22>`},
		// A value whose first word is not a scheme token must not have those bytes printed as if
		// it were one — the credential could be anything, including something with a space in it.
		{"sk-ant-01-with space", `<scheme=none credential_len=20>`},
		// A first word that merely LOOKS token-shaped (letters only, no digits) must not be
		// mistaken for a real scheme either — otherwise those credential bytes are printed verbatim.
		{"secret phrase", `<scheme=none credential_len=13>`},
		{"", `<scheme=none credential_len=0>`},
	}
	for _, tc := range tests {
		if got := credentialShape(tc.value); got != tc.want {
			t.Errorf("credentialShape(%q) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestHeaderNamesSortedAndLowercased(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("X-Api-Key", "value")
	hdr.Set("Accept", "value")
	hdr.Set("Content-Type", "value")
	if got, want := headerNames(hdr), "[accept,content-type,x-api-key]"; got != want {
		t.Errorf("headerNames = %q, want %q", got, want)
	}
	if got := headerNames(http.Header{}); got != "[]" {
		t.Errorf("headerNames(empty) = %q, want []", got)
	}
}

func TestQueryNamesFieldNamesOnly(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"no query", "https://h/p", ""},
		{"names only, sorted", "https://h/p?z=1&a=token-value", ` query=["a","z"]`},
		{"valueless flag", "https://h/p?beta", ` query=["beta"]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := queryNamesField(u); got != tc.want {
				t.Errorf("queryNamesField(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
	if got := queryNamesField(nil); got != "" {
		t.Errorf("queryNamesField(nil) = %q, want empty", got)
	}
}

// TestObserveReportsTheInjectedShapeUpstream is what a shape-translation verification run reads:
// the guest sends a placeholder on one header, and the `sent=` fragment on the response line shows
// the DIFFERENT header and scheme that actually went upstream — the only place injection is
// observable, since the request line logs the guest's own headers. The real credential still
// appears only as scheme + length.
func TestObserveReportsTheInjectedShapeUpstream(t *testing.T) {
	buf := captureLog(t)
	upstream := &echoTransport{}
	rule := InjectRule{
		Host:  "api.example.com",
		Strip: []string{"x-api-key", "authorization"},
		Set:   map[string]string{"authorization": "Bearer real-token-value"},
	}
	client := pipedMITMClient(t, Policy{
		Mode: ModeAllowlist, Allow: []string{"api.example.com"}, Inject: []InjectRule{rule},
		LogHeaderValues: []string{"authorization", "x-api-key", "x-beta"},
	}, upstream)

	req, _ := http.NewRequest(http.MethodPost, "https://api.example.com/v1/messages", nil)
	req.Header.Set("X-Api-Key", "sk-placeholder-not-a-credential")
	req.Header.Set("X-Beta", "client-flag")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	got := buf.String()
	// The guest's own request: the placeholder, on the header the container was configured with.
	if !strings.Contains(got, `x-api-key=<scheme=none credential_len=31>`) {
		t.Errorf("request line does not show the placeholder the container sent; got:\n%s", got)
	}
	// What actually went upstream: a different header, carrying a scheme the guest never sent.
	if !strings.Contains(got, `sent=[authorization=<scheme="Bearer" credential_len=16>`) {
		t.Errorf("response line does not report the injected credential shape; got:\n%s", got)
	}
	// The guest's own list header is forwarded untouched — krayt no longer rewrites it (shape
	// mirroring means the agent composes its own opt-in flags).
	if !strings.Contains(got, `x-beta="client-flag"`) {
		t.Errorf("response line does not show the guest's list header forwarded as sent; got:\n%s", got)
	}
	if strings.Contains(got, "real-token-value") {
		t.Errorf("the injected credential leaked into the log; got:\n%s", got)
	}
}
