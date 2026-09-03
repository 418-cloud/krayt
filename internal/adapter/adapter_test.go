package adapter_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/418-cloud/krayt/internal/adapter"
	"github.com/418-cloud/krayt/internal/task"
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

// TestGeminiAndNone covers the gemini-cli auth gate, its msb secret scoping, and the
// pass-through none adapter.
func TestGeminiAndNone(t *testing.T) {
	gem, _ := adapter.Get("gemini-cli")
	if _, err := gem.Prepare(adapter.Input{SecretKeys: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}}); err == nil {
		t.Error("gemini-cli: two credentials should be rejected")
	}
	p, err := gem.Prepare(adapter.Input{SecretKeys: []string{"GEMINI_API_KEY"}})
	if err != nil || p.Credential != "GEMINI_API_KEY" {
		t.Errorf("gemini-cli single cred: plan=%+v err=%v", p, err)
	}
	want := []task.SecretSpec{{Key: "GEMINI_API_KEY", Hosts: []string{"generativelanguage.googleapis.com"}}}
	if !reflect.DeepEqual(p.Secrets, want) {
		t.Errorf("gemini-cli Secrets = %+v, want %+v", p.Secrets, want)
	}

	n, _ := adapter.Get("none")
	// none imposes no auth rule — even with no secrets it prepares cleanly.
	if p, err := n.Prepare(adapter.Input{}); err != nil || p.Credential != "" {
		t.Errorf("none: plan=%+v err=%v", p, err)
	}
}

// TestOpenCodeExactlyOne is the §6.14 proof for the opencode adapter: it accepts exactly one of
// its three recognized credentials, fails fast when none or several are set, and scopes each
// credential to its own host (hand-secrets-to-msb.md).
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
		wantHost string
	}{
		{"anthropic only", []string{"ANTHROPIC_API_KEY", "GH_TOKEN"}, "", "ANTHROPIC_API_KEY", "api.anthropic.com"},
		{"openai only", []string{"OPENAI_API_KEY"}, "", "OPENAI_API_KEY", "api.openai.com"},
		{"openrouter only", []string{"OPENROUTER_API_KEY"}, "", "OPENROUTER_API_KEY", "openrouter.ai"},
		{"two set", []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"}, "exactly one", "", ""},
		{"all three set", []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY"}, "exactly one", "", ""},
		{"none set", []string{"GH_TOKEN"}, "no auth credential", "", ""},
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
				want := []task.SecretSpec{{Key: c.wantCred, Hosts: []string{c.wantHost}}}
				if !reflect.DeepEqual(plan.Secrets, want) {
					t.Errorf("Secrets = %+v, want %+v", plan.Secrets, want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, c.wantErr)
			}
		})
	}
}

// TestClaudeCodeSecretScoping proves the claude-code adapter returns exactly one Plan.Secrets
// entry naming the selected credential scoped to api.anthropic.com — the credential value never
// rides Plan.Env or any other channel (hand-secrets-to-msb.md; msb substitutes it at the host TLS
// boundary and sets the guest's own credential env var to its default placeholder itself).
func TestClaudeCodeSecretScoping(t *testing.T) {
	ad, err := adapter.Get("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	for _, cred := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"} {
		plan, err := ad.Prepare(adapter.Input{SecretKeys: []string{cred}})
		if err != nil {
			t.Fatalf("%s: %v", cred, err)
		}
		if plan.Credential != cred {
			t.Errorf("%s: Credential = %q, want %q", cred, plan.Credential, cred)
		}
		want := []task.SecretSpec{{Key: cred, Hosts: []string{"api.anthropic.com"}}}
		if !reflect.DeepEqual(plan.Secrets, want) {
			t.Errorf("%s: Secrets = %+v, want %+v", cred, plan.Secrets, want)
		}
		if _, set := plan.Env[cred]; set {
			t.Errorf("%s: the credential value must never ride Plan.Env; env = %v", cred, plan.Env)
		}
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
