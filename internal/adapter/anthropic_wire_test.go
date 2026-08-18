package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/418-cloud/krayt/internal/task"
)

// TestAnthropicWireRulesGolden pins anthropicWireRules' exact contents (inject-claude-oauth-token-at-proxy.md
// "Making the treadmill cheap": "A golden test asserts the exact rule set each credential shape
// produces. When Anthropic changes something, that test fails, and its diff IS the changelog of
// what changed."). Update this test's literal values ONLY alongside a new dated PROVENANCE entry
// in anthropic_wire.go backed by a real probe observation — never to make a change pass.
func TestAnthropicWireRulesGolden(t *testing.T) {
	want := map[string]anthropicWireRule{
		"ANTHROPIC_API_KEY": {
			Host:        "api.anthropic.com",
			Strip:       []string{"x-api-key", "authorization"},
			Set:         "x-api-key",
			Placeholder: "sk-ant-krayt-placeholder-do-not-use",
		},
		"CLAUDE_CODE_OAUTH_TOKEN": {
			Host:        "api.anthropic.com",
			Strip:       []string{"x-api-key", "authorization"},
			Set:         "authorization",
			SetPrefix:   "Bearer ",
			Placeholder: "sk-ant-oat01-krayt-placeholder-do-not-use",
		},
	}
	if len(anthropicWireRules) != len(want) {
		t.Fatalf("anthropicWireRules has %d entries, want %d (got %+v)", len(anthropicWireRules), len(want), anthropicWireRules)
	}
	for cred, w := range want {
		got, ok := anthropicWireRules[cred]
		if !ok {
			t.Fatalf("anthropicWireRules missing %q", cred)
		}
		if got.Host != w.Host || got.Set != w.Set || got.SetPrefix != w.SetPrefix ||
			got.Placeholder != w.Placeholder ||
			strings.Join(got.Strip, ",") != strings.Join(w.Strip, ",") {
			t.Errorf("anthropicWireRules[%q] = %+v, want %+v", cred, got, w)
		}
	}
}

// TestAnthropicWireRulesSelfConsistent guards every CURRENT and FUTURE table entry against the
// shape anthropicInjectRuleFor assumes, so a probe update that adds a new entry gets this
// property for free without touching this test: a non-empty host, at least one strip header, and
// a non-empty Set target (the header the real credential value lands on).
func TestAnthropicWireRulesSelfConsistent(t *testing.T) {
	for cred, r := range anthropicWireRules {
		if r.Host == "" {
			t.Errorf("%s: empty Host", cred)
		}
		if len(r.Strip) == 0 {
			t.Errorf("%s: empty Strip", cred)
		}
		if r.Set == "" {
			t.Errorf("%s: empty Set", cred)
		}
		if r.Placeholder == "" {
			t.Errorf("%s: empty Placeholder — the container would be configured with no credential "+
				"at all and the agent would refuse to start", cred)
		}
	}
}

// TestAnthropicWireRulesPassRealValidation runs every table entry through the SAME validator a
// user-written network.inject rule faces (task.ValidateNetworkPolicy), so a future probe cannot add
// an entry the run pre-flight would then reject at `krayt run` time — a malformed prefix, an
// append item with a comma in it, a hop-by-hop target. This replaced an earlier tripwire test that
// asserted CLAUDE_CODE_OAUTH_TOKEN had NO entry; the 2026-08-17 probe (see PROVENANCE) gave it one,
// which is exactly the event that test existed to flag.
func TestAnthropicWireRulesPassRealValidation(t *testing.T) {
	for cred := range anthropicWireRules {
		rule, _, ok := anthropicInjectRuleFor(cred)
		if !ok {
			t.Fatalf("%s: anthropicInjectRuleFor returned ok=false for a table entry", cred)
		}
		np := task.NetworkPolicy{
			Mode:   task.NetworkAllowlist,
			Allow:  []string{rule.Host},
			MITM:   true,
			Inject: []task.InjectRule{rule},
		}
		if err := task.ValidateNetworkPolicy(np, map[string]bool{cred: true}); err != nil {
			t.Errorf("%s: table entry produces a rule the run pre-flight rejects: %v", cred, err)
		}
	}
}

