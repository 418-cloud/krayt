package adapter

import "github.com/418-cloud/krayt/internal/task"

// claudeCodeAuthKeys are the auth credentials Claude Code accepts, in the §6.14 precedence
// order krayt surfaces in errors. Exactly one must be set: with both ANTHROPIC_API_KEY and
// CLAUDE_CODE_OAUTH_TOKEN present the API key silently wins and the subscription is bypassed
// (billed as API usage), so krayt refuses the ambiguous combination rather than picking for the
// user. Under `mitm: true` with an observed wire shape (anthropic_wire.go), this same check also
// prevents an ambiguous *injection rule* — Prepare only ever has one selected credential to
// translate, never two competing ones. Cloud-provider auth
// (CLAUDE_CODE_USE_BEDROCK/_VERTEX/_FOUNDRY) is out of scope here.
var claudeCodeAuthKeys = []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"}

// claudeCode is the worked-example adapter (§6.14): it enforces the exactly-one auth rule and
// wires the krayt-ask front-end. By default the credential rides the per-task secrets bundle like
// any other secret; the container entrypoint exports it from /run/secrets into the environment.
// When network.mitm is on AND the selected credential's wire shape has been observed
// (anthropic_wire.go), Prepare instead emits an injection rule and a placeholder — see
// inject-claude-oauth-token-at-proxy.md.
type claudeCode struct{}

func (claudeCode) Name() string { return "claude-code" }

func (claudeCode) Prepare(in Input) (Plan, error) {
	cred, err := exactlyOne("claude-code", in.SecretKeys, claudeCodeAuthKeys)
	if err != nil {
		return Plan{}, err
	}
	if in.MITM {
		if rule, placeholders, ok := anthropicInjectRuleFor(cred); ok {
			// Shape translation: the container is configured with the SAME credential env var the
			// user supplied, carrying a placeholder value (anthropic_wire.go's SHAPE MIRRORING), so
			// Claude Code runs its own code path for that shape. The real credential never rides
			// SecretsBundle at all — network.InjectedSecretKeys picks up rule.Set once merged (§4).
			env := askEnv(in)
			if env == nil {
				env = map[string]string{}
			}
			// KRAYT_INJECTED_CREDENTIAL is set here, not via credentialEnv: that helper keys off
			// in.InjectedKeys, which the caller computes from spec.Network.Inject BEFORE this
			// adapter's own rule is merged into it (internal/cli/run.go), so it is always false on
			// this path. Without the var, an entrypoint that predates the already-set-env-var branch
			// (§8.2) finds no /run/secrets file, concludes it has no credential, and exits 78 before
			// the agent ever starts — which is precisely what every published image does today.
			env["KRAYT_INJECTED_CREDENTIAL"] = cred
			return Plan{Env: env, Credential: cred, Inject: []task.InjectRule{rule}, Placeholders: placeholders}, nil
		}
	}
	return Plan{Env: credentialEnv(in, cred), Credential: cred}, nil
}
