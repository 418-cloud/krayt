package cli

import (
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/418-cloud/krayt/internal/sandbox"
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
// credential set before any sandbox is created (§6.14).
func TestApplyAdapterAuthGate(t *testing.T) {
	spec := &task.RunSpec{Questions: task.QuestionsPolicy{Mode: task.QuestionFail}}
	err := applyAdapter(io.Discard, spec, "claude-code", keySet("ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"))
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("expected exactly-one auth error; got %v", err)
	}
}

// TestApplyAdapterWiresAsk proves a valid single credential passes and, in wait mode, the
// krayt-ask front-end is wired into the container env with the msb vsock socket value (§6.13),
// without clobbering user env.
func TestApplyAdapterWiresAsk(t *testing.T) {
	spec := &task.RunSpec{
		Env:       map[string]string{"LOG_LEVEL": "debug"},
		Questions: task.QuestionsPolicy{Mode: task.QuestionWait},
	}
	if err := applyAdapter(io.Discard, spec, "claude-code", keySet("ANTHROPIC_API_KEY")); err != nil {
		t.Fatalf("applyAdapter: %v", err)
	}
	if spec.Env["KRAYT_ASK_SOCKET"] != sandbox.AskSocketEnv {
		t.Errorf("KRAYT_ASK_SOCKET = %q, want %q", spec.Env["KRAYT_ASK_SOCKET"], sandbox.AskSocketEnv)
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

// TestApplyAdapterScopesCredential proves applyAdapter merges the adapter's msb secret scope
// (hand-secrets-to-msb.md) into spec.Network.Secrets, and never sets the credential's value —
// only its name — anywhere in spec.Env.
func TestApplyAdapterScopesCredential(t *testing.T) {
	spec := &task.RunSpec{
		Questions: task.QuestionsPolicy{Mode: task.QuestionFail},
		Network:   task.NetworkPolicy{Mode: task.NetworkAllowlist, Allow: []string{"api.anthropic.com"}},
	}
	if err := applyAdapter(io.Discard, spec, "claude-code", keySet("ANTHROPIC_API_KEY")); err != nil {
		t.Fatalf("applyAdapter: %v", err)
	}
	want := []task.SecretSpec{{Key: "ANTHROPIC_API_KEY", Hosts: []string{"api.anthropic.com"}}}
	if !reflect.DeepEqual(spec.Network.Secrets, want) {
		t.Errorf("Network.Secrets = %+v, want %+v", spec.Network.Secrets, want)
	}
	if _, set := spec.Env["ANTHROPIC_API_KEY"]; set {
		t.Errorf("applyAdapter must never put a credential's value in spec.Env; env = %v", spec.Env)
	}
}

// TestApplyAdapterMergePrecedenceUserWins proves the merge rule: a user-written network.inject
// scope for the SAME credential the adapter would also scope wins outright, and the override is
// logged (task.MergeSecretSpecs).
func TestApplyAdapterMergePrecedenceUserWins(t *testing.T) {
	spec := &task.RunSpec{
		Questions: task.QuestionsPolicy{Mode: task.QuestionFail},
		Network: task.NetworkPolicy{
			Mode: task.NetworkAllowlist, Allow: []string{"api.anthropic.com", "my-proxy.example.com"},
			Secrets: []task.SecretSpec{{Key: "ANTHROPIC_API_KEY", Hosts: []string{"my-proxy.example.com"}}},
		},
	}
	var logbuf bytes.Buffer
	if err := applyAdapter(&logbuf, spec, "claude-code", keySet("ANTHROPIC_API_KEY")); err != nil {
		t.Fatalf("applyAdapter: %v", err)
	}
	if len(spec.Network.Secrets) != 1 {
		t.Fatalf("want the user's own scope to win outright, got %+v", spec.Network.Secrets)
	}
	if got := spec.Network.Secrets[0].Hosts; len(got) != 1 || got[0] != "my-proxy.example.com" {
		t.Errorf("hosts = %v, want the user's own my-proxy.example.com to win", got)
	}
	if !strings.Contains(logbuf.String(), "ANTHROPIC_API_KEY") {
		t.Errorf("override was not logged: %q", logbuf.String())
	}
}

// TestApplyAdapterUnallowlistedHostFailsPreflight proves an adapter-scoped credential is held to
// exactly the same pre-flight standard as a hand-written one: if the user's `network.allow`
// doesn't name the host the adapter wants to scope, the merged set fails
// ValidateNetworkPolicyForMsb before any sandbox work, the same way a typo'd hand-written scope
// would.
func TestApplyAdapterUnallowlistedHostFailsPreflight(t *testing.T) {
	spec := &task.RunSpec{
		Questions: task.QuestionsPolicy{Mode: task.QuestionFail},
		Network:   task.NetworkPolicy{Mode: task.NetworkAllowlist, Allow: []string{"example.com"}},
	}
	if err := applyAdapter(io.Discard, spec, "claude-code", keySet("ANTHROPIC_API_KEY")); err != nil {
		t.Fatalf("applyAdapter: %v", err)
	}
	err := task.ValidateNetworkPolicyForMsb(spec.Network, keySet("ANTHROPIC_API_KEY"), secretsToInjectRules(spec.Network.Secrets))
	if err == nil || !strings.Contains(err.Error(), "must also be in allow") {
		t.Fatalf("want a not-in-allow pre-flight error for the adapter's own host; got %v", err)
	}
}

// TestApplyAdapterMITMIsHardErroredByValidation proves network.mitm — kept only so a config that
// still sets it is caught rather than silently dropped — passes cleanly through applyAdapter (it
// is not this function's job to reject it) but fails the subsequent msb pre-flight validation.
func TestApplyAdapterMITMIsHardErroredByValidation(t *testing.T) {
	spec := &task.RunSpec{
		Questions: task.QuestionsPolicy{Mode: task.QuestionFail},
		Network:   task.NetworkPolicy{Mode: task.NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true},
	}
	if err := applyAdapter(io.Discard, spec, "claude-code", keySet("ANTHROPIC_API_KEY")); err != nil {
		t.Fatalf("applyAdapter: %v", err)
	}
	err := task.ValidateNetworkPolicyForMsb(spec.Network, keySet("ANTHROPIC_API_KEY"), secretsToInjectRules(spec.Network.Secrets))
	if err == nil || !strings.Contains(err.Error(), "mitm") {
		t.Fatalf("want network.mitm to be hard-errored; got %v", err)
	}
}

// TestApplyAdapterRecordsTranscriptDir: the adapter owns the in-guest path (it is the only thing
// that knows which agent runs), and applyAdapter is where it reaches the spec. Recorded
// unconditionally here — the --transcript gate lives at the call site, so this must not
// second-guess it.
func TestApplyAdapterRecordsTranscriptDir(t *testing.T) {
	spec := &task.RunSpec{}
	if err := applyAdapter(io.Discard, spec, "claude-code", keySet("ANTHROPIC_API_KEY")); err != nil {
		t.Fatalf("applyAdapter: %v", err)
	}
	if spec.TranscriptDir != ".claude/projects" {
		t.Errorf("TranscriptDir = %q, want the claude-code adapter's path", spec.TranscriptDir)
	}

	// `none` declares no path, so a run with no adapter can never capture one.
	bare := &task.RunSpec{}
	if err := applyAdapter(io.Discard, bare, "none", keySet()); err != nil {
		t.Fatalf("applyAdapter(none): %v", err)
	}
	if bare.TranscriptDir != "" {
		t.Errorf("TranscriptDir = %q for adapter none, want empty", bare.TranscriptDir)
	}
}
