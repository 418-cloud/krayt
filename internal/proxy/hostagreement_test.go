package proxy

import (
	"strings"
	"testing"

	"github.com/418-cloud/krayt/internal/task"
)

// TestHostRulesAgree runs one shared table of host strings through BOTH host predicates —
// internal/task's pre-flight validation (reached through its exported ValidateNetworkPolicy, the
// path `krayt run` actually takes) and this package's normalizeHost — and holds them to the
// invariant their comments claim.
//
// The two must stay deliberately duplicated: internal/proxy has no notion of a task or a secrets
// file and must not import internal/task, so nothing but a test can catch them drifting apart.
// This is a test-only import in the one direction that is acyclic.
//
// The invariant is ONE-directional: everything pre-flight ACCEPTS, normalizeHost must also accept
// and fold to the same bare key — otherwise a run starts with an allowlist entry or an inject rule
// the proxy silently dropped. Pre-flight may be stricter; that can only fail a run before it
// starts. Each stricter case is marked, so widening the gap has to be a deliberate edit here.
func TestHostRulesAgree(t *testing.T) {
	cases := []struct {
		host string
		// preflightOK is what task.ValidateNetworkPolicy must say; proxyOK is what normalizeHost
		// must say. preflightOK && !proxyOK is the forbidden combination and is asserted against
		// below rather than expressible in the table.
		preflightOK, proxyOK bool
		why                  string // set only where the two deliberately differ
	}{
		// Accepted by both, folding to the same key.
		{host: "api.anthropic.com", preflightOK: true, proxyOK: true},
		{host: "API.Anthropic.COM", preflightOK: true, proxyOK: true},
		{host: "  api.example.com  ", preflightOK: true, proxyOK: true},
		{host: "sub.domain.example", preflightOK: true, proxyOK: true},
		{host: "host-1.sub.example", preflightOK: true, proxyOK: true},
		{host: "xn--80ak6aa92e.com", preflightOK: true, proxyOK: true},
		{host: "10.0.0.1", preflightOK: true, proxyOK: true},
		{host: "::1", preflightOK: true, proxyOK: true},
		{host: "2606:4700:4700::1111", preflightOK: true, proxyOK: true},

		// Refused by both.
		{host: "", proxyOK: false},
		{host: "   ", proxyOK: false},
		{host: "a.example,evil.example", proxyOK: false},
		{host: "api.example.com evil.example", proxyOK: false},
		{host: "api.example.com\tevil.example", proxyOK: false},
		{host: "api.example\x00.com", proxyOK: false},
		{host: "api.example.com\r\nX: y", proxyOK: false},
		{host: "https://api.example.com", proxyOK: false},
		{host: "api.example.com/v1", proxyOK: false},
		{host: "user@api.example.com", proxyOK: false},
		{host: "api.anthrop%C4%B0c.com", proxyOK: false},
		{host: "api.anthropİc.com", proxyOK: false},
		{host: "Key.example.com", proxyOK: false},
		{host: "аpi.anthropic.com", proxyOK: false},
		{host: "[api.example.com]", proxyOK: false},
		{host: "[2606:4700:4700::1111", proxyOK: false},

		// Pre-flight is stricter. Legal under this direction of the invariant.
		{host: "[2606:4700:4700::1111]", proxyOK: true,
			why: "brackets fold fine, but this package's cross-checks key on lower(), which does not unwrap them — one spelling is demanded up front"},
		{host: "a..example", proxyOK: true,
			why: "an empty label folds to a usable key that no request host can ever equal"},
		{host: ".example.com", proxyOK: true, why: "leading dot: same"},
		{host: "example.com.", proxyOK: true, why: "trailing dot: same"},
		{host: "-example.com", proxyOK: true, why: "leading hyphen: same"},
		{host: "sub-.example.com", proxyOK: true, why: "label ends with a hyphen: same"},
	}

	for _, tc := range cases {
		t.Run(strings.ReplaceAll(tc.host, " ", "_"), func(t *testing.T) {
			err := task.ValidateNetworkPolicy(
				task.NetworkPolicy{Mode: task.NetworkAllowlist, Allow: []string{tc.host}}, nil)
			gotPreflight := err == nil
			if gotPreflight != tc.preflightOK {
				t.Fatalf("task.ValidateNetworkPolicy(allow: %q): accepted=%v (%v), want accepted=%v",
					tc.host, gotPreflight, err, tc.preflightOK)
			}
			folded, gotProxy := normalizeHost(tc.host)
			if gotProxy != tc.proxyOK {
				t.Fatalf("normalizeHost(%q) = %q, %v; want ok=%v", tc.host, folded, gotProxy, tc.proxyOK)
			}
			if gotPreflight && !gotProxy {
				t.Fatalf("host %q passes pre-flight but normalizeHost refuses it: the run would start "+
					"with this entry silently missing from the effective policy", tc.host)
			}
			// Agreement is not just yes/no: an entry pre-flight accepts must fold to the key
			// internal/task's own lower() will look it up under, or the two sides agree that the
			// host is usable and then disagree about which host it is.
			if gotPreflight {
				if want := strings.ToLower(strings.TrimSpace(tc.host)); folded != want {
					t.Fatalf("normalizeHost(%q) = %q, but internal/task keys this entry as %q", tc.host, folded, want)
				}
			}
			if tc.why != "" && tc.preflightOK {
				t.Fatalf("case %q is marked as a deliberate pre-flight-stricter case but pre-flight accepted it", tc.host)
			}
		})
	}
}
