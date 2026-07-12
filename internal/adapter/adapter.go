// Package adapter is krayt's optional per-agent orchestration layer (§6.14, §8.1). The
// agnostic core transports the task, code, and secrets; an adapter shapes the agent-specific
// concerns the core must not know about: which secret is the model credential, enforcing that
// exactly one auth credential is set (so billing is never silently ambiguous, §6.14), and
// wiring the krayt-ask question front-end when a run pauses for human input (§6.13).
//
// It is host-side and pre-flight: Prepare validates and returns environment additions before
// the VM boots, so a misconfigured run fails fast (before the image is pulled). The in-VM
// export of the credential is the container entrypoint's job (§8.2); MCP-server registration
// is Phase 6.
package adapter

import (
	"fmt"
	"sort"
	"strings"
)

// Input is what the run hands an adapter to prepare (§6.14). SecretKeys are the names — never
// the values — of the per-task secrets, so the adapter can select/validate the auth credential
// without touching secret material.
type Input struct {
	SecretKeys    []string // names of the per-task secrets
	QuestionsWait bool     // --on-question=wait: wire the krayt-ask front-end (§6.13)
	AskSocket     string   // container path to the ask-bridge socket (§6.13)
}

// Plan is an adapter's host-side contribution to a run: non-secret env additions for the
// container, and the secret key it selected as the model credential (for the report — the
// value stays in the secrets bundle, never here).
type Plan struct {
	Env        map[string]string
	Credential string
}

// Adapter is one agent integration. Name is the config/flag value (§8.1 `agent.adapter`).
type Adapter interface {
	Name() string
	Prepare(Input) (Plan, error)
}

// Get resolves an adapter by name (none | claude-code | gemini-cli). An unknown name errors so
// a typo fails fast instead of silently running the bare image entrypoint.
func Get(name string) (Adapter, error) {
	switch name {
	case "", "none":
		return none{}, nil
	case "claude-code":
		return claudeCode{}, nil
	case "gemini-cli":
		return geminiCLI{}, nil
	default:
		return nil, fmt.Errorf("unknown agent adapter %q (want none, claude-code, or gemini-cli)", name)
	}
}

// Names lists every valid adapter name (the config/flag values Get accepts), for shell
// completion. Keep in sync with Get's switch — small enough that duplication here is the
// simplest way to keep both colocated and reviewable together.
func Names() []string { return []string{"none", "claude-code", "gemini-cli"} }

// askEnv is the shared krayt-ask wiring: when the run pauses for questions, tell the in-image
// krayt-ask binary which socket to reach (§6.13). Universal across adapters — krayt-ask is the
// lowest-common-denominator front-end.
func askEnv(in Input) map[string]string {
	if !in.QuestionsWait || in.AskSocket == "" {
		return nil
	}
	return map[string]string{"KRAYT_ASK_SOCKET": in.AskSocket}
}

// exactlyOne selects the single credential key present among the recognized set, erroring on
// zero or many so a run never boots with missing or ambiguous auth (§6.14). recognized is in
// human-facing order for the error message.
func exactlyOne(agent string, secretKeys, recognized []string) (string, error) {
	have := make(map[string]bool, len(secretKeys))
	for _, k := range secretKeys {
		have[k] = true
	}
	var found []string
	for _, k := range recognized {
		if have[k] {
			found = append(found, k)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("%s: no auth credential in the secrets file (set exactly one of %s) (§6.14)",
			agent, strings.Join(recognized, ", "))
	default:
		sort.Strings(found)
		return "", fmt.Errorf("%s: %d auth credentials set (%s); set exactly one so billing isn't ambiguous (§6.14)",
			agent, len(found), strings.Join(found, ", "))
	}
}

// none is the default adapter: the image entrypoint owns everything; krayt only wires the
// universal krayt-ask front-end when questions are enabled.
type none struct{}

func (none) Name() string                   { return "none" }
func (none) Prepare(in Input) (Plan, error) { return Plan{Env: askEnv(in)}, nil }
