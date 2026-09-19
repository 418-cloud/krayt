// Command krayt is the CLI entry point (§9, §13).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/418-cloud/krayt/internal/cli"
)

// shutdownSignals cancel the command context so a run's or shell session's deferred sandbox
// teardown still fires (§7 guaranteed teardown): SIGINT for Ctrl-C, SIGTERM for `kill` and
// `krayt stop`, and SIGHUP for a closed terminal. Go's default action for an unhandled SIGHUP
// exits the process without running deferred functions, so before SIGHUP was listed here,
// closing the terminal of a `krayt shell` session (which has no --max-duration backstop) left its
// msb sandbox running (run_f771973c, 2026-09-17); with SIGHUP listed, the same test tore the
// sandbox down (run_3b460ff0, KRAYT_SPEC.md §14 Phase 12). syscall.SIGHUP is
// defined on Windows too; it is simply never delivered there.
var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals...)
	defer stop()

	if err := cli.NewRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "krayt:", err)
		os.Exit(1)
	}
}
