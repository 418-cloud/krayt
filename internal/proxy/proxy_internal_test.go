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
//
// Each case goes through normalizeHost first, exactly as ServeHTTP does: a host that fails to
// normalize is refused at the choke point and never reaches allowed(), so "want false" here
// covers both "not on the list" and "not a host we will speak to at all".
func TestAllowed(t *testing.T) {
	allowlist := Policy{Mode: ModeAllowlist, Allow: []string{
		"api.anthropic.com", "Registry.NPMJS.org", "key.example.com",
		"xn--80ak6aa92e.com",   // an allowlist entry that is literally punycode
		"2606:4700:4700::1111", // IPv6 literal
	}}
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

		// Unicode that strings.ToLower — but NOT this package — would fold onto ASCII. Both are
		// refused outright; see normalizeHost. U+0130 is the one rune where simple case folding
		// and UTS-46 disagree, and it is what made the allowlist and the dial diverge.
		{allowlist, "api.anthropİc.com", false}, // 'İ' → ToLower gives "api.anthropic.com"
		{allowlist, "Key.example.com", false},   // KELVIN SIGN → ToLower gives "key.example.com"

		// A trailing dot is a different name to this package: matched literally or not at all,
		// never silently stripped.
		{allowlist, "api.anthropic.com.", false},
		// Percent-escapes are not part of a hostname. Refused rather than decoded — decoding is
		// what smuggles a non-ASCII host past a Host-header validator in the first place.
		{allowlist, "api.anthrop%C4%B0c.com", false},
		// Punycode is ordinary ASCII LDH: allowed only when it is literally on the list.
		{allowlist, "xn--80ak6aa92e.com", true},
		{allowlist, "api.xn--anthropic-dkf.com", false}, // the lookalike's real punycode
		// IPv6 literals survive normalization (requestHost has already stripped the brackets).
		{allowlist, "2606:4700:4700::1111", true},
		{allowlist, "2606:4700:4700::1112", false},
	}
	for _, tc := range cases {
		h := newHandler(tc.policy, nil, nil, nil, nil)
		host, ok := normalizeHost(tc.host)
		got := ok && h.allowed(host)
		if got != tc.want {
			t.Errorf("mode=%q allow=%v host=%q: allowed=%v (normalized=%q ok=%v), want %v",
				tc.policy.Mode, tc.policy.Allow, tc.host, got, host, ok, tc.want)
		}
	}
}

// lookalikeHost is "api.anthropic.com" with its 'i' replaced by U+0130 (LATIN CAPITAL LETTER I
// WITH DOT ABOVE) — the single rune whose Unicode simple case folding (strings.ToLower) yields an
// ASCII letter while UTS-46 (every http.Transport dial) yields a punycode label instead. An
// exhaustive sweep of Unicode finds only U+0130 and U+212A folding onto ASCII at all, and UTS-46
// agrees with ToLower on U+212A, so this is the one host spelling where the two sides of the
// policy decision could disagree.
const lookalikeHost = "api.anthropİc.com"

// lookalikePunycode is what any Go transport actually dials for lookalikeHost — a domain an
// attacker can simply register.
const lookalikePunycode = "api.xn--anthropic-dkf.com"

