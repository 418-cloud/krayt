package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/418-cloud/krayt/internal/adapter"
	"github.com/418-cloud/krayt/internal/guest"
	"github.com/418-cloud/krayt/internal/task"
)

// keySet is the adapter_test.go helper mirroring loadSecretKeySet, for tests that build a
// secretKeys map by hand instead of round-tripping a real secrets file on disk.
func keySet(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// TestApplyAdapterAuthGate proves the run's host-side pre-flight rejects an ambiguous
// credential set before any VM boots (§6.14). Also covers inject-claude-oauth-token-at-proxy.md's
// requirement that exactlyOne still fires when the credential would be host-only (delivered by
// injection, never SecretsBundle) — the check runs on secretKeys regardless of network.mitm.
func TestApplyAdapterAuthGate(t *testing.T) {
	spec := &task.RunSpec{
		Questions: task.QuestionsPolicy{Mode: task.QuestionFail},
		Network:   task.NetworkPolicy{MITM: true},
	}
	err := applyAdapter(io.Discard, spec, "claude-code", keySet("ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"))
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("expected exactly-one auth error; got %v", err)
	}
}

// TestApplyAdapterWiresAsk proves a valid single credential passes and, in wait mode, the
// krayt-ask front-end is wired into the container env (§6.13), without clobbering user env.
func TestApplyAdapterWiresAsk(t *testing.T) {
	spec := &task.RunSpec{
		Env:       map[string]string{"LOG_LEVEL": "debug"},
		Questions: task.QuestionsPolicy{Mode: task.QuestionWait},
	}
	if err := applyAdapter(io.Discard, spec, "claude-code", keySet("ANTHROPIC_API_KEY")); err != nil {
		t.Fatalf("applyAdapter: %v", err)
	}
	if spec.Env["KRAYT_ASK_SOCKET"] != guest.ContainerAskSocket {
		t.Errorf("KRAYT_ASK_SOCKET = %q, want %q", spec.Env["KRAYT_ASK_SOCKET"], guest.ContainerAskSocket)
	}
	if spec.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("adapter clobbered user env: %v", spec.Env)
	}

	// fail mode: no front-end wiring.
	spec2 := &task.RunSpec{Questions: task.QuestionsPolicy{Mode: task.QuestionFail}}
	if err := applyAdapter(io.Discard, spec2, "claude-code", keySet("ANTHROPIC_API_KEY")); err != nil {
		t.Fatalf("applyAdapter fail-mode: %v", err)
	}
	if _, wired := spec2.Env["KRAYT_ASK_SOCKET"]; wired {
		t.Errorf("fail mode should not wire krayt-ask; env = %v", spec2.Env)
	}
}

// TestApplyAdapterMITMOff is the mitm:false regression (inject-claude-oauth-token-at-proxy.md
// "Done when"): with network.mitm off, applyAdapter never touches spec.Network.Inject even though
// the observed ANTHROPIC_API_KEY shape exists — byte-identical to a build with no shape-translation
// feature at all.
func TestApplyAdapterMITMOff(t *testing.T) {
	spec := &task.RunSpec{Questions: task.QuestionsPolicy{Mode: task.QuestionFail}}
	if err := applyAdapter(io.Discard, spec, "claude-code", keySet("ANTHROPIC_API_KEY")); err != nil {
		t.Fatalf("applyAdapter: %v", err)
	}
	if len(spec.Network.Inject) != 0 {
		t.Errorf("mitm:false must not add an inject rule; got %+v", spec.Network.Inject)
	}
	if spec.Env["ANTHROPIC_API_KEY"] != "" {
		t.Errorf("mitm:false must not set a placeholder into spec.Env; got %q", spec.Env["ANTHROPIC_API_KEY"])
	}
}

