// Package reexec is test-support for this repo's re-exec-the-test-binary fakes
// (internal/sandbox, internal/orchestrator and internal/cli each point sandbox.BinEnv at their own
// test binary and dispatch a real msb verb in argv[1] to a scriptable fake `msb`).
//
// It exists for one reason: under `-race`, running that binary costs a fixed ~1 SECOND of pure
// wall clock per process, and those packages spawn it hundreds of times per suite. The cost is
// ThreadSanitizer's atexit_sleep_ms flag, which defaults to 1000 and makes Finalize() sleep that
// long in the main thread before exiting whenever more than one thread is alive — which, in a Go
// binary, is always. It is not CPU: one orchestrator test driving ~20 msb calls measured 20.7s
// wall against 0.5s of user time, and `go test -race ./internal/configseed` — four pure unit tests
// with no subprocess at all — measured 1.007s.
//
// TSan reads that flag from the GORACE environment variable, but a plain `GORACE=...` exported
// around `go test` cannot reach these children: sandbox.childEnv hands the msb child a CLOSED
// allowlist (PATH/HOME/MSB_HOME/SSL_CERT_*/MSB_BACKEND) rather than os.Environ(), deliberately,
// and widening that allowlist to speed up tests would weaken the very production property
// TestChildEnvAllowlistExact exists to pin. So the child sets the flag on ITSELF instead:
// FastExit re-execs the already-started fake with GORACE in its environment, and the second image
// — the one that does the work and then exits — no longer sleeps.
//
// The re-exec, rather than an `sh` wrapper pointed at by BinEnv, is what keeps that allowlist test
// exact: a shell unavoidably exports its own PWD (and, on the macOS runner's bash-as-sh, `_`) into
// the child, which the test would correctly flag as an unexpected key. syscall.Exec adds nothing
// but the two variables named here, and SanitizeChildEnv removes exactly those.
//
// What this does NOT weaken: both images are fully race-instrumented, every memory access is still
// checked, a detected race is still reported and still exits 66 so the test still fails. The only
// thing given up is TSan's post-main grace window for catching races between the exiting main
// thread and threads still running during teardown — in these children, goroutines leaked past the
// end of a fake `msb copy`. No krayt production code runs in that window.
package reexec

import "os"

const (
	// MarkerEnv is set by FastExit on the image it execs, and is both the re-exec loop guard and
	// the signal to SanitizeChildEnv that the variables below are the harness's, not the parent's.
	// Its value records whether FastExit INVENTED the GORACE (markerInvented) or merely extended
	// one it inherited (markerInherited) — the distinction that keeps SanitizeChildEnv honest.
	MarkerEnv = "KRAYT_REEXEC_FASTEXIT"

	goraceEnv       = "GORACE"
	fastExitFlag    = "atexit_sleep_ms=0"
	markerInvented  = "invented"
	markerInherited = "inherited"
)

// FastExit is called by a fake msb's TestMain once it has recognized itself as a re-exec'd child,
// before it does any work. Under -race, and at most once per process, it replaces the running
// image with the same binary, same argv, plus GORACE=atexit_sleep_ms=0 — so this child skips
// TSan's one-second teardown sleep.
//
// It returns normally (and the caller just proceeds) whenever there is nothing to do or anything
// goes wrong: without -race there is no sleep to skip, on Windows there is no syscall.Exec and the
// native CI job runs without -race anyway, and a failed exec leaves a suite that still passes,
// only slowly. That is the right failure mode for an optimization.
func FastExit() {
	if !RaceEnabled {
		return
	}
	if _, already := os.LookupEnv(MarkerEnv); already {
		return // this IS the re-exec'd image
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	// Prepended, not assigned, so a GORACE that somehow reached us still wins under TSan's
	// last-repeat-wins flag parsing — and is recorded as inherited so SanitizeChildEnv leaves it
	// visible. For a real msb child, inheriting one could only mean krayt forwarded it, which is
	// exactly what TestChildEnvAllowlistExact must keep catching.
	value, marker := fastExitFlag, markerInvented
	if prev, ok := os.LookupEnv(goraceEnv); ok {
		value, marker = fastExitFlag+","+prev, markerInherited
	}
	env := append(os.Environ(), goraceEnv+"="+value, MarkerEnv+"="+marker)
	execSelf(self, os.Args, env)
}

// SanitizeChildEnv removes from a fake msb's record of its own os.Environ() exactly the variables
// FastExit added, so that what the fake records stays a truthful record of what sandbox.childEnv
// handed the child.
//
// Without this, internal/sandbox's TestChildEnvAllowlistExact — the test that pins the child-env
// allowlist CLOSED, the strongest assertion in that file — would see the harness's own GORACE and
// fail. Loosening that test to tolerate a GORACE would be the wrong trade: it would also stop
// catching a real one. Instead FastExit only claims a GORACE it actually invented, so one that
// krayt forwarded is recorded as inherited, is left in place here, and still fails the test.
func SanitizeChildEnv(env map[string]string) {
	marker, wrapped := env[MarkerEnv]
	if !wrapped {
		return
	}
	delete(env, MarkerEnv)
	if marker == markerInvented {
		delete(env, goraceEnv)
	}
}
