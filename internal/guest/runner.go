package guest

import (
	"context"

	"github.com/418-cloud/krayt/internal/protocol/pb"
)

// Container contract paths (§8.2): the guest-agent prepares per-run directories on the
// guest filesystem and the Runner bind-mounts them into the container at these canonical
// paths. They are the in-container locations; the host-side (guest fs) locations live under
// the Service root, which keeps the Service OS-agnostic and testable without a VM.
const (
	ContainerWorkspace  = "/workspace"
	ContainerTaskPrompt = "/task/prompt.md"
	ContainerOutput     = "/output"
	ContainerSecrets    = "/run/secrets"
	ContainerAskSocket  = "/run/krayt/ask.sock"      // in-VM question bridge socket (§6.13); front-ends connect here
	ContainerAskBin     = "/usr/local/bin/krayt-ask" // the krayt-ask CLI front-end, on PATH for shell-capable images (§6.13)
)

// RunConfig is what the Service hands the Runner to execute the user's image for one run
// (§6.10). All paths are guest-filesystem paths the Runner bind-mounts to the container
// contract locations above.
type RunConfig struct {
	ImageArchivePath string            // OCI archive on disk to import into containerd (§6.11)
	ImageRef         string            // image reference to create+run
	WorkspaceDir     string            // -> /workspace
	TaskPath         string            // -> /task/prompt.md
	OutputDir        string            // -> /output
	SecretsDir       string            // -> /run/secrets (tmpfs); empty when no secrets (§6.8)
	Env              map[string]string // non-secret env (§6.5 TaskSpec.env)
	AskSocket        string            // guest-side ask-bridge socket to bind-mount at /run/krayt/ask.sock (§6.13); empty if unavailable
	AskBinary        string            // guest-side krayt-ask binary to bind-mount onto the container PATH (§6.13); empty if unavailable
	Ask              AskFunc           // in-process bridge handle for fake runners; nil for the containerd runner
}

// LogFunc forwards a container log line to the host as it is produced; the Service wraps it
// into a RunEvent.LogLine on the Start stream (§6.5).
type LogFunc func(stream pb.LogLine_Stream, line []byte, tsUnixMs int64)

// AskFunc submits an agent → human question through the in-VM bridge and blocks until the
// host answers or the run is torn down (§6.13). It returns the response and whether it is a
// no-answer sentinel. The production containerd runner does not call this — the container
// reaches the bridge over AskSocket instead — but it lets an in-process (fake) runner drive a
// stubbed question without a unix socket.
type AskFunc func(ctx context.Context, prompt string, choices []string) (response string, noAnswer bool, err error)

// Runner runs exactly one user container per VM (§6.10). The production implementation
// (internal/guest/runner, //go:build linux) drives containerd; tests inject a fake that
// simulates the agent, so the Service's bundle→baseline→diff path is exercised without a
// real runtime (§14).
type Runner interface {
	// Run imports the image, runs its entrypoint with the contract mounts/env, streams
	// logs via log, and returns the container's exit code. A non-nil error is an
	// infrastructure failure (import/create/start), distinct from a non-zero exit code.
	Run(ctx context.Context, cfg RunConfig, log LogFunc) (exitCode int, err error)

	// Version reports the underlying runtime version for the Hello handshake (e.g. the
	// containerd version); empty if unknown.
	Version() string
}
