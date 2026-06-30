package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAllowed exercises the L7 decision engine across modes and host matching, with no
// network (§6.6). This is the heart of the egress allowlist.
func TestAllowed(t *testing.T) {
	allowlist := Policy{Mode: ModeAllowlist, Allow: []string{"api.anthropic.com", "Registry.NPMJS.org"}}
	cases := []struct {
		policy Policy
		host   string
		want   bool
	}{
		{allowlist, "api.anthropic.com", true},
		{allowlist, "API.ANTHROPIC.COM", true},   // case-insensitive
		{allowlist, "registry.npmjs.org", true},  // allowlist entry case-insensitive
		{allowlist, "evil.example.com", false},   // not listed
		{allowlist, "anthropic.com", false},      // not a parent match
		{allowlist, "xapi.anthropic.com", false}, // not a fuzzy/substring match
		{Policy{Mode: ModeNone, Allow: []string{"api.anthropic.com"}}, "api.anthropic.com", false},
		{Policy{Mode: ModeFull}, "anything.example.com", true},
		{Policy{Mode: ""}, "api.anthropic.com", false}, // empty mode defaults to allowlist
	}
	for _, tc := range cases {
		h := newHandler(tc.policy, nil)
		if got := h.allowed(tc.host); got != tc.want {
			t.Errorf("mode=%q allow=%v host=%q: allowed=%v, want %v", tc.policy.Mode, tc.policy.Allow, tc.host, got, tc.want)
		}
	}
}

// fakeTransport stands in for the upstream so the forward path needs no real socket.
type fakeTransport struct{ reached *string }

func (f *fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.reached != nil {
		*f.reached = r.URL.Host
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("upstream-ok")),
		Header:     make(http.Header),
	}, nil
}

// TestServeHTTPForwarding checks that an allowlisted plain-HTTP request is forwarded to the
// upstream, while a blocked one is rejected with 403 and never reaches it.
func TestServeHTTPForwarding(t *testing.T) {
	t.Run("allowlisted forwards", func(t *testing.T) {
		var reached string
		h := newHandler(Policy{Mode: ModeAllowlist, Allow: []string{"api.anthropic.com"}}, &fakeTransport{reached: &reached})
		req := httptest.NewRequest(http.MethodGet, "http://api.anthropic.com/v1/messages", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		if reached != "api.anthropic.com" {
			t.Errorf("upstream host = %q, want api.anthropic.com", reached)
		}
	})

	t.Run("blocked never reaches upstream", func(t *testing.T) {
		var reached string
		h := newHandler(Policy{Mode: ModeAllowlist, Allow: []string{"api.anthropic.com"}}, &fakeTransport{reached: &reached})
		req := httptest.NewRequest(http.MethodGet, "http://evil.example.com/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
		if reached != "" {
			t.Errorf("blocked request reached upstream %q", reached)
		}
	})
}

// TestConnectBlocked checks a CONNECT to a non-allowlisted host is refused before any dial
// (the allow path's byte-tunnel is covered by the real-VM integration test).
func TestConnectBlocked(t *testing.T) {
	h := newHandler(Policy{Mode: ModeAllowlist, Allow: []string{"api.anthropic.com"}}, http.DefaultTransport)
	req := httptest.NewRequest(http.MethodConnect, "//blocked.example.com:443", nil)
	req.Host = "blocked.example.com:443"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("CONNECT to blocked host: status = %d, want 403", rec.Code)
	}
}
