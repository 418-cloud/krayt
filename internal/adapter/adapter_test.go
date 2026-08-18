package adapter_test

import (
	"strings"
	"testing"

	"github.com/418-cloud/krayt/internal/adapter"
)

const askSocket = "/run/krayt/ask.sock"

// TestClaudeCodeExactlyOne is the §6.14 proof: the claude-code adapter accepts exactly one
// auth credential, and fails fast when none or both are set (the silent-billing trap).
func TestClaudeCodeExactlyOne(t *testing.T) {
	ad, err := adapter.Get("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		keys     []string
		wantErr  string // substring; "" = success
		wantCred string
	}{
		{"api key only", []string{"ANTHROPIC_API_KEY", "GH_TOKEN"}, "", "ANTHROPIC_API_KEY"},
		{"oauth only", []string{"CLAUDE_CODE_OAUTH_TOKEN"}, "", "CLAUDE_CODE_OAUTH_TOKEN"},
		{"both set", []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}, "exactly one", ""},
		{"none set", []string{"GH_TOKEN"}, "no auth credential", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan, err := ad.Prepare(adapter.Input{SecretKeys: c.keys})
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if plan.Credential != c.wantCred {
					t.Errorf("credential = %q, want %q", plan.Credential, c.wantCred)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, c.wantErr)
			}
		})
	}
}

// TestAskWiring checks that the krayt-ask front-end is wired (KRAYT_ASK_SOCKET) only when the
// run pauses for questions, across every adapter (§6.13).
func TestAskWiring(t *testing.T) {
	for _, name := range []string{"none", "claude-code", "gemini-cli", "opencode"} {
		ad, err := adapter.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		// A valid single credential so claude-code/gemini/opencode pass the auth gate.
		// ANTHROPIC_API_KEY alone satisfies opencode too (it's one of its recognized keys), so
		// this doesn't need a fourth key.
		keys := []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY"}

		waiting, err := ad.Prepare(adapter.Input{SecretKeys: keys, QuestionsWait: true, AskSocket: askSocket})
		if err != nil {
			t.Fatalf("%s wait: %v", name, err)
		}
		if waiting.Env["KRAYT_ASK_SOCKET"] != askSocket {
			t.Errorf("%s: wait should wire KRAYT_ASK_SOCKET; env = %v", name, waiting.Env)
		}

		fail, err := ad.Prepare(adapter.Input{SecretKeys: keys, QuestionsWait: false, AskSocket: askSocket})
		if err != nil {
			t.Fatalf("%s fail: %v", name, err)
		}
		if _, wired := fail.Env["KRAYT_ASK_SOCKET"]; wired {
			t.Errorf("%s: fail mode should not wire the front-end; env = %v", name, fail.Env)
		}
	}
}

// TestGeminiAndNone covers the gemini-cli auth gate and the pass-through none adapter.
func TestGeminiAndNone(t *testing.T) {
	gem, _ := adapter.Get("gemini-cli")
	if _, err := gem.Prepare(adapter.Input{SecretKeys: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}}); err == nil {
		t.Error("gemini-cli: two credentials should be rejected")
	}
	p, err := gem.Prepare(adapter.Input{SecretKeys: []string{"GEMINI_API_KEY"}})
	if err != nil || p.Credential != "GEMINI_API_KEY" {
		t.Errorf("gemini-cli single cred: plan=%+v err=%v", p, err)
	}

	n, _ := adapter.Get("none")
	// none imposes no auth rule — even with no secrets it prepares cleanly.
	if p, err := n.Prepare(adapter.Input{}); err != nil || p.Credential != "" {
		t.Errorf("none: plan=%+v err=%v", p, err)
	}
}

// TestOpenCodeExactlyOne is the §6.14 proof for the opencode adapter: it accepts exactly one of
// its three recognized credentials, and fails fast when none or several are set.
func TestOpenCodeExactlyOne(t *testing.T) {
	ad, err := adapter.Get("opencode")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		keys     []string
		wantErr  string // substring; "" = success
		wantCred string
	}{
		{"anthropic only", []string{"ANTHROPIC_API_KEY", "GH_TOKEN"}, "", "ANTHROPIC_API_KEY"},
		{"openai only", []string{"OPENAI_API_KEY"}, "", "OPENAI_API_KEY"},
		{"openrouter only", []string{"OPENROUTER_API_KEY"}, "", "OPENROUTER_API_KEY"},
		{"two set", []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"}, "exactly one", ""},
		{"all three set", []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY"}, "exactly one", ""},
		{"none set", []string{"GH_TOKEN"}, "no auth credential", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan, err := ad.Prepare(adapter.Input{SecretKeys: c.keys})
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if plan.Credential != c.wantCred {
					t.Errorf("credential = %q, want %q", plan.Credential, c.wantCred)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, c.wantErr)
			}
		})
	}
}

