package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/reexec"
)

// signalChildEnv makes the re-exec'd test binary act as a stand-in for main: install exactly
// main's shutdownSignals handling, report readiness, and exit 0 once the context is cancelled.
// A signal that is NOT handled kills the child instead, which is how the parent tells the two
// apart without a real sandbox.
const signalChildEnv = "KRAYT_TEST_SIGNAL_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(signalChildEnv) == "1" {
		// This process is test scaffolding, and the parent waits for it to exit after each signal.
		// Under -race that exit would otherwise spend a second in TSan's teardown sleep, once per
		// signal under test. See internal/reexec.
		reexec.FastExit()
		ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals...)
		defer stop()
		fmt.Println("ready")
		select {
		case <-ctx.Done():
			os.Exit(0)
		case <-time.After(30 * time.Second):
			os.Exit(3)
		}
	}
	os.Exit(m.Run())
}

// TestShutdownSignalsCancelContext proves each signal that ends a session in practice — Ctrl-C,
// `kill`, and a closed terminal — cancels the command context (so deferred sandbox teardown runs)
// rather than killing the process outright. SIGHUP is the regression: it used to be unhandled,
// and a closed `krayt shell` terminal leaked its sandbox.
func TestShutdownSignalsCancelContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals can't be sent to a process on Windows")
	}
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), signalChildEnv+"=1")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
				_ = cmd.Process.Kill()
				t.Fatalf("child never reported ready: %q, %v", line, err)
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			var exitErr *exec.ExitError
			switch {
			case err == nil:
				// Context cancelled: the handled path.
			case errors.As(err, &exitErr) && !exitErr.Exited():
				t.Fatalf("%v killed the process instead of cancelling the context (not in shutdownSignals)", sig)
			default:
				t.Fatalf("child: %v", err)
			}
		})
	}
}
