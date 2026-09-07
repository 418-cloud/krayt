package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// slotPoll is how often AcquireSlot re-checks for a freed slot while all are held.
const slotPoll = 100 * time.Millisecond

// slotsDir holds the per-.krayt lock files backing the cross-process concurrency limit (§6.2).
func slotsDir(stateDir string) string { return filepath.Join(stateDir, "slots") }

// AcquireSlot blocks until one of limit concurrency slots is free, then returns a release func
// to call when the run ends. Concurrency is enforced with advisory whole-file locks on limit
// slot files under stateDir/slots/ — flock on Unix (climit_unix.go), LockFileEx on Windows
// (climit_windows.go) — so the cap holds across independent processes sharing one .krayt —
// several foreground `krayt run`s and detached supervisors alike — not merely within a process
// (§6.2). Both platforms release the lock when the holder's fd/handle closes, including on
// crash, so slots never leak. limit <= 0 means unbounded (a no-op release). ctx cancellation
// aborts the wait.
func AcquireSlot(ctx context.Context, stateDir string, limit int) (func(), error) {
	if limit <= 0 {
		return func() {}, nil
	}
	dir := slotsDir(stateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("orchestrator: create slots dir: %w", err)
	}
	for {
		for i := 0; i < limit; i++ {
			f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("slot-%d", i)), os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				return nil, fmt.Errorf("orchestrator: open slot: %w", err)
			}
			// Non-blocking exclusive lock: a held slot reports "not acquired" here, so we move on
			// to the next; separate open()s contend even within one process, so this bounds
			// same-process concurrency too. Any OTHER lock error (e.g. a filesystem that doesn't
			// support advisory locks) is persistent — polling won't clear it — so fail fast with a
			// clear error instead of spinning until ctx expires and reporting a misleading deadline.
			locked, lerr := tryLockSlot(f)
			if lerr != nil {
				_ = f.Close()
				return nil, fmt.Errorf("orchestrator: lock slot %d in %s: %w", i, dir, lerr)
			}
			if locked {
				return func() { _ = unlockSlot(f); _ = f.Close() }, nil
			}
			_ = f.Close()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(slotPoll):
		}
	}
}
