package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/418-cloud/krayt/internal/sockroot"
)

// maxUnixSocketPath is the longest path bind(2) will accept for a unix socket, taken as the
// SHORTEST limit krayt has to run under rather than the local one: macOS's sockaddr_un.sun_path
// is 104 bytes including the NUL, so 103 characters. Linux allows 107. Using the smaller number
// everywhere means a run that works on Linux works on macOS, and costs nothing — no krayt socket
// is anywhere near either limit by itself.
//
// Exceeding it does not fail politely. bind returns EINVAL, which surfaces as the thoroughly
// unhelpful "bind: invalid argument" with no mention of length at all.
const maxUnixSocketPath = 103

// runSocketNames is every basename bound inside a run's socket directory. The budget check below
// has to clear the LONGEST of them, not just the one being bound at that moment, or a run would
// bind ask.sock successfully and then fail three statements later on control.sock.
var runSocketNames = []string{"ask.sock", "control.sock"}

// runSocketDir picks and prepares the directory holding a run's unix control sockets, preferring
// the run's own private state directory and falling back to a short shared root only when that
// path cannot fit under maxUnixSocketPath.
//
// dial-ask-channel-over-vsock.md decision 4 put these sockets in the run's own state dir and told
// us to "keep the path short by construction rather than 'fixing' an overflow later by moving to
// a shared world-writable dir". The first half is right and is what this function still prefers;
// the second half cannot be done as stated, and a real run proved it: that path is
// <repo>/.krayt/runs/run_<8hex>/ask/<name>, whose krayt-controlled suffix is already 43 bytes,
// leaving ~60 for a repo path krayt does not choose and cannot bound. macOS's own per-user
// temp dir is 49 bytes before the repo name, so `krayt run` from a scratch repo under $TMPDIR
// overflowed at 106 bytes and every --on-question=wait run failed instantly with EINVAL. There is
// no construction that keeps an unbounded caller-supplied prefix short.
//
// The fallback is not the world-writable dir that decision was guarding against. /tmp/krayt-<uid>
// is the root harden-vfkit-socket-dir.md established and sockroot.Ensure enforces: 0700, owned by
// the invoking user, never followed through a symlink, refused outright rather than repaired if
// something hostile is already sitting there. The per-run subdirectory under it gets the same
// treatment from askbridge.Listen. What the run loses in the fallback case is only that the
// socket no longer sits beside the run's other state — not any property that was protecting it.
//
// cleanup removes the directory when it lives outside runDir, and is a no-op otherwise, where the
// run directory's own lifetime already owns it. It is always non-nil.
func runSocketDir(runDir, runID string) (dir string, cleanup func(), err error) {
	noop := func() {}

	preferred := filepath.Join(runDir, "ask")
	if socketDirFits(preferred) {
		return preferred, noop, nil
	}

	for _, root := range shortSocketRoots() {
		fallback := filepath.Join(root, runID)
		if !socketDirFits(fallback) {
			continue
		}
		// askbridge.Listen hardens the leaf directory; the shared root above it is this
		// function's to vouch for, and it is the one an attacker could pre-place, since its
		// name is predictable. A hostile root is fail-closed, never a reason to try the next
		// candidate.
		if err := sockroot.Ensure(root); err != nil {
			return "", noop, err
		}
		return fallback, func() { _ = os.RemoveAll(fallback) }, nil
	}
	return "", noop, fmt.Errorf("orchestrator: no unix socket path short enough for run %s: "+
		"%q needs %d bytes, over the %d-byte limit, and no short root fits either",
		runID, preferred, longestSocketPathLen(preferred), maxUnixSocketPath)
}

// shortSocketRoots are the fallback roots, tried in order: the user's own temp dir first, then
// literal /tmp. Both get the same sockroot.Ensure hardening, so the only thing separating them is
// length — and length is the entire reason this fallback exists, so /tmp (always 4 bytes) has to
// remain reachable for the case where $TMPDIR is itself too long to help. vfkit used /tmp
// unconditionally; preferring os.TempDir() keeps the socket inside the per-user directory macOS
// already hands out when it fits.
func shortSocketRoots() []string {
	name := fmt.Sprintf("krayt-%d", os.Getuid())
	roots := []string{filepath.Join(os.TempDir(), name)}
	if fixed := filepath.Join("/tmp", name); fixed != roots[0] {
		roots = append(roots, fixed)
	}
	return roots
}

// socketDirFits reports whether every socket krayt binds in dir stays under the limit.
func socketDirFits(dir string) bool { return longestSocketPathLen(dir) <= maxUnixSocketPath }

func longestSocketPathLen(dir string) int {
	longest := 0
	for _, n := range runSocketNames {
		if l := len(filepath.Join(dir, n)); l > longest {
			longest = l
		}
	}
	return longest
}
