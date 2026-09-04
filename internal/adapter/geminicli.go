package adapter

import "github.com/418-cloud/krayt/internal/task"

// geminiCLIAuthKeys are the credentials the Gemini CLI accepts; exactly one must be set so the
// run's billing/identity is unambiguous, mirroring the claude-code rule (§6.14). Both names
// authenticate against the same host.
var geminiCLIAuthKeys = []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}

// geminiCLIAPIHost is the one host either credential ever needs (hand-secrets-to-msb.md).
const geminiCLIAPIHost = "generativelanguage.googleapis.com"

// geminiCLI is the Gemini adapter: same shape as claude-code (exactly-one auth + krayt-ask
// wiring + msb secret scoping), different credential names.
type geminiCLI struct{}

func (geminiCLI) Name() string { return "gemini-cli" }

func (geminiCLI) Prepare(in Input) (Plan, error) {
	cred, err := exactlyOne("gemini-cli", in.SecretKeys, geminiCLIAuthKeys)
	if err != nil {
		return Plan{}, err
	}
	return Plan{
		Env:        askEnv(in),
		Credential: cred,
		Secrets:    []task.SecretSpec{{Key: cred, Hosts: []string{geminiCLIAPIHost}}},
		// Inferred from Gemini CLI's own convention ($HOME/.gemini/tmp/<project-hash>/), not verified
		// against a real run. A wrong path costs a missing transcript, never a failed run.
		TranscriptDir: ".gemini/tmp",
	}, nil
}
