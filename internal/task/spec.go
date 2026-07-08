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

// NetworkPolicy is the host-side network policy for a run; it mirrors the proto enum
// in §6.5 and is translated to protocol.NetworkPolicy before being pushed to the guest.
type NetworkPolicy struct {
	Mode  NetworkMode
	Allow []string
}
