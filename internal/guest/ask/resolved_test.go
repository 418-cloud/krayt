package ask

import (
	"context"
	"sync"
	"testing"
)

// TestBridgeOnResolved: the OnResolved hook fires (once) with the question id when an answer is
// delivered, and not for a no-op Answer to an unknown id (§6.13 — the host's resume signal).
func TestBridgeOnResolved(t *testing.T) {
	var mu sync.Mutex
	var resolved []string
	b := NewBridge(func(_, _ string, _ []string) error { return nil })
	b.OnResolved(func(id string) { mu.Lock(); resolved = append(resolved, id); mu.Unlock() })

	// An Answer to a question that isn't pending must not fire the hook.
	if b.Answer("nope", "x", false) {
		t.Fatal("Answer to unknown id should return false")
	}

	answered := make(chan struct{})
	go func() {
		_, _, _ = b.Ask(context.Background(), "proceed?", nil)
		close(answered)
	}()
	// The first Ask registers "q1"; answer it once it's pending.
	waitFor(t, func() bool { return b.Answer("q1", "yes", false) })
	<-answered

	mu.Lock()
	defer mu.Unlock()
	if len(resolved) != 1 || resolved[0] != "q1" {
		t.Errorf("OnResolved fired = %v, want [q1]", resolved)
	}
}
