package task

import "testing"

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
