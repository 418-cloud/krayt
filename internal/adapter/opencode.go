package adapter

import "github.com/418-cloud/krayt/internal/task"

// openCodeAuthKeys are the credentials opencode's env-based provider auth accepts (verified
// against packages/llm/src/providers/{anthropic,openai,openrouter}.ts upstream); exactly one
// must be set so the run's billing/identity is unambiguous, mirroring claude-code/gemini-cli
// (§6.14). opencode is multi-provider (75+ via Models.dev) but this adapter deliberately covers
// only the three common setups — extend later if asked.
var openCodeAuthKeys = []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY"}

// openCodeAPIHosts maps each recognized credential to the one host it authenticates against
// (hand-secrets-to-msb.md) — unlike claude-code/gemini-cli, opencode's three credentials don't
// share a single host.
var openCodeAPIHosts = map[string]string{
	"ANTHROPIC_API_KEY":  "api.anthropic.com",
	"OPENAI_API_KEY":     "api.openai.com",
	"OPENROUTER_API_KEY": "openrouter.ai",
}

// openCode is the opencode adapter: same shape as claude-code/gemini-cli (exactly-one auth +
// krayt-ask wiring + msb secret scoping), different credential names/hosts.
type openCode struct{}

func (openCode) Name() string { return "opencode" }

func (openCode) Prepare(in Input) (Plan, error) {
	cred, err := exactlyOne("opencode", in.SecretKeys, openCodeAuthKeys)
	if err != nil {
		return Plan{}, err
	}
	return Plan{
		Env:        askEnv(in),
		Credential: cred,
		Secrets:    []task.SecretSpec{{Key: cred, Hosts: []string{openCodeAPIHosts[cred]}}},
	}, nil
}
