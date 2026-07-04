package adapter

// geminiCLIAuthKeys are the credentials the Gemini CLI accepts; exactly one must be set so the
// run's billing/identity is unambiguous, mirroring the claude-code rule (§6.14).
var geminiCLIAuthKeys = []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}

// geminiCLI is the Gemini adapter: same shape as claude-code (exactly-one auth + krayt-ask
// wiring), different credential names.
type geminiCLI struct{}

func (geminiCLI) Name() string { return "gemini-cli" }

func (geminiCLI) Prepare(in Input) (Plan, error) {
	cred, err := exactlyOne("gemini-cli", in.SecretKeys, geminiCLIAuthKeys)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Env: askEnv(in), Credential: cred}, nil
}
