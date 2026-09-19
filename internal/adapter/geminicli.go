package adapter

import "github.com/418-cloud/krayt/internal/task"

// geminiCLIAuthKeys are the credentials the Gemini CLI accepts; exactly one must be set so the
// run's billing/identity is unambiguous, mirroring the claude-code rule (§6.14). Both names
// authenticate against the same host.
var geminiCLIAuthKeys = []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}

// geminiCLIAPIHost is the one host either credential ever needs (hand-secrets-to-msb.md).
const geminiCLIAPIHost = "generativelanguage.googleapis.com"

// geminiCLIConfigDirEnv/geminiCLIConfigPath locate Gemini CLI's global settings
// (seed-agent-first-run-config.md evidence): Storage.getGlobalSettingsPath() joins homedir()
// (GEMINI_CLI_HOME if set, else os.homedir()) with ".gemini/settings.json".
const (
	geminiCLIConfigDirEnv = "GEMINI_CLI_HOME"
	geminiCLIConfigPath   = ".gemini/settings.json"
)

// Gemini CLI's AuthType values (seed-agent-first-run-config.md evidence): 'gemini-api-key'
// (USE_GEMINI) and 'vertex-ai' (USE_VERTEX_AI).
const (
	geminiAuthTypeAPIKey = "gemini-api-key"
	geminiAuthTypeVertex = "vertex-ai"
)

// geminiCLI is the Gemini adapter: same shape as claude-code (exactly-one auth + krayt-ask
// wiring + msb secret scoping), different credential names.
type geminiCLI struct{}

func (geminiCLI) Name() string { return "gemini-cli" }

func (geminiCLI) Prepare(in Input) (Plan, error) {
	cred, err := exactlyOne("gemini-cli", in.SecretKeys, geminiCLIAuthKeys)
	if err != nil {
		return Plan{}, err
	}

	env := askEnv(in)
	if env == nil {
		env = map[string]string{}
	}
	// Always on, regardless of credential (seed-agent-first-run-config.md decision 10): folder
	// trust is on by default and this is the only env var that bypasses the trust dialog — a
	// krayt shell session with no --skip-trust equivalent would otherwise still hit it even with
	// auth seeded.
	env["GEMINI_CLI_TRUST_WORKSPACE"] = "true"
	authType := geminiAuthTypeAPIKey
	if cred == "GOOGLE_API_KEY" {
		// GOOGLE_API_KEY authenticates through Vertex AI Express, which needs this flag set
		// (seed-agent-first-run-config.md evidence) — only the published entrypoint sets it
		// otherwise, and that never reaches a user-supplied image.
		env["GOOGLE_GENAI_USE_VERTEXAI"] = "true"
		authType = geminiAuthTypeVertex
	}

	return Plan{
		Env:        env,
		Credential: cred,
		Secrets:    []task.SecretSpec{{Key: cred, Hosts: []string{geminiCLIAPIHost}}},
		ConfigSeeds: []ConfigSeed{{
			DirEnv: geminiCLIConfigDirEnv,
			Path:   geminiCLIConfigPath,
			Defaults: map[string]any{
				"security": map[string]any{
					"auth": map[string]any{"selectedType": authType},
				},
			},
		}},
		// Inferred from Gemini CLI's own convention ($HOME/.gemini/tmp/<project-hash>/), not verified
		// against a real run. A wrong path costs a missing transcript, never a failed run.
		TranscriptDir: ".gemini/tmp",
	}, nil
}
