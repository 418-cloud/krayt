package proxy

import (
	"context"
	"errors"
	"io"
	"net"
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
		h := newHandler(tc.policy, nil, nil, nil, nil)
		if got := h.allowed(tc.host); got != tc.want {
			t.Errorf("mode=%q allow=%v host=%q: allowed=%v, want %v", tc.policy.Mode, tc.policy.Allow, tc.host, got, tc.want)
		}
	}
}

// TestCheckDialAddr exercises the post-resolution SSRF guard (§6.6) directly: the pure range
// decision, with no network and no mode parameter — since the proxy moved host-side, every
// private/special range is blocked in every mode with no carve-out (move-egress-proxy-to-
// host.md §2). This is the heart of the resolved-IP guard.
func TestCheckDialAddr(t *testing.T) {
	// addr joins an IP with a port, bracketing IPv6 correctly (":443" concatenation would
	// mangle "::1" into a different, valid address).
	addr := func(ip string) string { return net.JoinHostPort(ip, "443") }
	// Blocked everywhere: loopback/link-local/metadata/unspecified/multicast plus, since the
	// proxy is host-side now, every private/ULA/CGNAT range too — no mode carve-out survives.
	blocked := []string{
		"169.254.169.254", // cloud metadata
		"127.0.0.1",       // IPv4 loopback
		"::1",             // IPv6 loopback
		"fe80::1",         // IPv6 link-local
		"169.254.10.20",   // IPv4 link-local
		"0.0.0.0",         // unspecified
		"::",              // unspecified v6
		"224.0.0.1",       // multicast
		"10.1.2.3",        // RFC 1918
		"192.168.1.1",     // RFC 1918
		"172.16.0.1",      // RFC 1918
		"100.64.0.1",      // RFC 6598 CGNAT
		"fc00::1",         // RFC 4193 ULA
	}
	for _, ip := range blocked {
		if err := checkDialAddr(addr(ip)); err == nil {
			t.Errorf("checkDialAddr(%q) = nil, want blocked", ip)
		} else if !errors.Is(err, errBlockedAddr) {
			t.Errorf("checkDialAddr(%q) err = %v, want errBlockedAddr", ip, err)
		}
	}

	// Public addresses: allowed (still gated by the host allowlist elsewhere).
	public := []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"}
	for _, ip := range public {
		if err := checkDialAddr(addr(ip)); err != nil {
			t.Errorf("checkDialAddr(%q) = %v, want allowed", ip, err)
		}
	}

	// Fail closed: an address that cannot be parsed is refused.
	for _, bad := range []string{"not-an-ip:443", "garbage", ""} {
		if err := checkDialAddr(bad); !errors.Is(err, errBlockedAddr) {
			t.Errorf("checkDialAddr(%q) = %v, want blocked (fail-closed)", bad, err)
		}
	}
}

// blockingTransport simulates the guarded HTTP dialer: RoundTrip fails with the SSRF-guard
// error, as it would when Control refuses the resolved IP, without touching the network.
type blockingTransport struct{ reached *bool }

func (b *blockingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	if b.reached != nil {
		*b.reached = true
	}
	return nil, &net.OpError{Op: "dial", Err: checkDialAddr("169.254.169.254:80")}
}

// TestGuardBlocksResolvedIP asserts that when an allowlisted name resolves to a blocked IP,
// both the forward and CONNECT paths return 403 (not 502) and never complete a connection.
func TestGuardBlocksResolvedIP(t *testing.T) {
	pol := Policy{Mode: ModeAllowlist, Allow: []string{"rebind.example.com"}}

	t.Run("forward returns 403", func(t *testing.T) {
		var reached bool
		h := newHandler(pol, &blockingTransport{reached: &reached}, nil, nil, nil)
		req := httptest.NewRequest(http.MethodGet, "http://rebind.example.com/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "blocked") {
			t.Errorf("body = %q, want blocked-address message", rec.Body.String())
		}
	})

	t.Run("connect returns 403 and does not tunnel", func(t *testing.T) {
		dialed := false
		dial := func(_ context.Context, _, _ string) (net.Conn, error) {
			dialed = true // Control would refuse before a real connect; simulate that here.
			return nil, &net.OpError{Op: "dial", Err: checkDialAddr("127.0.0.1:443")}
		}
		h := newHandler(pol, http.DefaultTransport, dial, nil, nil)
		req := httptest.NewRequest(http.MethodConnect, "//rebind.example.com:443", nil)
		req.Host = "rebind.example.com:443"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
		if !dialed {
			t.Error("expected the guarded dial to be attempted")
		}
	})
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
		h := newHandler(Policy{Mode: ModeAllowlist, Allow: []string{"api.anthropic.com"}}, &fakeTransport{reached: &reached}, nil, nil, nil)
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
		h := newHandler(Policy{Mode: ModeAllowlist, Allow: []string{"api.anthropic.com"}}, &fakeTransport{reached: &reached}, nil, nil, nil)
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
	h := newHandler(Policy{Mode: ModeAllowlist, Allow: []string{"api.anthropic.com"}}, http.DefaultTransport, nil, nil, nil)
	req := httptest.NewRequest(http.MethodConnect, "//blocked.example.com:443", nil)
	req.Host = "blocked.example.com:443"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("CONNECT to blocked host: status = %d, want 403", rec.Code)
	}
}
