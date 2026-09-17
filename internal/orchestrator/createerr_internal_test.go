package orchestrator

import (
	"errors"
	"strings"
	"testing"
)

func TestPointCreateErrorAtConsoleLog(t *testing.T) {
	const logPath = "/repo/.krayt/runs/run_x/logs/console.log"
	base := errors.New("orchestrator: create sandbox: sandbox: msb create: exit status 1 (error: failed to start \"krayt-run_x\"\n" +
		"  → other: sandbox process exited (exit status: 0) before agent relay became available\n" +
		"  → run `msb logs --source system krayt-run_x` for full diagnostics)")

	got := pointCreateErrorAtConsoleLog(base, "krayt-run_x", logPath)
	msg := got.Error()
	if strings.Contains(msg, "msb logs --source system") {
		t.Errorf("msb's hint survived: %q", msg)
	}
	if !strings.Contains(msg, "  → full diagnostics saved to "+logPath+" (the sandbox has been removed))") {
		t.Errorf("hint not rewritten in place: %q", msg)
	}
	if !strings.Contains(msg, "before agent relay became available") {
		t.Errorf("the rest of msb's message was lost: %q", msg)
	}
	if !errors.Is(got, base) {
		t.Error("the original error is no longer reachable with errors.Is")
	}

	// A create failure without msb's hint (another sandbox's name, or a different message) gets
	// the pointer appended instead.
	other := errors.New("orchestrator: create sandbox: sandbox: msb create: exit status 1 (boom)")
	msg = pointCreateErrorAtConsoleLog(other, "krayt-run_x", logPath).Error()
	if msg != other.Error()+"; full diagnostics saved to "+logPath+" (the sandbox has been removed)" {
		t.Errorf("appended message = %q", msg)
	}
}