// TestCredentialInjectionSignal proves that when the adapter's selected credential is withheld
// from SecretsBundle by network.inject (§6.6.1), Prepare wires KRAYT_INJECTED_CREDENTIAL naming
// it — so the container entrypoint can start without the /run/secrets file that will never
// arrive — and that it is absent when injection isn't configured for that key.
func TestCredentialInjectionSignal(t *testing.T) {
	ad, err := adapter.Get("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{"ANTHROPIC_API_KEY"}

	injected, err := ad.Prepare(adapter.Input{SecretKeys: keys, InjectedKeys: map[string]bool{"ANTHROPIC_API_KEY": true}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if injected.Env["KRAYT_INJECTED_CREDENTIAL"] != "ANTHROPIC_API_KEY" {
		t.Errorf("KRAYT_INJECTED_CREDENTIAL = %q, want ANTHROPIC_API_KEY; env = %v", injected.Env["KRAYT_INJECTED_CREDENTIAL"], injected.Env)
	}

	notInjected, err := ad.Prepare(adapter.Input{SecretKeys: keys})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, set := notInjected.Env["KRAYT_INJECTED_CREDENTIAL"]; set {
		t.Errorf("KRAYT_INJECTED_CREDENTIAL should be unset without network.inject; env = %v", notInjected.Env)
	}

	// A different key injected (not the one actually selected) must not wire the signal.
	other, err := ad.Prepare(adapter.Input{SecretKeys: keys, InjectedKeys: map[string]bool{"SOME_OTHER_KEY": true}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, set := other.Env["KRAYT_INJECTED_CREDENTIAL"]; set {
		t.Errorf("KRAYT_INJECTED_CREDENTIAL should only name the SELECTED credential; env = %v", other.Env)
	}
}

// TestMITMShapeTranslationPlaceholderMirrorsTheCredential is the thesis test for SHAPE MIRRORING
// (internal/adapter/anthropic_wire.go's PROVENANCE, owner decision 2026-08-18): every credential
// shape produces a placeholder delivered under ITS OWN env var, never translated into another
// shape's variable. That is what makes the agent run its own code path for the credential the user
// actually supplied — so krayt never has to reproduce the request that path would have built.
//
// It iterates every credential claude-code recognizes rather than hardcoding the observed ones, so
// a future probe that adds a table entry gets this property asserted for free.
func TestMITMShapeTranslationPlaceholderMirrorsTheCredential(t *testing.T) {
	ad, err := adapter.Get("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	for _, cred := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		plan, err := ad.Prepare(adapter.Input{SecretKeys: []string{cred}, MITM: true})
		if err != nil {
			t.Fatalf("%s: %v", cred, err)
		}
		if len(plan.Inject) == 0 {
			// No wire rule for this shape yet — correctly falls back to SecretsBundle, and there is
			// no placeholder to assert anything about.
			continue
		}
		if len(plan.Placeholders) != 1 {
			t.Errorf("%s: Placeholders = %v, want exactly one entry", cred, plan.Placeholders)
		}
		got, ok := plan.Placeholders[cred]
		if !ok {
			t.Errorf("%s: Placeholders = %v, want the placeholder under the credential's OWN name", cred, plan.Placeholders)
			continue
		}
		if got == "" {
			t.Errorf("%s: empty placeholder value", cred)
		}
		// A placeholder that looked like the real thing would be indistinguishable from a leak in a
		// log; a human who finds one must be able to tell immediately (§3).
		if !strings.Contains(got, "krayt-placeholder-do-not-use") {
			t.Errorf("%s: placeholder %q is not self-describing", cred, got)
		}
		// The container is configured for exactly one credential, so the entrypoint's exactly-one
		// selection can never see two and pick the wrong one.
		for other := range plan.Placeholders {
			if other != cred {
				t.Errorf("%s: placeholder also configures %q", cred, other)
			}
		}
	}
}

// TestMITMShapeTranslationRequiresMITM proves the observed-shape injection path only ever
// activates when network.mitm is actually on — the same credential with in.MITM false must fall
// back to plain SecretsBundle delivery (mitm:false byte-identical regression).
func TestMITMShapeTranslationRequiresMITM(t *testing.T) {
	ad, err := adapter.Get("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := ad.Prepare(adapter.Input{SecretKeys: []string{"ANTHROPIC_API_KEY"}, MITM: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Inject) != 0 || len(plan.Placeholders) != 0 {
		t.Errorf("mitm:false must not translate; plan = %+v", plan)
	}
}

func TestGetUnknown(t *testing.T) {
	if _, err := adapter.Get("clyde"); err == nil {
		t.Error("unknown adapter should error")
	}
	if _, err := adapter.Get(""); err != nil {
		t.Errorf("empty adapter should default to none: %v", err)
	}
}
