package orchestrator

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// runctl.go is the msb-era replacement for `krayt answer`'s pre-msb path: dialing the guest's
// vsock control socket directly (any process could reach the VM, so a separate `krayt answer`
// invocation worked without going through the run's own supervising process). Under msb there is
// no guest-reachable control socket at all — the guest only ever dials OUT to ask a question
// (internal/askbridge) — so the authority to answer one lives only in the supervising process's
// memory (the askbridge.Bridge that registered it). This tiny host-only unix-socket protocol is
// how a separate `krayt answer` process reaches back into that memory: the run's supervisor
// listens on it (serveRunControl) and a second process dials it (DialRunControl). It never
// touches the sandbox, msb, or any secret — it is pure host-to-host IPC between two krayt
// invocations, present only for a --on-question=wait run (§6.13).

// runCtlRequest/runCtlResponse are the newline-delimited JSON protocol spoken over the socket:
// one request, one response, per connection — the same shape internal/askbridge and
// internal/askclient use for the guest-facing channel, reused here for consistency.
type runCtlRequest struct {
	QuestionID string `json:"question_id"`
	Response   string `json:"response"`
	NoAnswer   bool   `json:"no_answer"`
}

type runCtlResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// serveRunControl binds dir/control.sock (dir must already exist and be secured — the caller's
// job, since this is always co-located with the ask-bridge socket whose directory
// askbridge.Listen already hardens via sockroot.Ensure) and answers every connection with one
// exchange, calling answer for each. It returns the socket path (recorded in meta.json as
// CtrlSocket). Serving stops when the listener is closed; the caller owns that lifetime.
func serveRunControl(dir string, answer AnswerFunc) (string, func(), error) {
	path := filepath.Join(dir, "control.sock")
	lis, err := net.Listen("unix", path)
	if err != nil {
		return "", nil, fmt.Errorf("orchestrator: listen run control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = lis.Close()
		return "", nil, fmt.Errorf("orchestrator: chmod run control socket: %w", err)
	}
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go handleRunCtlConn(conn, answer)
		}
	}()
	return path, func() { _ = lis.Close() }, nil
}

func handleRunCtlConn(conn net.Conn, answer AnswerFunc) {
	defer func() { _ = conn.Close() }()
	var req runCtlRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	resp := runCtlResponse{OK: true}
	if err := answer(req.QuestionID, req.Response, req.NoAnswer); err != nil {
		resp = runCtlResponse{OK: false, Error: err.Error()}
	}
	_ = json.NewEncoder(conn).Encode(resp)
}

// DialRunControl delivers one answer to a live run's supervising process over its recorded
// control socket (§6.2, §6.13) — the cross-process half of serveRunControl, used by
// `krayt answer` when no in-process Manager owns the run (e.g. a detached supervisor, or a
// foreground run in a different terminal).
func DialRunControl(socket, questionID, response string, noAnswer bool) error {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return fmt.Errorf("orchestrator: dial run control socket: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if err := json.NewEncoder(conn).Encode(runCtlRequest{QuestionID: questionID, Response: response, NoAnswer: noAnswer}); err != nil {
		return fmt.Errorf("orchestrator: send run control request: %w", err)
	}
	var resp runCtlResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("orchestrator: read run control response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}
