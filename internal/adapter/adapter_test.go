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
	for _, name := range []string{"none", "claude-code", "gemini-cli"} {
		ad, err := adapter.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		// A valid single credential so claude-code/gemini pass the auth gate.
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

func TestGetUnknown(t *testing.T) {
	if _, err := adapter.Get("clyde"); err == nil {
		t.Error("unknown adapter should error")
	}
	if _, err := adapter.Get(""); err != nil {
		t.Errorf("empty adapter should default to none: %v", err)
	}
}
