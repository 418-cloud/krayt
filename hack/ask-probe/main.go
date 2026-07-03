// Command ask-probe is a throwaway "agent" image entrypoint that exercises the Phase-4
// agent→human question channel (§6.13) on real hardware — the confirmation described in
// HUMAN_TODO. It connects to the in-VM ask-bridge socket, asks one question, blocks for the
// human's answer (delivered by `krayt answer`), then writes the answer into /workspace so it
// lands in changes.patch.
//
// It speaks the raw wire protocol (no krayt imports) so a green run proves the transport
// itself, and it logs every hop with a distinct non-zero exit code per failure so a hardware
// break is point-blank obvious from `krayt ls` (the EXIT column) or the logs.
//
//	exit 0  — asked, answered, decision written (success)
//	exit 10 — /run/krayt/ask.sock absent  (runner didn't bind-mount it, or the guest couldn't open it)
//	exit 11 — dial failed
//	exit 12 — send failed
//	exit 13 — receive failed (no answer arrived / connection dropped)
//	exit 14 — writing the decision into /workspace failed
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	sockPath  = "/run/krayt/ask.sock" // where the runner bind-mounts the bridge (§6.13)
	workspace = "/workspace"          // the cloned repo; new files here show up in the patch
	outFile   = "ask-probe-decision.txt"
)

// request/response mirror internal/guest/ask.wireRequest / wireResponse exactly.
type request struct {
	Prompt  string   `json:"prompt"`
	Choices []string `json:"choices,omitempty"`
}

type response struct {
	Response string `json:"response"`
	NoAnswer bool   `json:"no_answer"`
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[ask-probe] "+format+"\n", a...)
}

func main() { os.Exit(run()) }

func run() int {
	logf("start: probing the agent→human question channel (§6.13)")

	// 1. The socket must exist — proves the guest opened it and the runner bind-mounted it in.
	if fi, err := os.Stat(sockPath); err != nil {
		logf("FAIL(10): %s absent: %v", sockPath, err)
		dumpDir("/run")
		dumpDir("/run/krayt")
		return 10
	} else {
		logf("ok: %s present (mode %s)", sockPath, fi.Mode())
	}

	// 2. Connect.
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		logf("FAIL(11): dial %s: %v", sockPath, err)
		return 11
	}
	defer func() { _ = conn.Close() }()
	logf("ok: connected to the bridge")

	// 3. Ask.
	if err := json.NewEncoder(conn).Encode(request{Prompt: "ask-probe: proceed?", Choices: []string{"yes", "no"}}); err != nil {
		logf("FAIL(12): send question: %v", err)
		return 12
	}
	logf("ok: question sent — the run is now `waiting`; answer it with `krayt answer <id> <yes|no>`")

	// 4. Block for the answer (arrives via the host Answer RPC, or a timeout sentinel).
	var resp response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		logf("FAIL(13): receive answer: %v", err)
		return 13
	}
	decision := resp.Response
	if resp.NoAnswer {
		decision = "no-answer-sentinel"
	}
	logf("ok: got answer response=%q no_answer=%v -> decision=%q", resp.Response, resp.NoAnswer, decision)

	// 5. Record it where it lands in the patch.
	out := filepath.Join(workspace, outFile)
	if err := os.WriteFile(out, []byte(decision+"\n"), 0o644); err != nil {
		logf("FAIL(14): write %s: %v", out, err)
		return 14
	}
	logf("ok: wrote %s", out)
	logf("done: success")
	return 0
}

// dumpDir best-effort lists a directory to aid debugging a missing socket.
func dumpDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		logf("  (%s: %v)", dir, err)
		return
	}
	logf("  %s contents:", dir)
	for _, e := range entries {
		logf("    - %s", e.Name())
	}
}