// TestApplyAdapterMITMInjectsObservedShape proves the load-bearing behavior of
// inject-claude-oauth-token-at-proxy.md: with mitm on and no user-written network.inject, the
// claude-code adapter alone produces a complete, pre-flight-valid injection rule for the one
// credential shape anthropic_wire.go actually has an observation for (ANTHROPIC_API_KEY) — "the
// user configures nothing beyond mitm: true and the secret itself."
func TestApplyAdapterMITMInjectsObservedShape(t *testing.T) {
	spec := &task.RunSpec{
		Questions: task.QuestionsPolicy{Mode: task.QuestionFail},
		Network: task.NetworkPolicy{
			Mode: task.NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true,
		},
	}
	if err := applyAdapter(io.Discard, spec, "claude-code", keySet("ANTHROPIC_API_KEY")); err != nil {
		t.Fatalf("applyAdapter: %v", err)
	}
	if len(spec.Network.Inject) != 1 {
		t.Fatalf("want exactly one inject rule, got %+v", spec.Network.Inject)
	}
	rule := spec.Network.Inject[0]
	if rule.Host != "api.anthropic.com" || rule.Set["x-api-key"] != "ANTHROPIC_API_KEY" {
		t.Errorf("unexpected inject rule: %+v", rule)
	}
	if err := task.ValidateNetworkPolicy(spec.Network, keySet("ANTHROPIC_API_KEY")); err != nil {
		t.Errorf("adapter-produced rule failed re-validation: %v", err)
	}
	// The container-facing credential is the placeholder, delivered as plain env — never a real
	// value, never routed through SecretsBundle (that's InjectedSecretKeys' job downstream).
	if got := spec.Env["ANTHROPIC_API_KEY"]; got != adapter.AnthropicPlaceholderAPIKey {
		t.Errorf("spec.Env[ANTHROPIC_API_KEY] = %q, want the placeholder %q", got, adapter.AnthropicPlaceholderAPIKey)
	}
	// Names the withheld credential for an entrypoint that predates the already-set-env-var branch
	// (§8.2) — without it those images exit 78 before the agent starts.
	if spec.Env["KRAYT_INJECTED_CREDENTIAL"] != "ANTHROPIC_API_KEY" {
		t.Errorf("KRAYT_INJECTED_CREDENTIAL = %q, want ANTHROPIC_API_KEY; env = %v",
			spec.Env["KRAYT_INJECTED_CREDENTIAL"], spec.Env)
	}
}

// TestApplyAdapterMITMSubscriptionTokenTranslatesShape is the end-to-end form of the 2026-08-17
// probe result (internal/adapter/anthropic_wire.go's PROVENANCE): a CLAUDE_CODE_OAUTH_TOKEN secret
// under mitm:true now produces the OAuth wire rule — Bearer on authorization, with the token forwarded
// verbatim — while the CONTAINER sees an OAuth-shaped placeholder under CLAUDE_CODE_OAUTH_TOKEN, the
// same variable the user supplied (SHAPE MIRRORING). That is the §6.14 claim this whole task exists
// for: the real credential never rides SecretsBundle, and Claude Code runs its own OAuth code path.
func TestApplyAdapterMITMSubscriptionTokenTranslatesShape(t *testing.T) {
	spec := &task.RunSpec{
		Questions: task.QuestionsPolicy{Mode: task.QuestionFail},
		Network: task.NetworkPolicy{
			Mode: task.NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true,
		},
	}
	if err := applyAdapter(io.Discard, spec, "claude-code", keySet("CLAUDE_CODE_OAUTH_TOKEN")); err != nil {
		t.Fatalf("applyAdapter: %v", err)
	}
	if len(spec.Network.Inject) != 1 {
		t.Fatalf("want one adapter-produced inject rule, got %+v", spec.Network.Inject)
	}
	rule := spec.Network.Inject[0]
	if rule.Set["authorization"] != "CLAUDE_CODE_OAUTH_TOKEN" || rule.SetPrefix["authorization"] != "Bearer " {
		t.Errorf("unexpected auth translation: Set=%v SetPrefix=%v", rule.Set, rule.SetPrefix)
	}
	if err := task.ValidateNetworkPolicy(spec.Network, keySet("CLAUDE_CODE_OAUTH_TOKEN")); err != nil {
		t.Errorf("adapter-produced rule failed re-validation: %v", err)
	}
	// The token is withheld from the guest bundle entirely, and the container is configured
	// OAuth-shaped with the placeholder under its own env var (SHAPE MIRRORING, not API-key-shaped).
	if !spec.Network.InjectedSecretKeys()["CLAUDE_CODE_OAUTH_TOKEN"] {
		t.Error("the OAuth token must be withheld from SecretsBundle once it is injected host-side")
	}
	// Shape mirroring: the container is configured with the SAME variable the user supplied,
	// carrying a placeholder — that is what makes Claude Code run its subscription code path.
	if got := spec.Env["CLAUDE_CODE_OAUTH_TOKEN"]; got != "sk-ant-oat01-krayt-placeholder-do-not-use" {
		t.Errorf("spec.Env[CLAUDE_CODE_OAUTH_TOKEN] = %q, want the OAuth placeholder", got)
	}
	if _, wrong := spec.Env["ANTHROPIC_API_KEY"]; wrong {
		t.Errorf("an OAuth run must not configure ANTHROPIC_API_KEY; env = %v", spec.Env)
	}
	// Without this the published entrypoints find no /run/secrets file, conclude they have no
	// credential, and exit 78 before the agent starts (§8.2).
	if spec.Env["KRAYT_INJECTED_CREDENTIAL"] != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Errorf("KRAYT_INJECTED_CREDENTIAL = %q, want CLAUDE_CODE_OAUTH_TOKEN; env = %v",
			spec.Env["KRAYT_INJECTED_CREDENTIAL"], spec.Env)
	}
}

