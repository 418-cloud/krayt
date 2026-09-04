package orchestrator

import (
	"net"
	"sync"
	"testing"
	"time"
)

// TestHandleRunCtlConnDropsSilentConnection: a client that connects and never writes must not
// block this goroutine forever — closing serveRunControl's listener does not close connections
// already accepted, so an unbounded Decode would leak a goroutine (and the connection's fd) for
// the supervisor's whole lifetime. Drives handleRunCtlConn directly over an in-memory net.Pipe
// with a short deadline, mirroring internal/askbridge's TestServeReadDeadlineDropsSilentConnection.
func TestHandleRunCtlConnDropsSilentConnection(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	answer := func(string, string, bool) error { return nil }

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		handleRunCtlConn(serverConn, answer, 100*time.Millisecond)
	}()

	buf := make([]byte, 1)
	start := time.Now()
	_, readErr := clientConn.Read(buf) // never wrote anything; expect the server to give up
	wg.Wait()

	if readErr == nil {
		t.Fatal("silent connection: read succeeded, want EOF/close once the server's deadline fires")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("connection stayed open for %v; want it dropped near the read deadline", elapsed)
	}
}

// TestHandleRunCtlConnAnswersBeforeDeadline is the sanity check for the deadline change above: a
// well-behaved client that sends its request promptly still gets a real answer.
func TestHandleRunCtlConnAnswersBeforeDeadline(t *testing.T) {
	socket := t.TempDir() + "/control.sock"
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix socket bind unavailable in this sandbox: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	var gotQID string
	answer := func(qid, _ string, _ bool) error {
		gotQID = qid
		return nil
	}

	go func() {
		conn, aerr := lis.Accept()
		if aerr != nil {
			return
		}
		handleRunCtlConn(conn, answer, time.Second)
	}()

	if err := DialRunControl(socket, "q1", "yes", false); err != nil {
		t.Fatalf("DialRunControl: %v", err)
	}
	if gotQID != "q1" {
		t.Errorf("answer callback got question id %q, want %q", gotQID, "q1")
	}
}