// TestAnthropicInjectRuleForOAuthShape is the direct proof of the 2026-08-17 subscription-token
// observation plus the SHAPE MIRRORING decision (both in PROVENANCE): the real token goes on
// `authorization` behind the observed `Bearer ` scheme, and the container is configured with a
// placeholder under its OWN variable — CLAUDE_CODE_OAUTH_TOKEN, not ANTHROPIC_API_KEY — so Claude
// Code runs its subscription code path and emits the OAuth beta flags itself.
func TestAnthropicInjectRuleForOAuthShape(t *testing.T) {
	rule, placeholders, ok := anthropicInjectRuleFor("CLAUDE_CODE_OAUTH_TOKEN")
	if !ok {
		t.Fatal("CLAUDE_CODE_OAUTH_TOKEN should have an observed wire rule since the 2026-08-17 probe")
	}
	if rule.Set["authorization"] != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Errorf("Set = %v, want authorization -> CLAUDE_CODE_OAUTH_TOKEN", rule.Set)
	}
	if rule.SetPrefix["authorization"] != "Bearer " {
		t.Errorf("SetPrefix = %v, want authorization -> %q (the observed scheme, trailing space included)",
			rule.SetPrefix, "Bearer ")
	}
	// krayt rewrites no list header: under shape mirroring the CLI knows it is on a subscription
	// and composes anthropic-beta itself, so anything krayt set here would fight the client.
	if rule.SetLiteral["anthropic-beta"] != "" {
		t.Error("krayt must not set anthropic-beta — the agent composes its own beta list")
	}
	// Shape mirroring: the placeholder is delivered as the credential's OWN env var, never as
	// ANTHROPIC_API_KEY, which is what makes the CLI take its OAuth code path.
	if got := placeholders["CLAUDE_CODE_OAUTH_TOKEN"]; got != "sk-ant-oat01-krayt-placeholder-do-not-use" {
		t.Errorf("placeholders = %v, want CLAUDE_CODE_OAUTH_TOKEN -> the OAuth placeholder", placeholders)
	}
	if _, wrong := placeholders["ANTHROPIC_API_KEY"]; wrong {
		t.Errorf("an OAuth run must not configure ANTHROPIC_API_KEY at all; got %v", placeholders)
	}
}

// TestAnthropicInjectRuleForObservedShape is the direct proof of the design table in
// inject-claude-oauth-token-at-proxy.md's "The design" section for the one shape that's actually
// observed: the rule strips the guest's own auth headers and sets the real value on x-api-key,
// keyed by the credential's own secrets-file name so orchestrator resolution (which only knows
// secrets-file key names) needs no Anthropic-specific code.
func TestAnthropicInjectRuleForObservedShape(t *testing.T) {
	rule, placeholders, ok := anthropicInjectRuleFor("ANTHROPIC_API_KEY")
	if !ok {
		t.Fatal("ANTHROPIC_API_KEY should have an observed wire rule")
	}
	if rule.Host != "api.anthropic.com" {
		t.Errorf("Host = %q, want api.anthropic.com", rule.Host)
	}
	if rule.Set["x-api-key"] != "ANTHROPIC_API_KEY" {
		t.Errorf("Set = %v, want x-api-key -> ANTHROPIC_API_KEY", rule.Set)
	}
	if placeholders["ANTHROPIC_API_KEY"] != AnthropicPlaceholderAPIKey {
		t.Errorf("placeholders = %v, want ANTHROPIC_API_KEY -> %q", placeholders, AnthropicPlaceholderAPIKey)
	}
	if !strings.HasPrefix(AnthropicPlaceholderAPIKey, "sk-ant-") {
		t.Errorf("placeholder %q must keep the sk-ant- prefix a client-side format check expects (§3)", AnthropicPlaceholderAPIKey)
	}
}

// TestAnthropicInjectRuleForDoesNotAliasTable proves the returned rule's Strip slice is an
// independent copy — a caller mutating it (MergeInjectRules does, in place) must never corrupt
// the package-level table for the next run.
func TestAnthropicInjectRuleForDoesNotAliasTable(t *testing.T) {
	rule, _, ok := anthropicInjectRuleFor("ANTHROPIC_API_KEY")
	if !ok {
		t.Fatal("ANTHROPIC_API_KEY should have an observed wire rule")
	}
	rule.Strip[0] = "corrupted"
	again, _, _ := anthropicInjectRuleFor("ANTHROPIC_API_KEY")
	if again.Strip[0] == "corrupted" {
		t.Error("anthropicInjectRuleFor leaked a mutable alias into the package-level table")
	}
}

// TestProxyPackageHasNoAnthropicIdentifiers is the automated form of
// inject-claude-oauth-token-at-proxy.md's "Making the treadmill cheap" grep check: "the string
// anthropic must not appear anywhere under internal/proxy". Scoped to non-test .go files —
// internal/proxy's pre-existing tests already use "api.anthropic.com" as realistic-looking
// EXAMPLE data for genuinely vendor-agnostic policy/CA tests (e.g. proxy_internal_test.go's
// allowlist-matching table), predating this task and unrelated to it; rewriting those to a fake
// hostname would reduce their readability for no safety benefit, since they encode no Anthropic
// wire-format knowledge. What actually matters — and what this test enforces — is that no
// SHIPPED proxy code (the vendor boundary this task's design section requires) names Anthropic.
func TestProxyPackageHasNoAnthropicIdentifiers(t *testing.T) {
	dir := filepath.Join("..", "proxy")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	found := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		found = true
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(strings.ToLower(string(b)), "anthropic") {
			t.Errorf("%s mentions \"anthropic\" — vendor facts belong only in internal/adapter/anthropic_wire.go", name)
		}
	}
	if !found {
		t.Fatal("no non-test .go files found under internal/proxy — check the relative path still resolves")
	}
}
