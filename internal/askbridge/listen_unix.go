//go:build !windows

package askbridge

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/418-cloud/krayt/internal/sockroot"
)

// Listen creates dir if necessary, binds ask.sock in it, and hardens both. Which directory that
// is belongs to the caller: orchestrator.runSocketDir prefers the run's own private state
// directory (decision 4) and falls back to the per-uid `<tmp>/krayt-<uid>` root when the run
// directory's path would push the socket past macOS's sockaddr_un limit — decision 4's claim that
// the socket could be kept "short by construction" does not survive an unbounded repo path in
// front of it. This function treats both the same way, reusing sockroot.Ensure's
// hostile-pre-existing-directory refusal (decision 12) rather than a second copy of that check,
// then binds a unix socket at dir/ask.sock and chmods it 0600 (decision 10) — narrower than the
// in-guest bridge's 0777, which existed only so a non-root container could reach a root-owned
// directory; here the socket lives in a 0700 directory owned by the invoking user either way, so
// there is no non-root party to widen it for. net.Listen itself never unlinks a
// pre-existing path at that name, so a socket already present here is a fail-closed error, not an
// unlink-then-bind (decision 12).
//
// The premise 0600 rests on — that msb's local backend bridges the guest's vsock dial as the
// invoking user, not as root or a system daemon under some other uid — is confirmed on hardware,
// not assumed: hack/msb-probes/p1-vsock-nonroot.sh dials a 0600 socket inside a 0700 directory
// from a non-root guest process and logs the accepted connection's peer uid, which came back as
// the invoking user's on msb 0.6.16 (2026-09-02, KRAYT_SPEC.md §14 Phase 11's P1 bullet). Had it
// come back as anything else, this socket would have been unreachable and the tempting fix would
// have been exactly the 0777 above.
func Listen(dir string) (net.Listener, error) {
	if err := sockroot.Ensure(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "ask.sock")
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("askbridge: listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = lis.Close()
		return nil, fmt.Errorf("askbridge: chmod ask socket: %w", err)
	}
	return lis, nil
}
