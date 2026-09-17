package orchestrator

import (
	"strings"
)

// consoleLogHintError is a failed `msb create` whose boot diagnostics krayt has already saved to
// the run's console.log. Error() is the original message with msb's diagnostics hint rewritten to
// name that file. Unwrap keeps the original error for errors.Is/As.
type consoleLogHintError struct {
	err error
	msg string
}

func (e *consoleLogHintError) Error() string { return e.msg }
func (e *consoleLogHintError) Unwrap() error { return e.err }

// pointCreateErrorAtConsoleLog rewrites a create failure to point at the saved console.log
// (logPath). msb's own message says "run `msb logs --source system <name>` for full diagnostics",
// but krayt's teardown has already removed the sandbox by the time anyone reads it, so that
// command finds nothing (`error: sandbox not found`, 2026-09-17, run_012a74f8). The saved file
// holds the same diagnostics, including the root cause (e.g. `agentd: init failed: … guest user
// not found`, run_985a6aae). Call it only when console.log was actually written.
func pointCreateErrorAtConsoleLog(err error, name, logPath string) error {
	msbHint := "run `msb logs --source system " + name + "` for full diagnostics"
	saved := "full diagnostics saved to " + logPath + " (the sandbox has been removed)"
	msg := err.Error()
	if strings.Contains(msg, msbHint) {
		msg = strings.Replace(msg, msbHint, saved, 1)
	} else {
		msg += "; " + saved
	}
	return &consoleLogHintError{err: err, msg: msg}
}