// TestUnicodeFoldingDoesNotBypassAllowlist is the permanent regression for the allowlist-bypass
// half of the folding bug: strings.ToLower(lookalikeHost) == "api.anthropic.com", so the policy
// approved it while the transport dialed the attacker's punycode domain — a complete egress
// bypass in the DEFAULT configuration (mode: allowlist, mitm: false). The host is now refused at
// the choke point, so no dial happens at all.
func TestUnicodeFoldingDoesNotBypassAllowlist(t *testing.T) {
	if strings.ToLower(lookalikeHost) != "api.anthropic.com" {
		t.Fatalf("premise broken: strings.ToLower(%q) = %q", lookalikeHost, strings.ToLower(lookalikeHost))
	}
	pol := Policy{Mode: ModeAllowlist, Allow: []string{"api.anthropic.com"}}

	t.Run("percent-encoded host in an absolute request-URI", func(t *testing.T) {
		var reached string
		h := newHandler(pol, &fakeTransport{reached: &reached}, nil, nil, nil)
		// Percent-encoding is what lets this reach the handler at all: raw non-ASCII in a Host
		// header is rejected by httpguts.ValidHostHeader, but net/url decodes %C4%B0 into
		// r.URL.Host, and net/http then sets r.Host from the URL for an absolute-URI request.
		req := httptest.NewRequest(http.MethodGet, "http://api.anthrop%C4%B0c.com/exfil?d=secret", nil)
		req.Host = "api.anthropic.com" // the innocent-looking Host the guest also sends
		if req.URL.Host != lookalikeHost {
			t.Fatalf("premise broken: net/url did not decode the host: %q", req.URL.Host)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
		if reached != "" {
			t.Errorf("proxy dialed %q for a request the allowlist must never approve", reached)
		}
	})

	t.Run("CONNECT to the lookalike", func(t *testing.T) {
		dialed := ""
		dial := func(_ context.Context, _, addr string) (net.Conn, error) {
			dialed = addr
			return nil, errors.New("should not be reached")
		}
		h := newHandler(pol, http.DefaultTransport, dial, nil, nil)
		req := httptest.NewRequest(http.MethodConnect, "//"+lookalikeHost+":443", nil)
		req.Host = lookalikeHost + ":443"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
		if dialed != "" {
			t.Errorf("proxy dialed %q for a CONNECT the allowlist must never approve", dialed)
		}
	})

	// The punycode the transport would have dialed is itself just another host: allowed only if
	// it is literally on the list.
	t.Run("the punycode target is not implicitly allowed", func(t *testing.T) {
		var reached string
		h := newHandler(pol, &fakeTransport{reached: &reached}, nil, nil, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+lookalikePunycode+"/", nil))
		if rec.Code != http.StatusForbidden || reached != "" {
			t.Errorf("status = %d, upstream = %q; want 403 and no dial", rec.Code, reached)
		}
	})
}

// TestForwardPathDialsExactlyTheApprovedHost is the permanent regression for the second half of
// the same bug: the allowlist checked one string while the transport resolved another (r.URL.Host,
// the guest's spelling). The forward path now re-points the outbound request at the normalized
// bytes, so what was approved and what is dialed are the same string, not merely equivalent ones.
func TestForwardPathDialsExactlyTheApprovedHost(t *testing.T) {
	var reached string
	h := newHandler(Policy{Mode: ModeAllowlist, Allow: []string{"api.anthropic.com"}}, &fakeTransport{reached: &reached}, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "http://API.Anthropic.COM:8080/v1/messages", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if reached != "api.anthropic.com:8080" {
		t.Errorf("dialed %q, want the normalized host the allowlist approved (api.anthropic.com:8080)", reached)
	}
	if req.Host != "api.anthropic.com:8080" {
		t.Errorf("outbound Host = %q, want the normalized authority", req.Host)
	}
}

// TestIPv6LiteralIsOneKeyOnEveryPath is the regression for the bracket half of "one host, one
// key": a CONNECT authority always carries a port, so net.SplitHostPort stripped its brackets in
// requestHost, while a plain "http://[2606:4700:4700::1111]/" had no port to split and kept them
// — so one allowlist entry matched the same target through CONNECT and denied it over plain HTTP.
// normalizeHost now unwraps brackets, and normalizedAuthority puts them back only to build an
// authority, so every path agrees on the bare literal and still dials a syntactically valid one.
func TestIPv6LiteralIsOneKeyOnEveryPath(t *testing.T) {
	const literal = "2606:4700:4700::1111"
	pol := Policy{Mode: ModeAllowlist, Allow: []string{literal}}

	// The same policy written with brackets must fold to the same key, not a second unreachable one.
	if len(newHandler(Policy{Mode: ModeAllowlist, Allow: []string{"[" + literal + "]"}}, nil, nil, nil, nil).allow) != 1 {
		t.Fatal("a bracketed allow entry did not survive normalization")
	}
	for _, entry := range []string{literal, "[" + literal + "]"} {
		h := newHandler(Policy{Mode: ModeAllowlist, Allow: []string{entry}}, nil, nil, nil, nil)
		if !h.allow[literal] {
			t.Errorf("allow entry %q keyed as %v, want the bare literal", entry, h.allow)
		}
	}

	t.Run("plain HTTP, no port", func(t *testing.T) {
		var reached string
		h := newHandler(pol, &fakeTransport{reached: &reached}, nil, nil, nil)
		req := httptest.NewRequest(http.MethodGet, "http://["+literal+"]/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 — the allowlist named this exact target", rec.Code)
		}
		// Brackets go back on for the outbound authority: "2606:...:1111" as a URL host would have
		// its colons read as a port separator.
		if want := "[" + literal + "]"; reached != want {
			t.Errorf("dialed %q, want %q", reached, want)
		}
	})

	t.Run("CONNECT, with port", func(t *testing.T) {
		dialed := ""
		h := newHandler(pol, nil, func(_ context.Context, _, addr string) (net.Conn, error) {
			dialed = addr
			return nil, errors.New("dial refused in test")
		}, nil, nil)
		req := httptest.NewRequest(http.MethodConnect, "//["+literal+"]:443", nil)
		req.Host = "[" + literal + "]:443"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("status = 403; the allowlist named this exact target")
		}
		if want := "[" + literal + "]:443"; dialed != want {
			t.Errorf("dialed %q, want %q", dialed, want)
		}
	})

	t.Run("an unlisted literal is still refused", func(t *testing.T) {
		var reached string
		h := newHandler(pol, &fakeTransport{reached: &reached}, nil, nil, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://[2606:4700:4700::1112]/", nil))
		if rec.Code != http.StatusForbidden || reached != "" {
			t.Errorf("status = %d, upstream = %q; want 403 and no dial", rec.Code, reached)
		}
	})
}

