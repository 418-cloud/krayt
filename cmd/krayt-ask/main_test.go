package main

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/guest/ask"
)

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

	b := ask.NewBridge(func(_, _ string, _ []string) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ask.Serve(ctx, ln, b) }()

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
