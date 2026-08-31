//go:build linux

// Command krayt-helper is the stateless, root-run guest binary that builds the patch
// (add-krayt-guest-helper.md; docs/adr-microsandbox-sandbox-layer.md, "The guest helper"). It is
// copied into a sandbox with `msb copy` and invoked with `msb exec --user root`, once per
// subcommand, against a workspace a non-root agent user has already edited — restoring exactly
// the privilege separation fix-guest-git-config-rce.md bought: the agent cannot write into a
// root-owned git dir, so the root-run helper never trusts agent-controlled git config.
//
// Stateless, exec'd, argv in and JSON on stdout, exits. No gRPC, no control protocol, no
// long-running process, no supervising the workload, no listener of any kind. If it ever grows
// one, krayt has re-created the guest agent inside someone else's sandbox while keeping none of
// B1's benefit, and the ADR's LOC ledger has to be re-examined at that point.
//
// It takes no secrets argument, reads no secret, and scans for none: secret values never enter
// the guest under B1, so the matched-secret-key-names scan moved host-side
// (hand-secrets-to-msb.md decision 6) — a deliberate narrowing of the ADR's original description
// of this helper's job.
//
// ask_human does not go through it either: that would require a listener, exactly the boundary
// above. krayt-ask dials AF_VSOCK to the host directly (dial-ask-channel-over-vsock.md).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Exit codes: success, a usage error (bad/missing flags), and a subcommand failure.
const (
	exitOK      = 0
	exitUsage   = 64
	exitFailure = 1
)

func main() {
	os.Exit(mainRun(os.Args[1:], os.Stdout, os.Stderr))
}

// mainRun is the testable core: dispatch to the two subcommands, nothing else (decision 1).
func mainRun(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: krayt-helper <setup|finish> [flags]")
		return exitUsage
	}
	switch args[0] {
	case "setup":
		return runSetupCmd(args[1:], stdout, stderr)
	case "finish":
		return runFinishCmd(args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "krayt-helper: unknown subcommand %q (want setup or finish)\n", args[0])
		return exitUsage
	}
}

// writeJSON encodes v to stdout, the success contract both subcommands share.
func writeJSON(stdout, stderr io.Writer, v any) int {
	if err := json.NewEncoder(stdout).Encode(v); err != nil {
		_, _ = fmt.Fprintln(stderr, "krayt-helper: encode result:", err)
		return exitFailure
	}
	return exitOK
}