// TestNormalizeHost pins the fold itself: ASCII-only, byte-wise, never strings.ToLower.
func TestNormalizeHost(t *testing.T) {
	ok := map[string]string{
		"api.anthropic.com":     "api.anthropic.com",
		"API.Anthropic.COM":     "api.anthropic.com",
		"  api.example.com  ":   "api.example.com",
		"api.example.com.":      "api.example.com.", // trailing dot preserved, not stripped
		"xn--80ak6aa92e.com":    "xn--80ak6aa92e.com",
		"host-1.sub.example":    "host-1.sub.example",
		"2606:4700:4700::1111":  "2606:4700:4700::1111",
		"[2606:4700:4700::11]":  "2606:4700:4700::11", // brackets are authority syntax: unwrapped to the one canonical key
		"[2606:4700:4700::AA]":  "2606:4700:4700::aa",
		"1.2.3.4":               "1.2.3.4",
		"2606:4700:4700::AAAA":  "2606:4700:4700::aaaa",
		"UPPER.EXAMPLE.COM:443": "upper.example.com:443",
	}
	for in, want := range ok {
		if got, ok := normalizeHost(in); !ok || got != want {
			t.Errorf("normalizeHost(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}
	bad := []string{
		"",
		"   ",
		lookalikeHost,             // U+0130
		"Key.example.com",         // U+212A KELVIN SIGN
		"api.anthrop%C4%B0c.com",  // percent-escapes are not part of a hostname
		"api.example.com\x00",     // NUL
		"api.example.com/path",    // not a host
		"user@api.example.com",    // userinfo
		"api.example.com\r\nX: y", // header smuggling
		"fe80::1%25eth0",          // IPv6 zone id (percent-escaped, as net/url yields it)
		"exàmple.com",             // ordinary non-ASCII
		"аpi.anthropic.com",       // Cyrillic 'а' homoglyph
		"[2606:4700:4700::11",     // unbalanced bracket
		"[api.example.com]",       // brackets are IPv6-literal syntax and nothing else
		"[1.2.3.4]",               // ... which does not include IPv4
		"[]",
	}
	for _, in := range bad {
		if got, ok := normalizeHost(in); ok {
			t.Errorf("normalizeHost(%q) = %q, true; want refused", in, got)
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
