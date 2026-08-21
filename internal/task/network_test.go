package task

import (
	"strings"
	"testing"
)

func TestValidateNetworkPolicyValid(t *testing.T) {
	cases := []struct {
		name string
		np   NetworkPolicy
		keys map[string]bool
	}{
		{
			name: "no mitm, no inject",
			np:   NetworkPolicy{Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"}},
		},
		{
			name: "mitm with no inject",
			np:   NetworkPolicy{Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true},
		},
		{
			name: "passthrough subset of allow",
			np: NetworkPolicy{
				Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com", "github.com"},
				MITM: true, Passthrough: []string{"github.com"},
			},
		},
		{
			name: "valid inject rule, key exists, strip defaults to set keys",
			np: NetworkPolicy{
				Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true,
				Inject: []InjectRule{{Host: "api.anthropic.com", Set: map[string]string{"x-api-key": "ANTHROPIC_API_KEY"}}},
			},
			keys: map[string]bool{"ANTHROPIC_API_KEY": true},
		},
		{
			name: "set_literal alone is valid (no secrets file needed)",
			np: NetworkPolicy{
				Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true,
				Inject: []InjectRule{{Host: "api.anthropic.com", SetLiteral: map[string]string{"x-krayt": "1"}}},
			},
		},
		{
			name: "mode full: inject host needs no allow-list membership",
			np: NetworkPolicy{
				Mode: NetworkFull, MITM: true,
				Inject: []InjectRule{{Host: "api.anthropic.com", Set: map[string]string{"x-api-key": "K"}}},
			},
			keys: map[string]bool{"K": true},
		},
		{
			name: "mode full: passthrough is free-form",
			np:   NetworkPolicy{Mode: NetworkFull, MITM: true, Passthrough: []string{"anything.example.com"}},
		},
		{
			name: "valid refresh block",
			np: NetworkPolicy{
				Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true,
				Inject: []InjectRule{{
					Host: "api.anthropic.com", Set: map[string]string{"x-api-key": "K"},
					Refresh: &RefreshRule{Host: "api.anthropic.com", PathPrefix: "/oauth", ResponseTokenFields: []string{"access_token"}},
				}},
			},
			keys: map[string]bool{"K": true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateNetworkPolicy(tc.np, tc.keys); err != nil {
				t.Errorf("ValidateNetworkPolicy() = %v, want nil", err)
			}
		})
	}
}

func TestValidateNetworkPolicyInvalid(t *testing.T) {
	cases := []struct {
		name string
		np   NetworkPolicy
		keys map[string]bool
	}{
		{
			name: "inject without mitm",
			np: NetworkPolicy{
				Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"},
				Inject: []InjectRule{{Host: "api.anthropic.com", Set: map[string]string{"x-api-key": "K"}}},
			},
			keys: map[string]bool{"K": true},
		},
		{
			name: "inject host also in passthrough",
			np: NetworkPolicy{
				Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true,
				Passthrough: []string{"api.anthropic.com"},
				Inject:      []InjectRule{{Host: "api.anthropic.com", Set: map[string]string{"x-api-key": "K"}}},
			},
			keys: map[string]bool{"K": true},
		},
		{
			name: "inject host not in allow (allowlist mode)",
			np: NetworkPolicy{
				Mode: NetworkAllowlist, Allow: []string{"other.example.com"}, MITM: true,
				Inject: []InjectRule{{Host: "api.anthropic.com", Set: map[string]string{"x-api-key": "K"}}},
			},
			keys: map[string]bool{"K": true},
		},
		{
			name: "passthrough not in allow (allowlist mode)",
			np: NetworkPolicy{
				Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true,
				Passthrough: []string{"github.com"},
			},
		},
		{
			name: "set references missing secrets key",
			np: NetworkPolicy{
				Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true,
				Inject: []InjectRule{{Host: "api.anthropic.com", Set: map[string]string{"x-api-key": "TYPO_KEY"}}},
			},
			keys: map[string]bool{"ANTHROPIC_API_KEY": true},
		},
		{
			name: "no set or set_literal",
			np: NetworkPolicy{
				Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true,
				Inject: []InjectRule{{Host: "api.anthropic.com"}},
			},
		},
		{
			name: "invalid header token",
			np: NetworkPolicy{
				Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true,
				Inject: []InjectRule{{Host: "api.anthropic.com", SetLiteral: map[string]string{"bad header": "1"}}},
			},
		},
		{
			name: "hop-by-hop header rejected",
			np: NetworkPolicy{
				Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true,
				Inject: []InjectRule{{Host: "api.anthropic.com", SetLiteral: map[string]string{"Connection": "keep-alive"}}},
			},
		},
		{
			name: "duplicate host across rules",
			np: NetworkPolicy{
				Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true,
				Inject: []InjectRule{
					{Host: "api.anthropic.com", SetLiteral: map[string]string{"a": "1"}},
					{Host: "api.anthropic.com", SetLiteral: map[string]string{"b": "2"}},
				},
			},
		},
		{
			name: "empty host",
			np: NetworkPolicy{
				Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true,
				Inject: []InjectRule{{Host: "", SetLiteral: map[string]string{"a": "1"}}},
			},
		},
		{
			name: "header in both set and set_literal",
			np: NetworkPolicy{
				Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true,
				Inject: []InjectRule{{
					Host: "api.anthropic.com",
					Set:  map[string]string{"x-api-key": "K"}, SetLiteral: map[string]string{"x-api-key": "1"},
				}},
			},
			keys: map[string]bool{"K": true},
		},
		{
			name: "header in both set and set_literal, differing only by case",
			np: NetworkPolicy{
				Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true,
				Inject: []InjectRule{{
					Host: "api.anthropic.com",
					Set:  map[string]string{"X-Api-Key": "K"}, SetLiteral: map[string]string{"x-api-key": "1"},
				}},
			},
			keys: map[string]bool{"K": true},
		},
		{
			name: "incomplete refresh block",
			np: NetworkPolicy{
				Mode: NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true,
				Inject: []InjectRule{{
					Host: "api.anthropic.com", Set: map[string]string{"x-api-key": "K"},
					Refresh: &RefreshRule{Host: "api.anthropic.com"}, // missing path_prefix + response_token_fields
				}},
			},
			keys: map[string]bool{"K": true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateNetworkPolicy(tc.np, tc.keys); err == nil {
				t.Error("ValidateNetworkPolicy() = nil, want an error")
			}
		})
	}
}

// TestValidateNetworkPolicyHostEntries pins the pre-flight half of "one definition of the same
// hostname". internal/proxy's newHandler drops a host entry its normalizeHost refuses, so any
// entry accepted here that the proxy would refuse means the run silently enforces a policy the
// user did not write — an allowlist entry, or an inject rule, quietly absent. The bad cases below
// are exactly the ones internal/proxy's TestNormalizeHost refuses; the good ones are exactly what
// it accepts and folds to the same bare key.
func TestValidateNetworkPolicyHostEntries(t *testing.T) {
	bad := map[string]string{
		// Written as escapes on purpose: these three are the runes that look like ASCII (or fold
		// onto it), so a literal is exactly the thing an editor or a copy-paste can silently
		// normalize away, leaving a test that passes while testing nothing.
		"non-ASCII lookalike":   "api.anthrop\u0130c.com", // U+0130 'İ': ToLower folds it onto "api.anthropic.com"
		"Kelvin sign":           "\u212Aey.example.com",   // U+212A KELVIN SIGN, indistinguishable from 'K'
		"Cyrillic homoglyph":    "\u0430pi.anthropic.com", // U+0430 CYRILLIC SMALL A, indistinguishable from 'a'
		"a URL, not a host":     "https://api.example.com",
		"path":                  "api.example.com/v1",
		"userinfo":              "user@api.example.com",
		"percent-escape":        "api.anthrop%C4%B0c.com",
		"CRLF":                  "api.example.com\r\nX: y",
		"bracketed IPv6":        "[2606:4700:4700::1111]", // must be written bare, so both lists key alike
		"bracketed non-literal": "[api.example.com]",
		"unbalanced bracket":    "[2606:4700:4700::1111",
		"whitespace-only":       "   ",
		"empty allow entry":     "",
	}
	for name, host := range bad {
		t.Run("allow: "+name, func(t *testing.T) {
			if err := ValidateNetworkPolicy(NetworkPolicy{Mode: NetworkAllowlist, Allow: []string{host}}, nil); err == nil {
				t.Errorf("ValidateNetworkPolicy(allow: %q) = nil, want an error — the proxy will drop this entry", host)
			}
		})
		if host == "" {
			continue // an empty inject host has its own, more specific "host is required" error
		}
		t.Run("inject: "+name, func(t *testing.T) {
			np := NetworkPolicy{
				Mode: NetworkFull, MITM: true,
				Inject: []InjectRule{{Host: host, Set: map[string]string{"x-api-key": "K"}}},
			}
			if err := ValidateNetworkPolicy(np, map[string]bool{"K": true}); err == nil {
				t.Errorf("ValidateNetworkPolicy(inject.host: %q) = nil, want an error", host)
			}
		})
		t.Run("passthrough: "+name, func(t *testing.T) {
			np := NetworkPolicy{Mode: NetworkFull, MITM: true, Passthrough: []string{host}}
			if err := ValidateNetworkPolicy(np, nil); err == nil {
				t.Errorf("ValidateNetworkPolicy(passthrough: %q) = nil, want an error", host)
			}
		})
	}

	good := []string{
		"api.anthropic.com", "API.Anthropic.COM", "  api.example.com  ",
		"xn--80ak6aa92e.com", // punycode is ordinary ASCII LDH
		"host-1.sub.example", "1.2.3.4",
		"2606:4700:4700::1111", // an IPv6 literal, written bare
	}
	for _, host := range good {
		t.Run("allow: "+host, func(t *testing.T) {
			if err := ValidateNetworkPolicy(NetworkPolicy{Mode: NetworkAllowlist, Allow: []string{host}}, nil); err != nil {
				t.Errorf("ValidateNetworkPolicy(allow: %q) = %v, want nil", host, err)
			}
		})
	}
}

func TestInjectedSecretKeys(t *testing.T) {
	np := NetworkPolicy{Inject: []InjectRule{
		{Host: "a.example.com", Set: map[string]string{"x-api-key": "KEY_A"}},
		{Host: "b.example.com", Set: map[string]string{"authorization": "KEY_B", "x-other": "KEY_A"}},
	}}
	got := np.InjectedSecretKeys()
	want := map[string]bool{"KEY_A": true, "KEY_B": true}
	if len(got) != len(want) {
		t.Fatalf("InjectedSecretKeys() = %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("InjectedSecretKeys() missing %q", k)
		}
	}
}

func TestInjectedSecretKeysEmpty(t *testing.T) {
	if got := (NetworkPolicy{}).InjectedSecretKeys(); got != nil {
		t.Errorf("InjectedSecretKeys() = %v, want nil for no inject rules", got)
	}
}

// TestMergeInjectRulesNoOverlap: an adapter rule for a host the user never mentioned is added
// as-is, alongside the user's own untouched rule (§4, inject-claude-oauth-token-at-proxy.md).
func TestMergeInjectRulesNoOverlap(t *testing.T) {
	user := []InjectRule{{Host: "example.com", Set: map[string]string{"x-token": "EX_KEY"}}}
	adapterRules := []InjectRule{{Host: "api.anthropic.com", Strip: []string{"x-api-key"}, Set: map[string]string{"x-api-key": "ANTHROPIC_API_KEY"}}}

	merged, overrides := MergeInjectRules(user, adapterRules)
	if len(overrides) != 0 {
		t.Errorf("no overlap should log no overrides, got %v", overrides)
	}
	if len(merged) != 2 {
		t.Fatalf("want 2 rules, got %+v", merged)
	}
	byHost := map[string]InjectRule{}
	for _, r := range merged {
		byHost[r.Host] = r
	}
	if byHost["example.com"].Set["x-token"] != "EX_KEY" {
		t.Errorf("user rule not preserved: %+v", byHost["example.com"])
	}
	if byHost["api.anthropic.com"].Set["x-api-key"] != "ANTHROPIC_API_KEY" {
		t.Errorf("adapter rule not added: %+v", byHost["api.anthropic.com"])
	}
}

// TestMergeInjectRulesSameHostDifferentHeaders: same host, non-conflicting headers — merged into
// one rule, strip lists unioned, no override logged.
func TestMergeInjectRulesSameHostDifferentHeaders(t *testing.T) {
	user := []InjectRule{{Host: "api.anthropic.com", Strip: []string{"x-custom"}, SetLiteral: map[string]string{"x-krayt": "1"}}}
	adapterRules := []InjectRule{{Host: "api.anthropic.com", Strip: []string{"x-api-key", "authorization"}, Set: map[string]string{"x-api-key": "ANTHROPIC_API_KEY"}}}

	merged, overrides := MergeInjectRules(user, adapterRules)
	if len(overrides) != 0 {
		t.Errorf("non-conflicting headers should log no overrides, got %v", overrides)
	}
	if len(merged) != 1 {
		t.Fatalf("want 1 merged rule, got %+v", merged)
	}
	r := merged[0]
	if r.Set["x-api-key"] != "ANTHROPIC_API_KEY" {
		t.Errorf("adapter's Set not merged in: %+v", r)
	}
	if r.SetLiteral["x-krayt"] != "1" {
		t.Errorf("user's SetLiteral not preserved: %+v", r)
	}
	wantStrip := map[string]bool{"x-custom": true, "x-api-key": true, "authorization": true}
	if len(r.Strip) != len(wantStrip) {
		t.Fatalf("Strip = %v, want union of %v", r.Strip, wantStrip)
	}
	for _, h := range r.Strip {
		if !wantStrip[h] {
			t.Errorf("unexpected Strip entry %q", h)
		}
	}
}

// TestMergeInjectRulesSameHeaderUserWins is the §4 conflict rule: same host AND same header
// (case-insensitively) — the user's value wins, and MergeInjectRules reports the override rather
// than silently dropping it.
func TestMergeInjectRulesSameHeaderUserWins(t *testing.T) {
	user := []InjectRule{{Host: "api.anthropic.com", Set: map[string]string{"X-Api-Key": "MY_OWN_KEY"}}}
	adapterRules := []InjectRule{{Host: "api.anthropic.com", Set: map[string]string{"x-api-key": "ANTHROPIC_API_KEY"}}}

	merged, overrides := MergeInjectRules(user, adapterRules)
	if len(overrides) != 1 {
		t.Fatalf("want exactly one logged override, got %v", overrides)
	}
	if len(merged) != 1 {
		t.Fatalf("want 1 merged rule, got %+v", merged)
	}
	if got := merged[0].Set["X-Api-Key"]; got != "MY_OWN_KEY" {
		t.Errorf("Set[X-Api-Key] = %q, want the user's MY_OWN_KEY to survive untouched", got)
	}
	if _, added := merged[0].Set["x-api-key"]; added {
		t.Errorf("adapter's differently-cased header must not also be added: %+v", merged[0])
	}
	if got := (NetworkPolicy{Inject: merged}).InjectedSecretKeys(); !got["ANTHROPIC_API_KEY"] {
		t.Errorf("InjectedSecretKeys() = %v, want the overridden adapter credential ANTHROPIC_API_KEY "+
			"still withheld even though its header lost to the user's own rule — otherwise it rides "+
			"SecretsBundle into the guest, defeating host-side-only injection", got)
	}
}

// TestMergeInjectRulesRefreshUserWins mirrors the header-conflict rule for the Refresh block: a
// user-supplied Refresh for a host is never replaced by an adapter one, and the collision is
// logged the same way a header collision is.
func TestMergeInjectRulesRefreshUserWins(t *testing.T) {
	userRefresh := &RefreshRule{Host: "auth.example.com", PathPrefix: "/user", ResponseTokenFields: []string{"token"}}
	user := []InjectRule{{Host: "api.anthropic.com", Set: map[string]string{"x-a": "A"}, Refresh: userRefresh}}
	adapterRules := []InjectRule{{
		Host: "api.anthropic.com", Set: map[string]string{"x-b": "B"},
		Refresh: &RefreshRule{Host: "auth.anthropic.com", PathPrefix: "/adapter", ResponseTokenFields: []string{"access_token"}},
	}}

	merged, overrides := MergeInjectRules(user, adapterRules)
	if merged[0].Refresh != userRefresh {
		t.Errorf("Refresh = %+v, want the user's own block untouched", merged[0].Refresh)
	}
	if len(overrides) != 1 {
		t.Errorf("want the refresh collision logged, got %v", overrides)
	}
}

// TestMergeInjectRulesDoesNotMutateInputs: MergeInjectRules must not mutate either input slice's
// backing maps — a caller reusing spec.Network.Inject after the call must see it unchanged.
func TestMergeInjectRulesDoesNotMutateInputs(t *testing.T) {
	user := []InjectRule{{Host: "api.anthropic.com", Set: map[string]string{"x-a": "A"}}}
	adapterRules := []InjectRule{{Host: "api.anthropic.com", Set: map[string]string{"x-b": "B"}}}

	if _, _ = MergeInjectRules(user, adapterRules); len(user[0].Set) != 1 {
		t.Errorf("MergeInjectRules mutated the user's own InjectRule.Set: %+v", user[0].Set)
	}
	if len(adapterRules[0].Set) != 1 {
		t.Errorf("MergeInjectRules mutated the adapter's own InjectRule.Set: %+v", adapterRules[0].Set)
	}
}

// TestValidateNetworkPolicySetPrefix covers the mechanism shape translation added
// (InjectRule.SetPrefix): it must be rejected at `krayt run` pre-flight when it expresses an intent
// it cannot carry out, rather than silently degrading at request time — a prefix with no value to
// prefix would send a credential without its scheme.
func TestValidateNetworkPolicySetPrefix(t *testing.T) {
	base := func(r InjectRule) NetworkPolicy {
		r.Host = "api.example.com"
		return NetworkPolicy{
			Mode: NetworkAllowlist, Allow: []string{"api.example.com"}, MITM: true,
			Inject: []InjectRule{r},
		}
	}
	keys := map[string]bool{"TOKEN": true}

	tests := []struct {
		name    string
		rule    InjectRule
		wantErr string // substring; empty means the rule must validate
	}{
		{
			name: "valid prefix",
			rule: InjectRule{
				Set:       map[string]string{"authorization": "TOKEN"},
				SetPrefix: map[string]string{"authorization": "Bearer "},
			},
		},
		{
			name: "prefix casing need not match set's casing",
			rule: InjectRule{
				Set:       map[string]string{"Authorization": "TOKEN"},
				SetPrefix: map[string]string{"authorization": "Bearer "},
			},
		},
		{
			name: "prefix without a matching set entry",
			rule: InjectRule{
				Set:       map[string]string{"authorization": "TOKEN"},
				SetPrefix: map[string]string{"x-other": "Bearer "},
			},
			wantErr: "no matching set",
		},
		{
			name: "empty prefix",
			rule: InjectRule{
				Set:       map[string]string{"authorization": "TOKEN"},
				SetPrefix: map[string]string{"authorization": ""},
			},
			wantErr: "value is empty",
		},
		{
			name: "prefix carrying CRLF",
			rule: InjectRule{
				Set:       map[string]string{"authorization": "TOKEN"},
				SetPrefix: map[string]string{"authorization": "Bearer \r\nx-evil: 1"},
			},
			wantErr: "CR or LF",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNetworkPolicy(base(tc.rule), keys)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("ValidateNetworkPolicy = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("ValidateNetworkPolicy = nil, want an error containing %q", tc.wantErr)
			case tc.wantErr != "" && err != nil && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("ValidateNetworkPolicy = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestMergeInjectRulesCarriesPrefix proves SetPrefix obeys the SAME user-wins rule as
// set/set_literal: an adapter prefix rides along with the adapter's set when it is taken, and is
// dropped with it when the user has claimed that header — prefixing the USER'S value would corrupt
// a credential they configured deliberately.
func TestMergeInjectRulesCarriesPrefix(t *testing.T) {
	adapterRule := InjectRule{
		Host:      "api.example.com",
		Strip:     []string{"x-api-key", "authorization"},
		Set:       map[string]string{"authorization": "TOKEN"},
		SetPrefix: map[string]string{"authorization": "Bearer "},
	}

	t.Run("no user rule: adapter rule taken whole", func(t *testing.T) {
		merged, overrides := MergeInjectRules(nil, []InjectRule{adapterRule})
		if len(merged) != 1 || len(overrides) != 0 {
			t.Fatalf("merged = %+v, overrides = %v", merged, overrides)
		}
		if merged[0].SetPrefix["authorization"] != "Bearer " {
			t.Errorf("prefix not carried: %+v", merged[0])
		}
	})

	t.Run("user claims the auth header: adapter prefix dropped with it", func(t *testing.T) {
		user := []InjectRule{{
			Host: "api.example.com",
			Set:  map[string]string{"authorization": "MY_OWN_TOKEN"},
		}}
		merged, overrides := MergeInjectRules(user, []InjectRule{adapterRule})
		if len(merged) != 1 {
			t.Fatalf("merged = %+v", merged)
		}
		if merged[0].Set["authorization"] != "MY_OWN_TOKEN" {
			t.Errorf("user value must win, got %v", merged[0].Set)
		}
		if _, prefixed := merged[0].SetPrefix["authorization"]; prefixed {
			t.Errorf("adapter prefix must not be applied to the user's own value; got %v", merged[0].SetPrefix)
		}
		if len(overrides) == 0 {
			t.Error("the override should be reported to the user")
		}
	})

	t.Run("clone does not alias the adapter table", func(t *testing.T) {
		merged, _ := MergeInjectRules(nil, []InjectRule{adapterRule})
		merged[0].SetPrefix["authorization"] = "corrupted"
		if adapterRule.SetPrefix["authorization"] != "Bearer " {
			t.Error("MergeInjectRules mutated the caller's rule in place")
		}
	})
}
