// Package task holds the host-side, fully-resolved description of one run plus the
// config schema and parsing. RunSpec is what the CLI hands to the orchestrator after
// merging defaults, config file, and flags (§6.1, §8.3).
package task

import "time"

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
	Detach       bool              // headless vs stream-to-terminal
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

// QuestionTimeoutAction is what happens when a question's wait limit expires (§6.13).
type QuestionTimeoutAction string

// Question timeout actions (§6.13).
const (
	OnTimeoutSentinel QuestionTimeoutAction = "sentinel" // default; agent gets a "no answer" sentinel
	OnTimeoutAbort    QuestionTimeoutAction = "abort"    // fail the whole run
)

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

// NetworkPolicy is the host-side network policy for a run; it mirrors the proto enum
// in §6.5 and is translated to protocol.NetworkPolicy before being pushed to the guest.
type NetworkPolicy struct {
	Mode  NetworkMode
	Allow []string
}
