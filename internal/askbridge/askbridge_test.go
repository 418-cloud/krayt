package askbridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/secrets"
)

func newTestListener(t *testing.T) (net.Listener, string) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "ask.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix socket bind unavailable in this sandbox: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	return lis, socket
}

// dialAndExchange writes a raw request and returns the raw response (or an error, if the server
// dropped the connection instead of answering), without going through package ask's client
// helpers — this package tests the server half only.
func dialAndExchange(t *testing.T, socket string, req wireRequest) (wireResponse, error) {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	var resp wireResponse
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return resp, err // e.g. a refused oversized request can EPIPE mid-write
	}
	err = json.NewDecoder(conn).Decode(&resp)
	return resp, err
}

// TestServeRoundTrip drives the full loop over a plain unix socket (the Done-when's offline
// test): Serve + Bridge answer a real question exchange.
func TestServeRoundTrip(t *testing.T) {
	lis, socket := newTestListener(t)
	b := NewBridge(func(_, _ string, _ []string) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, lis, b) }()

	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if b.Answer("q1", "postgres", false) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	resp, err := dialAndExchange(t, socket, wireRequest{Prompt: "which database?", Choices: []string{"postgres", "sqlite"}})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if resp.NoAnswer || resp.Response != "postgres" {
		t.Errorf("resp = %+v, want {postgres false}", resp)
	}
}

// TestServeNoAnswerSentinel: tearing down the run's context while a question is pending resolves
// it as a no-answer sentinel rather than hanging.
func TestServeNoAnswerSentinel(t *testing.T) {
	lis, socket := newTestListener(t)
	b := NewBridge(func(_, _ string, _ []string) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = Serve(ctx, lis, b) }()

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	resp, err := dialAndExchange(t, socket, wireRequest{Prompt: "anyone there?"})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if !resp.NoAnswer {
		t.Errorf("resp = %+v, want NoAnswer=true", resp)
	}
}

// TestServeOversizedRequestRefused: a request over maxAskRequestBytes is refused — the connection
// is dropped with no response, never allocated/truncated into something that happens to parse
// (decision 8). The client may see that either as a write failure (server closed its read side
// mid-write) or as a failure to decode a response; either is the refusal.
func TestServeOversizedRequestRefused(t *testing.T) {
	lis, socket := newTestListener(t)
	b := NewBridge(func(_, _ string, _ []string) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, lis, b) }()

	huge := wireRequest{Prompt: string(make([]byte, maxAskRequestBytes*2))}
	_, err := dialAndExchange(t, socket, huge)
	if err == nil {
		t.Fatal("oversized request got a response; want refusal")
	}
}

// TestServeReadDeadlineDropsSilentConnection: a connection that opens and never writes is
// dropped once the read deadline fires — bounded, not left open forever. Drives handleConn
// directly over an in-memory net.Pipe (rather than through Serve's Accept loop) so the deadline
// can be passed in as an argument. It used to shrink the package var instead, which is a data
// race: Serve's handler goroutines read it and no test can join them.
func TestServeReadDeadlineDropsSilentConnection(t *testing.T) {
	b := NewBridge(func(_, _ string, _ []string) error { return nil })
	serverConn, clientConn := net.Pipe()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		handleConn(context.Background(), serverConn, b, 100*time.Millisecond)
	}()

	buf := make([]byte, 1)
	start := time.Now()
	_, readErr := clientConn.Read(buf) // never wrote anything; expect the server to give up
	wg.Wait()

	if readErr == nil {
		t.Fatal("silent connection: read succeeded, want EOF/close once the server's deadline fires")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("connection stayed open for %v; want it dropped near the read deadline", elapsed)
	}
}

