//go:build windows

package askbridge

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Microsoft/go-winio"
)

// TestListenCreatesNamedPipe: Listen's address is a \\.\pipe\ name derived from the run ID (the
// parent directory's basename), the Windows analogue of listen_unix.go's dir/ask.sock.
func TestListenCreatesNamedPipe(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run_deadbeef", "ask")
	lis, err := Listen(dir)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = lis.Close() }()

	addr := lis.Addr().String()
	if !strings.HasPrefix(addr, `\\.\pipe\krayt-ask-run_deadbeef`) {
		t.Errorf("Addr = %q, want a name derived from the run ID", addr)
	}
}

// TestListenRoundTrip proves the returned net.Listener actually round-trips a connection through
// Serve/Bridge, exactly as the unix implementation's callers rely on.
func TestListenRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run_roundtrip", "ask")
	lis, err := Listen(dir)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = lis.Close() }()

	b := NewBridge(func(_, _ string, _ []string) error { return nil })
	answerFirstQuestion(t, b, "yes")
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		handleConn(t.Context(), conn, b, askReadDeadline)
	}()

	conn, err := winio.DialPipe(lis.Addr().String(), nil)
	if err != nil {
		t.Fatalf("DialPipe: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := json.NewEncoder(conn).Encode(wireRequest{Prompt: "proceed?"}); err != nil {
		t.Fatalf("encode request: %v", err)
	}
	var resp wireResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Response != "yes" || resp.NoAnswer {
		t.Errorf("resp = %+v, want {Response: yes, NoAnswer: false}", resp)
	}
}
