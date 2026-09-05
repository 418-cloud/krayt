package askclient

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/askbridge"
)

// TestOverSocketRoundTrip drives the full client↔server exchange over a unix socket against the
// real internal/askbridge server half — the two packages must agree on the wire protocol despite
// living in separate packages (run-tasks-on-microsandbox.md split the pre-msb in-guest ask
// package into this client-only package plus internal/askbridge's server half).
func TestOverSocketRoundTrip(t *testing.T) {
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
			if b.Answer("q1", "sure", false) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	resp, noAnswer, err := OverSocket(socket, "ok?", nil)
	if err != nil {
		t.Fatalf("OverSocket: %v", err)
	}
	if noAnswer || resp != "sure" {
		t.Fatalf("OverSocket = (%q, %v), want (sure, false)", resp, noAnswer)
	}
}

func TestOverSocketMalformedVsockIsError(t *testing.T) {
	if _, _, err := OverSocket("vsock://bad", "q", nil); err == nil {
		t.Fatal("OverSocket with a malformed vsock:// socket should error")
	}
}
