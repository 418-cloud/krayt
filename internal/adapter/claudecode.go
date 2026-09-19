package adapter

import (
	"strings"

	"github.com/418-cloud/krayt/internal/task"
)

// claudeCodeAPIHost is the one host Claude Code's model-provider credential ever needs
// (hand-secrets-to-msb.md): msb substitutes a placeholder STRING wherever it appears rather than
// a named header, so this adapter needs to know only the host, not which header carries the
// credential.
const claudeCodeAPIHost = "api.anthropic.com"

// claudeCodeAuthKeys are the auth credentials Claude Code accepts, in the §6.14 precedence
// order krayt surfaces in errors. Exactly one must be set: with both ANTHROPIC_API_KEY and
// CLAUDE_CODE_OAUTH_TOKEN present the API key silently wins and the subscription is bypassed
// (billed as API usage), so krayt refuses the ambiguous combination rather than picking for the
// user. Cloud-provider auth (CLAUDE_CODE_USE_BEDROCK/_VERTEX/_FOUNDRY) is out of scope here.
var claudeCodeAuthKeys = []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"}

// claudeCode is the worked-example adapter (§6.14): it enforces the exactly-one auth rule, wires
// the krayt-ask front-end, and scopes the selected credential to msb's TLS-substitution channel.
// msb sets the guest's OWN credential env var to its default placeholder itself
// (KRAYT_SPEC.md §6.14), so there is nothing here to synthesize for the container.
type claudeCode struct{}

func (claudeCode) Name() string { return "claude-code" }

// claudeCodeConfigDirEnv/claudeCodeConfigPath locate Claude Code's global config
// (seed-agent-first-run-config.md evidence): join(CLAUDE_CONFIG_DIR || homedir(), ".claude.json").
const (
	claudeCodeConfigDirEnv = "CLAUDE_CONFIG_DIR"
	claudeCodeConfigPath   = ".claude.json"
)

// claudeCodeApprovalLen is the number of trailing characters Claude Code's own startup flow
// checks a custom API key against (`key.trim().slice(-20)`, seed-agent-first-run-config.md
// evidence).
const claudeCodeApprovalLen = 20

func (claudeCode) Prepare(in Input) (Plan, error) {
	cred, err := exactlyOne("claude-code", in.SecretKeys, claudeCodeAuthKeys)
	if err != nil {
		return Plan{}, err
	}

	// hasCompletedOnboarding always: skips the theme picker, login flow, and connectivity
	// preflight (which otherwise fails reaching platform.claude.com, unallowlisted by default) —
	// §6.14 "First-run state". Nothing else from onboarding: not the folder-trust dialog, not the
	// theme, not bypassPermissionsModeAccepted (seed-agent-first-run-config.md decision 9).
	defaults := map[string]any{"hasCompletedOnboarding": true}
	// ANTHROPIC_API_KEY additionally needs its approval seeded — onboarding being skipped does
	// NOT make Claude Code use an env ANTHROPIC_API_KEY on its own; interactive mode still checks
	// customApiKeyResponses.approved and defaults to rejecting it. ANTHROPIC_AUTH_TOKEN and
	// CLAUDE_CODE_OAUTH_TOKEN need no such approval (decision 9).
	if cred == "ANTHROPIC_API_KEY" && in.Placeholder != nil {
		defaults["customApiKeyResponses"] = map[string]any{
			"approved": []any{claudeCodeApprovalElement(in.Placeholder(cred))},
		}
	}

	return Plan{
		Env:        askEnv(in),
		Credential: cred,
		Secrets:    []task.SecretSpec{{Key: cred, Hosts: []string{claudeCodeAPIHost}}},
		ConfigSeeds: []ConfigSeed{{
			DirEnv:   claudeCodeConfigDirEnv,
			Path:     claudeCodeConfigPath,
			Defaults: defaults,
		}},
		// $HOME/.claude/projects/<slugified-cwd>/<session-uuid>.jsonl — verified against Claude Code's
		// documented session storage; -p (print mode) persists a transcript the same as an
		// interactive session, and it records tool calls AND their results, which stdout does not.
		TranscriptDir: ".claude/projects",
	}, nil
}

// claudeCodeApprovalElement mirrors the minified startup flow's own check
// (seed-agent-first-run-config.md evidence): an env ANTHROPIC_API_KEY is approved only if
// key.trim().slice(-20) is present in customApiKeyResponses.approved. JS trim() ≈
// strings.TrimSpace for this ASCII placeholder value.
func claudeCodeApprovalElement(placeholder string) string {
	p := strings.TrimSpace(placeholder)
	if len(p) <= claudeCodeApprovalLen {
		return p
	}
	return p[len(p)-claudeCodeApprovalLen:]
}
