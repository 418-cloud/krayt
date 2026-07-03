package ask

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBridgeAskAnswer(t *testing.T) {
	var mu sync.Mutex
	var gotID, gotPrompt string
	b := NewBridge(func(id, prompt string, _ []string) error {
		mu.Lock()
		gotID, gotPrompt = id, prompt
		mu.Unlock()
		return nil
	})

	type res struct {
		resp  string
		noAns bool
		err   error
	}
	done := make(chan res, 1)
	go func() {
		r, n, e := b.Ask(context.Background(), "proceed?", []string{"yes", "no"})
		done <- res{r, n, e}
	}()

	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return gotID != "" })
	mu.Lock()
	id, prompt := gotID, gotPrompt
	mu.Unlock()
	if prompt != "proceed?" {
		t.Fatalf("pushed prompt = %q", prompt)
	}
	if !b.Answer(id, "yes", false) {
		t.Fatal("Answer did not match the pending question")
	}
	r := <-done
	if r.err != nil || r.resp != "yes" || r.noAns {
		t.Fatalf("Ask returned %+v", r)
	}
	if b.Answer(id, "again", false) {
		t.Error("a second answer to the same id should be a no-op")
	}
}

func TestAskCanceledIsNoAnswer(t *testing.T) {
	b := NewBridge(func(string, string, []string) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	type res struct {
		noAns bool
	}
	done := make(chan res, 1)
	go func() {
		_, n, _ := b.Ask(ctx, "q", nil)
		done <- res{n}
	}()
	time.Sleep(20 * time.Millisecond)
	cancel() // run torn down mid-question
	if r := <-done; !r.noAns {
		t.Error("a canceled Ask should return the no-answer sentinel")
	}
}

func TestServeSocketRoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "ask.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		// Some sandboxes forbid bind(2); the socket path is exercised on real hosts/CI.
		if strings.Contains(err.Error(), "operation not permitted") || strings.Contains(err.Error(), "permission denied") {
			t.Skipf("unix socket bind not permitted in this environment: %v", err)
		}
		t.Fatal(err)
	}
	var mu sync.Mutex
	var gotID string
	b := NewBridge(func(id, _ string, _ []string) error {
		mu.Lock()
		gotID = id
		mu.Unlock()
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, ln, b) }()

	type res struct {
		resp string
		err  error
	}
	done := make(chan res, 1)
	go func() {
		r, _, e := OverSocket(sock, "ok?", nil) // the "container" asking
		done <- res{r, e}
	}()

	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return gotID != "" })
	mu.Lock()
	id := gotID
	mu.Unlock()
	b.Answer(id, "sure", false)
	if r := <-done; r.err != nil || r.resp != "sure" {
		t.Fatalf("OverSocket returned %+v", r)
	}
}
