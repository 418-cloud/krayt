// Package adapter is krayt's optional per-agent orchestration layer (§6.14, §8.1). The
// agnostic core transports the task, code, and secrets; an adapter shapes the agent-specific
// concerns the core must not know about: which secret is the model credential, enforcing that
// exactly one auth credential is set (so billing is never silently ambiguous, §6.14), and
// wiring the krayt-ask question front-end when a run pauses for human input (§6.13).
//
// It is host-side and pre-flight: Prepare validates and returns environment additions before
// the sandbox is created, so a misconfigured run fails fast (before the image is even resolved).
// The in-container export of the credential is the entrypoint's job (§8.2).
package adapter

import (
	"fmt"
	"sort"
	"strings"

	"github.com/418-cloud/krayt/internal/task"
)

// Input is what the run hands an adapter to prepare (§6.14). SecretKeys are the names — never
// the values — of the per-task secrets, so the adapter can select/validate the auth credential
// without touching secret material.
type Input struct {
	SecretKeys    []string // names of the per-task secrets
	QuestionsWait bool     // --on-question=wait: wire the krayt-ask front-end (§6.13)
	AskSocket     string   // KRAYT_ASK_SOCKET value the container should dial (§6.13, §8.2)

	// Placeholder, when set, returns the non-secret placeholder string a guest process sees in
	// place of a given secret key's real value (sandbox.SecretPlaceholder — msb's own
	// substitution stand-in, never the real credential). An adapter uses it to seed first-run
	// config that must match what the guest actually observes (§6.14 "First-run state", e.g.
	// claude-code's customApiKeyResponses.approved). This package stays msb-agnostic: it never
	// imports internal/sandbox itself, only this function. A nil Placeholder means the adapter
	// omits any placeholder-derived seed.
	Placeholder func(key string) string
}

// ConfigSeed declares one first-run config file an adapter wants seeded before the agent (or a
// human inside `krayt shell`) ever runs it (§6.14 "First-run state",
// seed-agent-first-run-config.md decision 2) — image-agnostic and entirely host-side. An adapter
// only describes the state it needs; internal/orchestrator does the guest I/O.
type ConfigSeed struct {
	// DirEnv is the guest env var that, when set and non-empty, replaces $HOME as the base
	// directory Path is resolved against (e.g. CLAUDE_CONFIG_DIR, GEMINI_CLI_HOME). Empty means
	// always use $HOME.
	DirEnv string
	// Path is the seed file's path, relative to the resolved base dir; clean, relative, and
	// carries no ".." element.
	Path string
	// Defaults is the JSON object merged INTO the file, filling in only what isn't already
	// there (decision 3's fill-in-never-override rule) — never a value that could overwrite an
	// image author's or a user's own explicit choice.
	Defaults map[string]any
}

// Plan is an adapter's host-side contribution to a run: non-secret env additions for the
// container, the secret key it selected as the model credential (for the report — the value
// itself never appears here), and the network-scoped secret declarations
// (hand-secrets-to-msb.md) the run should merge into its own — msb substitutes the credential's
// value at the host TLS boundary for exactly these hosts; it never rides the container's env or
// filesystem as a real value.
type Plan struct {
	Env        map[string]string
	Credential string
	Secrets    []task.SecretSpec

	// ConfigSeeds is the first-run guest config state this adapter wants filled in (§6.14
	// "First-run state") before the agent — or a human inside `krayt shell` — ever runs. Empty
	// means the adapter has nothing to seed, which is also what `none` and opencode return: no
	// first-run step gates an env-var credential for either.
	ConfigSeeds []ConfigSeed

	// TranscriptDir is where this agent writes its own session transcript, as a path RELATIVE to
	// the container user's $HOME. Empty means the adapter has no transcript to collect, which is
	// also what `none` returns — the run then captures nothing.
	//
	// Relative, not absolute, because the images disagree on the home directory: claude-code and
	// krayt-dev run as `agent` out of /home/agent, gemini-cli as `node` out of /home/node. The
	// orchestrator resolves $HOME inside the guest at capture time and joins it to this, so a
	// future image that moves its user does not silently capture nothing.
	//
	// Only claude-code's value is verified (documented, and no image sets CLAUDE_CONFIG_DIR or any
	// XDG override). The other two are inferred from each CLI's conventions and unexercised — which
	// is safe by construction: a wrong path makes the copy fail and the run carries on without a
	// transcript, exactly as it does today.
	TranscriptDir string
}

// Adapter is one agent integration. Name is the config/flag value (§8.1 `agent.adapter`).
type Adapter interface {
	Name() string
	Prepare(Input) (Plan, error)
}

// Get resolves an adapter by name (none | claude-code | gemini-cli | opencode). An unknown name
// errors so a typo fails fast instead of silently running the bare image entrypoint.
func Get(name string) (Adapter, error) {
	switch name {
	case "", "none":
		return none{}, nil
	case "claude-code":
		return claudeCode{}, nil
	case "gemini-cli":
		return geminiCLI{}, nil
	case "opencode":
		return openCode{}, nil
	default:
		return nil, fmt.Errorf("unknown agent adapter %q (want none, claude-code, gemini-cli, or opencode)", name)
	}
}

// Names lists every valid adapter name (the config/flag values Get accepts), for shell
// completion. Keep in sync with Get's switch — small enough that duplication here is the
// simplest way to keep both colocated and reviewable together.
func Names() []string { return []string{"none", "claude-code", "gemini-cli", "opencode"} }

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
