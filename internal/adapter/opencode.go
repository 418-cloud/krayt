package adapter

// openCodeAuthKeys are the credentials opencode's env-based provider auth accepts (verified
// against packages/llm/src/providers/{anthropic,openai,openrouter}.ts upstream); exactly one
// must be set so the run's billing/identity is unambiguous, mirroring claude-code/gemini-cli
// (§6.14). opencode is multi-provider (75+ via Models.dev) but this adapter deliberately covers
// only the three common setups — extend later if asked.
var openCodeAuthKeys = []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY"}

// openCode is the opencode adapter: same shape as claude-code/gemini-cli (exactly-one auth +
// krayt-ask wiring), different credential names.
type openCode struct{}

func (openCode) Name() string { return "opencode" }

func (openCode) Prepare(in Input) (Plan, error) {
	cred, err := exactlyOne("opencode", in.SecretKeys, openCodeAuthKeys)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Env: askEnv(in), Credential: cred}, nil
}
