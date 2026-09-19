package adapter_test

import (
	"reflect"
	"testing"

	"github.com/418-cloud/krayt/internal/adapter"
)

// placeholderFunc mirrors sandbox.SecretPlaceholder without importing internal/sandbox — the
// adapter package must stay msb-agnostic (seed-agent-first-run-config.md decision 8), and its
// tests shouldn't need that import either.
func placeholderFunc(key string) string { return "$MSB_" + key }

// TestClaudeCodeConfigSeeds covers seed-agent-first-run-config.md decision 9 for each of
// claude-code's three credentials.
func TestClaudeCodeConfigSeeds(t *testing.T) {
	ad, err := adapter.Get("claude-code")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("ANTHROPIC_API_KEY seeds the onboarding key and the approval", func(t *testing.T) {
		plan, err := ad.Prepare(adapter.Input{SecretKeys: []string{"ANTHROPIC_API_KEY"}, Placeholder: placeholderFunc})
		if err != nil {
			t.Fatal(err)
		}
		want := []adapter.ConfigSeed{{
			DirEnv: "CLAUDE_CONFIG_DIR",
			Path:   ".claude.json",
			Defaults: map[string]any{
				"hasCompletedOnboarding": true,
				"customApiKeyResponses": map[string]any{
					"approved": []any{"SB_ANTHROPIC_API_KEY"},
				},
			},
		}}
		if !reflect.DeepEqual(plan.ConfigSeeds, want) {
			t.Errorf("ConfigSeeds = %+v, want %+v", plan.ConfigSeeds, want)
		}
	})

	for _, cred := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"} {
		t.Run(cred+" seeds only the onboarding key", func(t *testing.T) {
			plan, err := ad.Prepare(adapter.Input{SecretKeys: []string{cred}, Placeholder: placeholderFunc})
			if err != nil {
				t.Fatal(err)
			}
			want := []adapter.ConfigSeed{{
				DirEnv:   "CLAUDE_CONFIG_DIR",
				Path:     ".claude.json",
				Defaults: map[string]any{"hasCompletedOnboarding": true},
			}}
			if !reflect.DeepEqual(plan.ConfigSeeds, want) {
				t.Errorf("%s: ConfigSeeds = %+v, want %+v", cred, plan.ConfigSeeds, want)
			}
		})
	}

	t.Run("nil Placeholder omits the approval seed but keeps the onboarding key", func(t *testing.T) {
		plan, err := ad.Prepare(adapter.Input{SecretKeys: []string{"ANTHROPIC_API_KEY"}})
		if err != nil {
			t.Fatal(err)
		}
		want := []adapter.ConfigSeed{{
			DirEnv:   "CLAUDE_CONFIG_DIR",
			Path:     ".claude.json",
			Defaults: map[string]any{"hasCompletedOnboarding": true},
		}}
		if !reflect.DeepEqual(plan.ConfigSeeds, want) {
			t.Errorf("ConfigSeeds = %+v, want %+v", plan.ConfigSeeds, want)
		}
	})
}

// TestGeminiCLIConfigSeeds covers seed-agent-first-run-config.md decision 10: the settings.json
// seed for both credentials, and the accompanying Plan.Env additions.
func TestGeminiCLIConfigSeeds(t *testing.T) {
	ad, err := adapter.Get("gemini-cli")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		cred         string
		wantAuthType string
		wantVertex   bool
	}{
		{"GEMINI_API_KEY", "gemini-api-key", false},
		{"GOOGLE_API_KEY", "vertex-ai", true},
	}
	for _, c := range cases {
		t.Run(c.cred, func(t *testing.T) {
			plan, err := ad.Prepare(adapter.Input{SecretKeys: []string{c.cred}})
			if err != nil {
				t.Fatal(err)
			}
			want := []adapter.ConfigSeed{{
				DirEnv: "GEMINI_CLI_HOME",
				Path:   ".gemini/settings.json",
				Defaults: map[string]any{
					"security": map[string]any{
						"auth": map[string]any{"selectedType": c.wantAuthType},
					},
				},
			}}
			if !reflect.DeepEqual(plan.ConfigSeeds, want) {
				t.Errorf("ConfigSeeds = %+v, want %+v", plan.ConfigSeeds, want)
			}
			if plan.Env["GEMINI_CLI_TRUST_WORKSPACE"] != "true" {
				t.Errorf("Env[GEMINI_CLI_TRUST_WORKSPACE] = %q, want true", plan.Env["GEMINI_CLI_TRUST_WORKSPACE"])
			}
			_, hasVertex := plan.Env["GOOGLE_GENAI_USE_VERTEXAI"]
			if hasVertex != c.wantVertex {
				t.Errorf("Env[GOOGLE_GENAI_USE_VERTEXAI] set = %v, want %v", hasVertex, c.wantVertex)
			}
		})
	}
}

// TestGeminiCLIEnvWithAndWithoutQuestionsWait proves GEMINI_CLI_TRUST_WORKSPACE is always set,
// and askEnv's KRAYT_ASK_SOCKET is merged alongside it rather than replacing it, in both modes
// (seed-agent-first-run-config.md decision 10).
func TestGeminiCLIEnvWithAndWithoutQuestionsWait(t *testing.T) {
	ad, err := adapter.Get("gemini-cli")
	if err != nil {
		t.Fatal(err)
	}

	waiting, err := ad.Prepare(adapter.Input{
		SecretKeys: []string{"GEMINI_API_KEY"}, QuestionsWait: true, AskSocket: "vsock://2:1026",
	})
	if err != nil {
		t.Fatal(err)
	}
	if waiting.Env["GEMINI_CLI_TRUST_WORKSPACE"] != "true" {
		t.Errorf("wait mode: GEMINI_CLI_TRUST_WORKSPACE missing; env = %v", waiting.Env)
	}
	if waiting.Env["KRAYT_ASK_SOCKET"] != "vsock://2:1026" {
		t.Errorf("wait mode: KRAYT_ASK_SOCKET missing; env = %v", waiting.Env)
	}

	failing, err := ad.Prepare(adapter.Input{SecretKeys: []string{"GEMINI_API_KEY"}, QuestionsWait: false})
	if err != nil {
		t.Fatal(err)
	}
	if failing.Env["GEMINI_CLI_TRUST_WORKSPACE"] != "true" {
		t.Errorf("fail mode: GEMINI_CLI_TRUST_WORKSPACE missing; env = %v", failing.Env)
	}
	if _, wired := failing.Env["KRAYT_ASK_SOCKET"]; wired {
		t.Errorf("fail mode: KRAYT_ASK_SOCKET should not be wired; env = %v", failing.Env)
	}
}

// TestOpenCodeAndNoneHaveNoConfigSeeds covers seed-agent-first-run-config.md decision 11.
func TestOpenCodeAndNoneHaveNoConfigSeeds(t *testing.T) {
	for _, name := range []string{"opencode", "none"} {
		ad, err := adapter.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := ad.Prepare(adapter.Input{
			SecretKeys:  []string{"ANTHROPIC_API_KEY"},
			Placeholder: placeholderFunc,
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(plan.ConfigSeeds) != 0 {
			t.Errorf("%s: ConfigSeeds = %+v, want none", name, plan.ConfigSeeds)
		}
	}
}
