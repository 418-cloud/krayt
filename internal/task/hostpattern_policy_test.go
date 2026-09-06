package task

import (
	"strings"
	"testing"
)

// The three asymmetric cases (support-wildcard-network-hosts.md). Swapping the old exact map
// lookups for "does any allow entry cover this?" is not enough on its own: the DIRECTION differs
// per check, and getting it wrong is silent in two of the three.

// Case 1 — allow `*.example.com` + passthrough `api.example.com` is VALID.
//
// Load-bearing beyond ergonomics: adapter-supplied secret scopes are always EXACT hosts
// (internal/adapter/claudecode.go's api.anthropic.com, and the gemini-cli/opencode equivalents),
// so without directional coverage an operator writing `allow: ["*.anthropic.com"]` would break
// every claude-code run at pre-flight on a config that is obviously correct.
func TestWildcardAllowCoversExactPassthrough(t *testing.T) {
	np := NetworkPolicy{
		Mode:        NetworkAllowlist,
		Allow:       []string{"*.example.com"},
		Passthrough: []string{"api.example.com"},
	}
	if err := ValidateNetworkPolicy(np, nil); err != nil {
		t.Fatalf("ValidateNetworkPolicy = %v, want nil — a wildcard allow entry covers an exact "+
			"passthrough host", err)
	}
	// The same shape through the msb-era validator, with an exact secret host under the wildcard —
	// the adapter-compatibility case itself.
	np = NetworkPolicy{Mode: NetworkAllowlist, Allow: []string{"*.anthropic.com"}}
	inject := []ConfigInjectRule{{Key: "ANTHROPIC_API_KEY", Host: "api.anthropic.com"}}
	if err := ValidateNetworkPolicyForMsb(np, map[string]bool{"ANTHROPIC_API_KEY": true}, inject); err != nil {
		t.Fatalf("ValidateNetworkPolicyForMsb = %v, want nil — an adapter's exact secret scope must "+
			"validate under a wildcard allow entry", err)
	}
}

// Case 2 — allow `api.example.com` + passthrough `*.example.com` is an ERROR, and the message has
// to say WHY rather than repeat "must also be in allow". --tls-bypass is emitted verbatim and msb's
// bypass matcher is independent of its net-rule matcher, so a bypass wider than the allowlist is a
// real mismatch between the policy krayt printed and the one msb enforces.
func TestExactAllowDoesNotCoverWildcardPassthrough(t *testing.T) {
	np := NetworkPolicy{
		Mode:        NetworkAllowlist,
		Allow:       []string{"api.example.com"},
		Passthrough: []string{"*.example.com"},
	}
	err := ValidateNetworkPolicy(np, nil)
	if err == nil {
		t.Fatal("ValidateNetworkPolicy = nil, want an error — a wildcard passthrough exempts a whole " +
			"subtree the exact allowlist never granted")
	}
	for _, want := range []string{`"*.example.com"`, "not covered by allow", `"api.example.com"`, "interception"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q — the operator cannot act on it", err, want)
		}
	}
}

// Case 3 — passthrough `*.example.com` + a secret scoped to `api.example.com` is an ERROR, and the
// message must NAME THE PASSTHROUGH ENTRY. This is a genuinely silent failure otherwise: a
// passthrough host is tunneled un-MITM'd, so the secret can never be substituted and the guest
// sends only msb's placeholder — while the secret's own host appears nowhere in the passthrough
// list literally, so an error naming only the secret host is unactionable.
func TestWildcardPassthroughSwallowsExactSecretHost(t *testing.T) {
	np := NetworkPolicy{
		Mode:        NetworkAllowlist,
		Allow:       []string{"*.example.com"},
		Passthrough: []string{"*.example.com"},
	}
	err := ValidateNetworkPolicyForMsb(np, map[string]bool{"GH_TOKEN": true},
		[]ConfigInjectRule{{Key: "GH_TOKEN", Host: "api.example.com"}})
	if err == nil {
		t.Fatal("ValidateNetworkPolicyForMsb = nil, want an error — a wildcard passthrough swallows " +
			"the exact secret host and the credential would never be substituted")
	}
	if !strings.Contains(err.Error(), `"*.example.com"`) {
		t.Errorf("error %q does not name the offending passthrough entry — the operator never wrote "+
			"%q in passthrough, so naming only the secret host is unactionable", err, "api.example.com")
	}
	for _, want := range []string{"GH_TOKEN", `"api.example.com"`, "placeholder"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}

	// The same shape on the pre-msb inject path, whose check is likewise an overlap.
	legacy := NetworkPolicy{
		Mode: NetworkAllowlist, Allow: []string{"*.example.com"}, MITM: true,
		Passthrough: []string{"*.example.com"},
		Inject:      []InjectRule{{Host: "api.example.com", Set: map[string]string{"x-api-key": "K"}}},
	}
	err = ValidateNetworkPolicy(legacy, map[string]bool{"K": true})
	if err == nil {
		t.Fatal("ValidateNetworkPolicy = nil, want an error for an inject host under a wildcard passthrough")
	}
	if !strings.Contains(err.Error(), `"*.example.com"`) {
		t.Errorf("error %q does not name the offending passthrough entry", err)
	}
}

