// Package task holds the host-side, fully-resolved description of one run plus the
// config schema and parsing. RunSpec is what the CLI hands to the orchestrator after
// merging defaults, config file, and flags (§6.1, §8.3).
package task

import (
	"fmt"
	"time"
)

// RunSpec is the host-side, fully-resolved description of one run (config + flags +
// defaults already merged). The orchestrator derives the provider.VMSpec from
// RunSpec.Resources plus the pinned base image (§6.1).
type RunSpec struct {
	ID           string            // assigned by the orchestrator
	ImageRef     string            // user OCI image (tag or digest)
	RepoPath     string            // host repo to bundle (default: cwd)
	IncludeDirty bool              // include uncommitted changes via non-mutating capture (§6.7); wired in Phase 3
	BundleDepth  int               // forward-bundle shallow depth (§6.7); default 1, 0 = full history
	TaskPrompt   []byte            // contents of the task (file or inline)
	Env          map[string]string // non-secret env for the container
	SecretsPath  string            // path to per-task secrets file (may be empty)
	Network      NetworkPolicy     // mode + allowlist (mirrors the proto enum, §6.5)
	Resources    Resources         // CPUs, MemoryMiB, DiskGiB, Timeout
	Questions    QuestionsPolicy   // mode + per-question timeout + on-timeout (§6.13)
	Container    ContainerPolicy   // least-privilege OCI overrides applied by the guest runner (§6.10, §10)
	Detach       bool              // headless vs stream-to-terminal
}

// ContainerPolicy is the resolved per-task container hardening policy the guest runner turns
// into OCI spec options (§6.10, §10). The defaults are the secure ones — all capabilities
// dropped, the containerd seccomp profile applied, writable rootfs — so a zero value already
// closes the egress bypass; the fields only widen or narrow from there.
type ContainerPolicy struct {
	AddCapabilities   []string // opt-in caps re-granted on top of drop-all (normalized + validated, CAP_-prefixed)
	SeccompUnconfined bool     // drop the default seccomp profile (seccomp: unconfined)
	ReadonlyRootfs    bool     // mount the container rootfs read-only (default false; §8.2 caveat)
}

// SeccompMode is the config value for the container's seccomp profile (§8.1).
type SeccompMode string

// Seccomp modes (§8.1). An unset field ("") and the explicit "default" both apply the containerd
// default profile — so an unset field stays secure; only "unconfined" opts out.
const (
	SeccompUnset      SeccompMode = ""           // unset ⇒ containerd default profile (secure default)
	SeccompDefault    SeccompMode = "default"    // explicit alias for the default profile
	SeccompUnconfined SeccompMode = "unconfined" // no seccomp filter
)

// ParseSeccompMode validates a config seccomp value, mirroring ParseNetworkMode so a typo fails
// fast at config load rather than silently disabling the filter.
func ParseSeccompMode(s string) (SeccompMode, error) {
	switch m := SeccompMode(s); m {
	case SeccompUnset, SeccompDefault, SeccompUnconfined:
		return m, nil
	default:
		return "", fmt.Errorf("invalid seccomp mode %q (want %q or %q)", s, SeccompDefault, SeccompUnconfined)
	}
}

// Resources bounds one run (§6.1). Expiry of Timeout kills the container then the VM.
type Resources struct {
	CPUs      int
	MemoryMiB uint64
	DiskGiB   uint64
	Timeout   time.Duration // wall-clock; expiry kills container then VM
}

// QuestionMode controls whether a run pauses for agent → human questions (§6.13).
type QuestionMode string

// Question modes (§6.13).
const (
	QuestionFail QuestionMode = "fail" // default; autonomous — never blocks
	QuestionWait QuestionMode = "wait" // pause the run and surface the question
)

// ParseQuestionMode validates s against the known modes, keeping the set of valid values
// authoritative here rather than duplicated at each call site (CLI flag + config file).
func ParseQuestionMode(s string) (QuestionMode, error) {
	switch m := QuestionMode(s); m {
	case QuestionFail, QuestionWait:
		return m, nil
	default:
		return "", fmt.Errorf("invalid question mode %q (want %q or %q)", s, QuestionFail, QuestionWait)
	}
}

// QuestionTimeoutAction is what happens when a question's wait limit expires (§6.13).
type QuestionTimeoutAction string

// Question timeout actions (§6.13).
const (
	OnTimeoutSentinel QuestionTimeoutAction = "sentinel" // default; agent gets a "no answer" sentinel
	OnTimeoutAbort    QuestionTimeoutAction = "abort"    // fail the whole run
)

// ParseQuestionTimeoutAction validates s against the known on-timeout actions.
func ParseQuestionTimeoutAction(s string) (QuestionTimeoutAction, error) {
	switch a := QuestionTimeoutAction(s); a {
	case OnTimeoutSentinel, OnTimeoutAbort:
		return a, nil
	default:
		return "", fmt.Errorf("invalid on-timeout action %q (want %q or %q)", s, OnTimeoutSentinel, OnTimeoutAbort)
	}
}

