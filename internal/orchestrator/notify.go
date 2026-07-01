package orchestrator

import (
	"context"
	"os/exec"
	"runtime"
	"time"
)

// notifyWaiting fires a best-effort desktop notification that a run is waiting for input
// (§6.13). It shells out to the platform tool (no extra dependency, §9.1) and silently no-ops
// when that tool is absent — the `waiting` state in `krayt ls` is the durable signal, the
// notification is just a nicety. The prompt comes from untrusted agent code, so only a fixed
// message is shown here; the full text lives in the persisted question record.
func notifyWaiting(runID, prompt string) {
	title := "krayt: run waiting for input"
	body := "run " + runID + " is waiting for an answer — krayt answer " + runID
	_ = prompt // intentionally not interpolated into the notification (untrusted; sanitized on display elsewhere)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		script := "display notification " + quoteAppleScript(body) + " with title " + quoteAppleScript(title)
		cmd = exec.CommandContext(ctx, "osascript", "-e", script)
	case "linux":
		cmd = exec.CommandContext(ctx, "notify-send", title, body)
	default:
		return
	}
	_ = cmd.Run() // best-effort; a missing tool is not an error
}

// quoteAppleScript wraps s as an AppleScript string literal, escaping backslashes and quotes
// so the notification body can't break out of the -e script.
func quoteAppleScript(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' || s[i] == '"' {
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	out = append(out, '"')
	return string(out)
}