// TestWildcardSecretHostValidatesUnderWildcardAllow pins decision 1 — wildcards are accepted in
// network.inject[].host too — rather than leaving it incidental to whichever validator ran.
func TestWildcardSecretHostValidatesUnderWildcardAllow(t *testing.T) {
	np := NetworkPolicy{Mode: NetworkAllowlist, Allow: []string{"*.blob.core.windows.net"}}
	inject := []ConfigInjectRule{{Key: "AZURE_TOKEN", Host: "*.blob.core.windows.net"}}
	if err := ValidateNetworkPolicyForMsb(np, map[string]bool{"AZURE_TOKEN": true}, inject); err != nil {
		t.Fatalf("ValidateNetworkPolicyForMsb = %v, want nil — a wildcard secret host under a wildcard "+
			"allow entry is the per-tenant storage case this feature exists for", err)
	}
	// A narrower wildcard secret host under a wider wildcard allow entry is covered too.
	np = NetworkPolicy{Mode: NetworkAllowlist, Allow: []string{"*.example.com"}}
	inject = []ConfigInjectRule{{Key: "AZURE_TOKEN", Host: "*.api.example.com"}}
	if err := ValidateNetworkPolicyForMsb(np, map[string]bool{"AZURE_TOKEN": true}, inject); err != nil {
		t.Fatalf("ValidateNetworkPolicyForMsb = %v, want nil for *.api.example.com under *.example.com", err)
	}
}

// TestSecretHostWiderThanAllowIsRejected is the other direction: a secret scoped to a whole subtree
// the allowlist granted only one host of must fail, or the scope krayt prints is wider than the one
// the allowlist justifies.
func TestSecretHostWiderThanAllowIsRejected(t *testing.T) {
	np := NetworkPolicy{Mode: NetworkAllowlist, Allow: []string{"api.example.com"}}
	inject := []ConfigInjectRule{{Key: "GH_TOKEN", Host: "*.example.com"}}
	err := ValidateNetworkPolicyForMsb(np, map[string]bool{"GH_TOKEN": true}, inject)
	if err == nil {
		t.Fatal("ValidateNetworkPolicyForMsb = nil, want an error — an exact allow entry cannot cover " +
			"a wildcard secret scope")
	}
}

// TestDangerousWildcardsRejectedAsSecretHosts is the case msb itself would NOT catch, and the
// reason decision 1 is safe: HostPattern::parse has no validation at all, so `--secret TOKEN@*`
// becomes HostPattern::Any — "any host (dangerous — secret can be exfiltrated)" in msb's own doc
// comment — and `--secret TOKEN@*.com` is accepted silently. krayt's pre-flight is the only guard,
// and it must refuse both on every field.
func TestDangerousWildcardsRejectedAsSecretHosts(t *testing.T) {
	for _, host := range []string{"*", "*.com"} {
		t.Run("secret host "+host, func(t *testing.T) {
			np := NetworkPolicy{Mode: NetworkFull} // full mode: no allow-list membership to fail on
			err := ValidateNetworkPolicyForMsb(np, map[string]bool{"GH_TOKEN": true},
				[]ConfigInjectRule{{Key: "GH_TOKEN", Host: host}})
			if err == nil {
				t.Fatalf("a secret scoped to %q validated — msb would take it as %s", host,
					map[string]string{"*": "HostPattern::Any", "*.com": "every domain under .com"}[host])
			}
		})
		t.Run("allow "+host, func(t *testing.T) {
			if err := ValidateNetworkPolicy(NetworkPolicy{Mode: NetworkAllowlist, Allow: []string{host}}, nil); err == nil {
				t.Errorf("allow: [%q] validated", host)
			}
		})
		t.Run("passthrough "+host, func(t *testing.T) {
			np := NetworkPolicy{Mode: NetworkFull, Passthrough: []string{host}}
			if err := ValidateNetworkPolicy(np, nil); err == nil {
				t.Errorf("passthrough: [%q] validated", host)
			}
		})
	}
}