// QuestionsPolicy governs the optional agent → human question channel (§6.13).
type QuestionsPolicy struct {
	Mode      QuestionMode          // QuestionFail (default) | QuestionWait
	Timeout   time.Duration         // per-question wait limit
	OnTimeout QuestionTimeoutAction // OnTimeoutSentinel (default) | OnTimeoutAbort
}

// NetworkMode mirrors the proto NetworkPolicy.Mode enum (§6.5).
type NetworkMode string

// Network policy modes (§6.5, §6.6).
const (
	NetworkAllowlist NetworkMode = "allowlist" // default; proxy enforces the domain list
	NetworkFull      NetworkMode = "full"      // nftables policy switched to accept
	NetworkNone      NetworkMode = "none"      // proxy denies everything
)

// ParseNetworkMode validates s against the known egress modes.
func ParseNetworkMode(s string) (NetworkMode, error) {
	switch m := NetworkMode(s); m {
	case NetworkAllowlist, NetworkFull, NetworkNone:
		return m, nil
	default:
		return "", fmt.Errorf("invalid network mode %q (want %q, %q, or %q)", s, NetworkAllowlist, NetworkFull, NetworkNone)
	}
}

// NetworkPolicy is the host-side network policy for a run (§6.6); internal/task/netpolicy_msb.go
// translates it into msb's own `--net-rule`/`--net-default*`/`--tls-intercept`/`--tls-bypass`
// argv, and internal/sandbox.SecretArgs/SecretEnv render Secrets into `--secret` flags plus the
// msb child's environment. None of this ever rides the guest — msb substitutes each declared
// secret's value at the host TLS boundary and the sandbox never holds anything but a placeholder.
type NetworkPolicy struct {
	Mode  NetworkMode
	Allow []string

	MITM        bool         // pre-msb only; hard-errored by ValidateNetworkPolicyForMsb (run-tasks-on-microsandbox.md)
	Passthrough []string     // hosts tunneled (never MITM'd/never intercepted); msb: --tls-bypass
	Inject      []InjectRule // pre-msb only; hard-errored by ValidateNetworkPolicyForMsb

	// Secrets is the msb-era secret-scoping list (hand-secrets-to-msb.md, wired at the
	// run-tasks-on-microsandbox.md cut-over): one SecretSpec per credential, resolved from
	// network.inject's raw config entries (SecretSpecsFromConfig) merged with any adapter-selected
	// credential (MergeSecretSpecs). Never populated alongside a non-empty Inject — a run uses one
	// delivery shape or the other, matching which orchestrator path actually consumes it.
	Secrets []SecretSpec
}

// InjectRule is one `network.inject[]` entry (§8.1): for requests to Host through the MITM
// path, delete every header named in Strip, then set every header in Set (resolved secrets-file
// key names, resolved host-side to real values) and SetLiteral (fixed, non-secret values).
// Strip and Set are deliberately separate — the header the container sends is not necessarily
// the header that goes upstream (step 3 removes one auth header and sets a different one).
//
// SetPrefix exists because a credential's wire format is not always "this header = this value"
// (§6.6.1, and the 2026-08-17 subscription-token observation in
// internal/adapter/anthropic_wire.go): it prefixes a Set header's resolved value with a literal —
// an auth SCHEME, e.g. `authorization: Bearer <token>`. Applied host-side while the secrets-file
// key is resolved (internal/orchestrator's buildEgressStdinConfig), so the proxy's contract stays
// the simple "set this header to this exact string" and no scheme knowledge reaches it.
type InjectRule struct {
	Host       string
	Strip      []string          // header names to delete from the guest's request first
	Set        map[string]string // header name -> secrets-file key name, resolved host-side
	SetPrefix  map[string]string // header name (must be in Set) -> literal prefix the value carries
	SetLiteral map[string]string // header name -> fixed literal value (never a secret)
	Refresh    *RefreshRule      // optional host-side credential refresh (plumbing only; step 3 fills in execution)

	// Withheld names secrets-file keys that must stay out of SecretsBundle even though this rule
	// no longer sets any header from them — e.g. an adapter-selected credential whose header the
	// user's own network.inject rule claimed instead (MergeInjectRules). Without this, dropping
	// the adapter's Set entry on conflict would also drop it from InjectedSecretKeys(), and the
	// credential the adapter deliberately kept out of the guest would ride SecretsBundle in.
	Withheld []string
}

// RefreshRule declaratively names an upstream credential-refresh endpoint for one InjectRule
// (§4.6). The proxy is generic: it recognizes the shape and, on this task, provides only the
// generic "one refresh, one retry" mechanism (the pre-msb host proxy's RefreshFunc seam) — it does not
// know how to actually perform a refresh for any specific vendor. That knowledge (request
// construction, response parsing) belongs in a per-agent adapter (§6.14), the first consumer
// being inject-claude-oauth-token-at-proxy.md (step 3).
type RefreshRule struct {
	Host                string
	PathPrefix          string
	ResponseTokenFields []string
}
