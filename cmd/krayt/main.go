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

func main() {
	// SIGINT/SIGTERM cancel the command context so a run's deferred VM teardown still
	// fires on Ctrl-C (§7 guaranteed teardown).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cli.NewRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "krayt:", err)
		os.Exit(1)
	}
}
