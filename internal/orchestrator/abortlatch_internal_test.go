package orchestrator

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestAbortLatchFiredWaitsForHandlerInFlight is the regression guard for the race that let a
// timed-out run report success. The timeout handler releases the agent (bridge.Answer) partway
// through its body; the agent can then exit and the orchestrator reach its post-exec read while
// the handler is still deciding. fired() must block for that handler rather than sampling a flag
// it has not written yet.
func TestAbortLatchFiredWaitsForHandlerInFlight(t *testing.T) {
	var l abortLatch
	var handlerDone atomic.Bool
	started := make(chan struct{})

	go func() {
		defer l.begin()()
		close(started) // the agent is released at this point in the real handler
		time.Sleep(50 * time.Millisecond)
		l.set()
		handlerDone.Store(true)
	}()

	<-started
	if !l.fired() {
		t.Fatal("fired() reported no abort while the handler was still mid-decision")
	}
	if !handlerDone.Load() {
		t.Error("fired() returned before the handler finished; it did not wait")
	}
}

// A latch no handler ever touched must not claim an abort, and must not block.
func TestAbortLatchQuietWhenNoTimeoutFired(t *testing.T) {
	var l abortLatch
	done := make(chan bool, 1)
	go func() { done <- l.fired() }()
	select {
	case got := <-done:
		if got {
			t.Error("fired() reported an abort with no handler having run")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fired() blocked with no handler in flight")
	}
}

// A handler that finds the question already answered returns without setting the latch — the
// self-correcting property §6.13 requires, so a human answer moments before the deadline is never
// retroactively turned into an abort.
func TestAbortLatchHandlerThatDoesNotSetLeavesRunAlive(t *testing.T) {
	var l abortLatch
	func() { defer l.begin()() }() // handler ran, decided there was nothing to do
	if l.fired() {
		t.Error("a handler that set nothing still reported an abort")
	}
}