// TestServeAcceptedQuestionSurvivesLongerThanReadDeadline is the ordering assertion decision 8
// calls non-negotiable: once decoding succeeds, Bridge.Ask is not bounded by the read deadline —
// an accepted question must survive a wait longer than it. Same net.Pipe pattern as the test
// above, with the deadline passed in for the same reason.
func TestServeAcceptedQuestionSurvivesLongerThanReadDeadline(t *testing.T) {
	const readDeadline = 50 * time.Millisecond

	b := NewBridge(func(_, _ string, _ []string) error { return nil })
	serverConn, clientConn := net.Pipe()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		handleConn(context.Background(), serverConn, b, readDeadline)
	}()

	if err := json.NewEncoder(clientConn).Encode(wireRequest{Prompt: "slow human?"}); err != nil {
		t.Fatalf("encode: %v", err)
	}

	// Answer well after the read deadline would have fired, proving Ask itself is unbounded by it.
	go func() {
		time.Sleep(5 * readDeadline)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if b.Answer("q1", "still here", false) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	var resp wireResponse
	err := json.NewDecoder(clientConn).Decode(&resp)
	_ = clientConn.Close() // the server now waits for the sandbox to close (lingerUntilPeerCloses)
	wg.Wait()

	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.NoAnswer || resp.Response != "still here" {
		t.Errorf("resp = %+v, want {still here false}", resp)
	}
}

// answerFirstQuestion answers q1 as soon as Ask registers it, for the tests that only care what
// happens to the connection after the response has been written.
func answerFirstQuestion(t *testing.T, b *Bridge, response string) {
	t.Helper()
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if b.Answer("q1", response, false) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

// TestServeDoesNotCloseBeforeTheSandboxDoes pins the property msb's relay forced onto this
// channel: once the response is written, the host leaves the connection open and lets the sandbox
// close it. Closing first loses the answer — hack/msb-probes/p1-vsock-nonroot.sh measured 21 of 75
// round trips completing with the host closing first against 25 of 25 with it waiting (msb 0.6.16,
// 2026-09-02) — and the loss is invisible to the guest, which sees EOF with nothing read and
// cannot tell it from a host that never answered. Same net.Pipe + WaitGroup-join pattern as the
// deadline tests above.
func TestServeDoesNotCloseBeforeTheSandboxDoes(t *testing.T) {
	b := NewBridge(func(_, _ string, _ []string) error { return nil })
	serverConn, clientConn := net.Pipe()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		handleConn(context.Background(), serverConn, b, askReadDeadline)
	}()
	answerFirstQuestion(t, b, "still open")

	if err := json.NewEncoder(clientConn).Encode(wireRequest{Prompt: "who closes?"}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var resp wireResponse
	if err := json.NewDecoder(clientConn).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Response != "still open" {
		t.Fatalf("resp = %+v, want the answer back", resp)
	}

	// The answer is in hand, so the exchange is over as far as the protocol is concerned. The
	// connection must nevertheless still be open: a read has to hit its own deadline rather than
	// report the server's close.
	if err := clientConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, err := clientConn.Read(make([]byte, 1))
	if !os.IsTimeout(err) {
		_ = clientConn.Close()
		wg.Wait()
		t.Fatalf("read after the response = %v, want a timeout: the host closed first, which is exactly what msb's relay drops the reply for", err)
	}

	_ = clientConn.Close() // the sandbox closes, as krayt-ask does once it has its answer
	wg.Wait()              // and only then does the handler return
}

// TestLingerIsBounded: a sandbox that takes its answer and then goes silent instead of closing
// does not pin the connection open. It drives lingerUntilPeerCloses directly with a short timeout
// rather than shrinking a package-level default — the version that did the latter was a data race
// against handler goroutines still reading that default from an earlier test, which is why both
// timeouts are constants passed as arguments now.
func TestLingerIsBounded(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })

	// Set before the server closes: net.Pipe refuses SetReadDeadline once either end is closed, so
	// a deadline armed afterwards would fail the test on plumbing rather than on the behaviour.
	if err := clientConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		lingerUntilPeerCloses(serverConn, 50*time.Millisecond)
		_ = serverConn.Close() // what handleConn's deferred Close does once the wait is over
		done <- time.Since(start)
	}()

	select {
	case elapsed := <-done:
		if elapsed > time.Second {
			t.Errorf("waited %v on a peer that never closed; want the wait bounded near its timeout", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lingerUntilPeerCloses never returned: a sandbox that goes silent instead of closing would pin the connection open")
	}

	// And the connection is actually closed afterwards, not merely abandoned.
	if _, err := clientConn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("client read after the linger = %v, want io.EOF from the server's close", err)
	}
}

