package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/askbridge"
)

// execMarkerEnv turns a re-exec of this test binary into the real krayt-ask CLI (the repo's
// established re-exec-the-test-binary pattern — internal/orchestrator/climit_test.go's TestMain,
// internal/sandbox/fakemsb_test.go's runFakeMsb): TestMain checks for it before anything else and,
// if set, calls the real main() and lets it os.Exit as it normally would. It has to be an env var,
// not an argv marker, because the whole point of these tests is to hand the subprocess
// application-shaped argv ("--choices a,b" or a bare question) indistinguishable from a real
// invocation.
const execMarkerEnv = "KRAYT_ASK_TEST_EXEC"

func TestMain(m *testing.M) {
	if os.Getenv(execMarkerEnv) == "1" {
		main() // os.Exit's itself; never returns
	}
	os.Exit(m.Run())
}

// TestRunSentinelWhenUnreachable: with no bridge behind the socket (fail mode / not wired), the
// CLI returns the no-answer sentinel with an empty stdout so the agent falls back (§6.13).
func TestRunSentinelWhenUnreachable(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "nope.sock")
	var out, errb bytes.Buffer
	if code := run([]string{"Should I proceed?"}, socket, &out, &errb); code != exitNoAnswer {
		t.Errorf("exit = %d, want %d (sentinel)", code, exitNoAnswer)
	}
	if out.Len() != 0 {
		t.Errorf("stdout should be empty on sentinel; got %q", out.String())
	}
}

// TestRunMalformedSocketIsUsageError: a malformed vsock:// KRAYT_ASK_SOCKET value is a usage
// error, not the no-answer sentinel — a misconfiguration must not look like "the agent quietly
// never asks" (§6.13).
func TestRunMalformedSocketIsUsageError(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"Should I proceed?"}, "vsock://not-a-cid:5000", &out, &errb); code != exitUsage {
		t.Errorf("exit = %d, want %d (usage)", code, exitUsage)
	}
	if out.Len() != 0 {
		t.Errorf("stdout should be empty on a usage error; got %q", out.String())
	}
}

// TestRunUsage: a missing question is a usage error, not a silent no-answer.
func TestRunUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, "", &out, &errb); code != exitUsage {
		t.Errorf("exit = %d, want %d", code, exitUsage)
	}
	if code := run([]string{"--bogus", "q"}, "", &out, &errb); code != exitUsage {
		t.Errorf("unknown flag: exit = %d, want %d", code, exitUsage)
	}
}

// TestRunRoundTrip drives the real client→bridge exchange over a unix socket: the CLI submits a
// question with choices and prints the answer the bridge delivers (§6.13).
func TestRunRoundTrip(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "ask.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix socket bind unavailable in this sandbox: %v", err)
	}
	defer func() { _ = ln.Close() }()

	b := askbridge.NewBridge(func(_, _ string, _ []string) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = askbridge.Serve(ctx, ln, b) }()

	// The first question the bridge registers is "q1"; answer it once it appears.
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if b.Answer("q1", "postgres", false) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	var out, errb bytes.Buffer
	code := run([]string{"--choices", "postgres, sqlite", "Which database?"}, socket, &out, &errb)
	if code != exitAnswered {
		t.Fatalf("exit = %d (stderr: %s), want %d", code, errb.String(), exitAnswered)
	}
	if got := strings.TrimSpace(out.String()); got != "postgres" {
		t.Errorf("stdout = %q, want postgres", got)
	}
}

// realBinaryCmd re-execs this test binary as the real krayt-ask CLI (execMarkerEnv), over a plain
// unix socket — the Done-when's "real krayt-ask invocation" offline test. The transport is the
// only thing vsock changes (dial-ask-channel-over-vsock.md), so exercising this over unix covers
// the CLI's process-level contract (exit code, stdout shape) without a VM.
func realBinaryCmd(t *testing.T, socket string, args ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), execMarkerEnv+"=1", "KRAYT_ASK_SOCKET="+socket)
	return cmd
}

// TestRealBinaryRoundTrip drives the actual compiled krayt-ask binary (re-exec'd-test-binary
// pattern) end to end: the answer reaches stdout and the process exits 0.
func TestRealBinaryRoundTrip(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "ask.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix socket bind unavailable in this sandbox: %v", err)
	}
	defer func() { _ = ln.Close() }()

	b := askbridge.NewBridge(func(_, _ string, _ []string) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = askbridge.Serve(ctx, ln, b) }()
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if b.Answer("q1", "postgres", false) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	cmd := realBinaryCmd(t, socket, "--choices", "postgres,sqlite", "Which database?")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("real krayt-ask binary failed: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "postgres" {
		t.Errorf("stdout = %q, want postgres", got)
	}
	if code := cmd.ProcessState.ExitCode(); code != exitAnswered {
		t.Errorf("exit code = %d, want %d", code, exitAnswered)
	}
}

// TestRealBinaryNoAnswerSentinelExit: the actual compiled binary against an unreachable bridge
// exits with the no-answer sentinel code and empty stdout.
func TestRealBinaryNoAnswerSentinelExit(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "nope.sock") // nothing listening

	cmd := realBinaryCmd(t, socket, "Should I proceed?")
	var out bytes.Buffer
	cmd.Stdout = &out
	runErr := cmd.Run()

	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		t.Fatalf("run error = %v, want an *exec.ExitError", runErr)
	}
	if code := exitErr.ExitCode(); code != exitNoAnswer {
		t.Errorf("exit code = %d, want %d", code, exitNoAnswer)
	}
	if out.Len() != 0 {
		t.Errorf("stdout should be empty on sentinel; got %q", out.String())
	}
}