// TestApplyAdapterNoMITMSubscriptionTokenUnchanged is the other half: with mitm off, the OAuth
// token still rides SecretsBundle exactly as it did before shape translation existed. Every run
// that does not opt into mitm must be byte-identical to the pre-task behavior.
func TestApplyAdapterNoMITMSubscriptionTokenUnchanged(t *testing.T) {
	spec := &task.RunSpec{
		Questions: task.QuestionsPolicy{Mode: task.QuestionFail},
		Network:   task.NetworkPolicy{Mode: task.NetworkAllowlist},
	}
	if err := applyAdapter(io.Discard, spec, "claude-code", keySet("CLAUDE_CODE_OAUTH_TOKEN")); err != nil {
		t.Fatalf("applyAdapter: %v", err)
	}
	if len(spec.Network.Inject) != 0 {
		t.Errorf("mitm:false must produce no inject rule; got %+v", spec.Network.Inject)
	}
	if spec.Env["ANTHROPIC_API_KEY"] == adapter.AnthropicPlaceholderAPIKey {
		t.Error("mitm:false must not configure the container with the injection placeholder")
	}
}

// TestApplyAdapterMergePrecedenceUserWins proves §4's merge rule: a user-written network.inject
// rule for the SAME host+header the adapter would also set wins, and the override is logged.
func TestApplyAdapterMergePrecedenceUserWins(t *testing.T) {
	spec := &task.RunSpec{
		Questions: task.QuestionsPolicy{Mode: task.QuestionFail},
		Network: task.NetworkPolicy{
			Mode: task.NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true,
			Inject: []task.InjectRule{{
				Host: "api.anthropic.com",
				Set:  map[string]string{"x-api-key": "MY_OWN_KEY"},
			}},
		},
	}
	var logbuf bytes.Buffer
	if err := applyAdapter(&logbuf, spec, "claude-code", keySet("ANTHROPIC_API_KEY", "MY_OWN_KEY")); err != nil {
		t.Fatalf("applyAdapter: %v", err)
	}
	if len(spec.Network.Inject) != 1 {
		t.Fatalf("want the two host-matching rules merged into one, got %+v", spec.Network.Inject)
	}
	if got := spec.Network.Inject[0].Set["x-api-key"]; got != "MY_OWN_KEY" {
		t.Errorf("x-api-key = %q, want the user's own MY_OWN_KEY to win", got)
	}
	if !strings.Contains(logbuf.String(), "x-api-key") || !strings.Contains(logbuf.String(), "api.anthropic.com") {
		t.Errorf("override was not logged: %q", logbuf.String())
	}
}

// TestApplyAdapterUnallowlistedHostFailsPreflight proves an adapter rule is held to exactly the
// same pre-flight standard as a hand-written one (§4): if the user's `network.allow` doesn't name
// the host the adapter wants to inject on, the merged set fails ValidateNetworkPolicy before any
// VM work, the same way a typo'd hand-written rule would.
func TestApplyAdapterUnallowlistedHostFailsPreflight(t *testing.T) {
	spec := &task.RunSpec{
		Questions: task.QuestionsPolicy{Mode: task.QuestionFail},
		Network:   task.NetworkPolicy{Mode: task.NetworkAllowlist, Allow: []string{"example.com"}, MITM: true},
	}
	if err := applyAdapter(io.Discard, spec, "claude-code", keySet("ANTHROPIC_API_KEY")); err != nil {
		t.Fatalf("applyAdapter: %v", err)
	}
	err := task.ValidateNetworkPolicy(spec.Network, keySet("ANTHROPIC_API_KEY"))
	if err == nil || !strings.Contains(err.Error(), "must also be in allow") {
		t.Fatalf("want a not-in-allow pre-flight error for the adapter's own host; got %v", err)
	}
}