// TestBridgePendingBound: past maxPendingQuestions concurrently in-flight questions, a new Ask
// gets the no-answer sentinel immediately rather than a queue slot (decision 8).
func TestBridgePendingBound(t *testing.T) {
	b := NewBridge(func(_, _ string, _ []string) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	started := make(chan struct{}, maxPendingQuestions)
	for range maxPendingQuestions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started <- struct{}{}
			_, _, _ = b.Ask(ctx, "filler", nil)
		}()
	}
	for range maxPendingQuestions {
		<-started
	}
	// Give the goroutines a moment to actually register under Bridge.mu.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		n := len(b.pending)
		b.mu.Unlock()
		if n == maxPendingQuestions {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	resp, noAnswer, err := b.Ask(context.Background(), "one too many", nil)
	if err != nil {
		t.Fatalf("Ask past the bound: %v", err)
	}
	if !noAnswer || resp != "" {
		t.Errorf("Ask past the bound = (%q, %v), want (\"\", true)", resp, noAnswer)
	}

	cancel() // unblock the filler goroutines
	wg.Wait()
}

// TestListenCreatesPrivateDirAndSocket: the parent dir is 0700 and the socket is 0600 inside it
// (decision 10).
func TestListenCreatesPrivateDirAndSocket(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run-state")
	lis, err := Listen(dir)
	if err != nil {
		t.Skipf("unix socket bind unavailable in this sandbox: %v", err)
	}
	defer func() { _ = lis.Close() }()

	dfi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !dfi.IsDir() || dfi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want a 0700 directory", dfi.Mode())
	}

	sockPath := filepath.Join(dir, "ask.sock")
	sfi, err := os.Lstat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if sfi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, want 0600", sfi.Mode().Perm())
	}
	if st, ok := sfi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		t.Errorf("socket uid = %d, want %d", st.Uid, os.Getuid())
	}
}

// TestListenRefusesHostileDir: a pre-existing world-writable directory at the target path is
// refused rather than reused — reusing harden-vfkit-socket-dir.md's sockroot.Ensure check rather
// than a second one (decision 4/12).
func TestListenRefusesHostileDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run-state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(dir); err == nil {
		t.Fatal("Listen accepted a 0777 pre-existing dir; want refusal")
	}
}

// TestBridgeRedactsAtPushBoundary: decision 11 — a prompt/choices carrying a secret value comes
// back redacted, applied in the push closure the caller supplies to NewBridge (the host already
// holds the values, so this costs nothing at the boundary where questions are persisted), the
// host-side equivalent of internal/guest/service.go's guest-side redaction.
func TestBridgeRedactsAtPushBoundary(t *testing.T) {
	redactor := secrets.NewRedactor(secrets.Values(map[string]string{"API_KEY": "sk-super-secret"}))

	var gotPrompt string
	var gotChoices []string
	push := func(_, prompt string, choices []string) error {
		gotPrompt = string(redactor.Redact([]byte(prompt)))
		if choices != nil {
			gotChoices = make([]string, len(choices))
			for i, c := range choices {
				gotChoices[i] = string(redactor.Redact([]byte(c)))
			}
		}
		return nil
	}
	b := NewBridge(push)
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if b.Answer("q1", "ok", false) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if _, _, err := b.Ask(context.Background(), "the key is sk-super-secret, ok?", []string{"sk-super-secret", "no"}); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if want := "the key is " + secrets.RedactionMarker + ", ok?"; gotPrompt != want {
		t.Errorf("prompt = %q, want %q", gotPrompt, want)
	}
	for _, c := range gotChoices {
		if c == "sk-super-secret" {
			t.Errorf("choice not redacted: %q", gotChoices)
		}
	}
}
