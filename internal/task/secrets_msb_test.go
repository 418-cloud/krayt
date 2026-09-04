package task

import (
	"reflect"
	"strings"
	"testing"
)

func TestSecretSpecsFromConfigHostSingularBecomesOneElementHosts(t *testing.T) {
	specs, err := SecretSpecsFromConfig([]ConfigInjectRule{{Key: "ANTHROPIC_API_KEY", Host: "api.anthropic.com"}})
	if err != nil {
		t.Fatalf("SecretSpecsFromConfig: %v", err)
	}
	want := []SecretSpec{{Key: "ANTHROPIC_API_KEY", Hosts: []string{"api.anthropic.com"}}}
	if !reflect.DeepEqual(specs, want) {
		t.Fatalf("specs = %+v, want %+v", specs, want)
	}
}

func TestSecretSpecsFromConfigHostsPluralPassesThrough(t *testing.T) {
	specs, err := SecretSpecsFromConfig([]ConfigInjectRule{{Key: "GH_TOKEN", Hosts: []string{"api.github.com", "github.com"}}})
	if err != nil {
		t.Fatalf("SecretSpecsFromConfig: %v", err)
	}
	want := []SecretSpec{{Key: "GH_TOKEN", Hosts: []string{"api.github.com", "github.com"}}}
	if !reflect.DeepEqual(specs, want) {
		t.Fatalf("specs = %+v, want %+v", specs, want)
	}
}

func TestSecretSpecsFromConfigHostAndHostsAreMutuallyExclusive(t *testing.T) {
	_, err := SecretSpecsFromConfig([]ConfigInjectRule{{Key: "GH_TOKEN", Host: "api.github.com", Hosts: []string{"github.com"}}})
	if err == nil {
		t.Fatal("expected an error when both host and hosts are set")
	}
}

func TestSecretSpecsFromConfigRequiresAHost(t *testing.T) {
	_, err := SecretSpecsFromConfig([]ConfigInjectRule{{Key: "GH_TOKEN"}})
	if err == nil {
		t.Fatal("expected an error when neither host nor hosts is set")
	}
}

func TestSecretSpecsFromConfigRequiresAKey(t *testing.T) {
	_, err := SecretSpecsFromConfig([]ConfigInjectRule{{Host: "api.github.com"}})
	if err == nil {
		t.Fatal("expected an error when key is empty")
	}
	if !strings.Contains(err.Error(), "key") {
		t.Errorf("error %q does not name key", err)
	}
}

func TestSecretSpecsFromConfigRejectsDuplicateKey(t *testing.T) {
	_, err := SecretSpecsFromConfig([]ConfigInjectRule{
		{Key: "GH_TOKEN", Host: "api.github.com"},
		{Key: "GH_TOKEN", Host: "github.com"},
	})
	if err == nil {
		t.Fatal("expected an error for a key scoped by two entries")
	}
}

func TestSecretSpecsFromConfigRejectsEachRemovedKey(t *testing.T) {
	cases := []struct {
		name string
		rule ConfigInjectRule
	}{
		{"strip", ConfigInjectRule{Key: "K", Host: "h.example", Strip: []string{"authorization"}}},
		{"set", ConfigInjectRule{Key: "K", Host: "h.example", Set: map[string]string{"authorization": "K"}}},
		{"set_prefix", ConfigInjectRule{Key: "K", Host: "h.example", SetPrefix: map[string]string{"authorization": "Bearer "}}},
		{"set_literal", ConfigInjectRule{Key: "K", Host: "h.example", SetLiteral: map[string]string{"x": "1"}}},
		{"refresh", ConfigInjectRule{Key: "K", Host: "h.example", Refresh: &ConfigRefresh{Host: "h.example", PathPrefix: "/", ResponseTokenFields: []string{"access_token"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SecretSpecsFromConfig([]ConfigInjectRule{tc.rule})
			if err == nil {
				t.Fatalf("expected an error for a %s key", tc.name)
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Errorf("error %q does not name %q", err, tc.name)
			}
		})
	}
}

func TestSecretSpecsFromConfigEmptyInputIsEmptyOutput(t *testing.T) {
	specs, err := SecretSpecsFromConfig(nil)
	if err != nil {
		t.Fatalf("SecretSpecsFromConfig(nil): %v", err)
	}
	if len(specs) != 0 {
		t.Fatalf("specs = %+v, want empty", specs)
	}
}

func TestMergeSecretSpecsUserWins(t *testing.T) {
	user := []SecretSpec{{Key: "ANTHROPIC_API_KEY", Hosts: []string{"user.example.com"}}}
	adapter := []SecretSpec{{Key: "ANTHROPIC_API_KEY", Hosts: []string{"api.anthropic.com"}}}
	merged, overrides := MergeSecretSpecs(user, adapter)
	if !reflect.DeepEqual(merged, user) {
		t.Errorf("merged = %+v, want the user's own scope %+v to win", merged, user)
	}
	if len(overrides) != 1 {
		t.Errorf("overrides = %v, want one override line", overrides)
	}
}

func TestMergeSecretSpecsAdapterFillsGap(t *testing.T) {
	adapter := []SecretSpec{{Key: "ANTHROPIC_API_KEY", Hosts: []string{"api.anthropic.com"}}}
	merged, overrides := MergeSecretSpecs(nil, adapter)
	if len(overrides) != 0 {
		t.Errorf("overrides = %v, want none", overrides)
	}
	if !reflect.DeepEqual(merged, adapter) {
		t.Errorf("merged = %+v, want %+v", merged, adapter)
	}
}

func TestSecretSpecsFromConfigMultipleEntriesPreserveOrder(t *testing.T) {
	specs, err := SecretSpecsFromConfig([]ConfigInjectRule{
		{Key: "ANTHROPIC_API_KEY", Host: "api.anthropic.com"},
		{Key: "GH_TOKEN", Hosts: []string{"api.github.com"}},
	})
	if err != nil {
		t.Fatalf("SecretSpecsFromConfig: %v", err)
	}
	want := []SecretSpec{
		{Key: "ANTHROPIC_API_KEY", Hosts: []string{"api.anthropic.com"}},
		{Key: "GH_TOKEN", Hosts: []string{"api.github.com"}},
	}
	if !reflect.DeepEqual(specs, want) {
		t.Fatalf("specs = %+v, want %+v", specs, want)
	}
}
