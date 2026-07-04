package adapter

// claudeCodeAuthKeys are the auth credentials Claude Code accepts, in the §6.14 precedence
// order krayt surfaces in errors. Exactly one must be set: with both ANTHROPIC_API_KEY and
// CLAUDE_CODE_OAUTH_TOKEN present the API key silently wins and the subscription is bypassed
// (billed as API usage), so krayt refuses the ambiguous combination rather than picking for the
// user. Cloud-provider auth (CLAUDE_CODE_USE_BEDROCK/_VERTEX/_FOUNDRY) is out of scope here.
var claudeCodeAuthKeys = []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"}

// claudeCode is the worked-example adapter (§6.14): it enforces the exactly-one auth rule and
// wires the krayt-ask front-end. The credential rides the per-task secrets bundle like any
// other secret; the container entrypoint exports it from /run/secrets into the environment.
type claudeCode struct{}

func (claudeCode) Name() string { return "claude-code" }

func (claudeCode) Prepare(in Input) (Plan, error) {
	cred, err := exactlyOne("claude-code", in.SecretKeys, claudeCodeAuthKeys)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Env: askEnv(in), Credential: cred}, nil
}
