// Package askbridge is the host-side half of the agent → human question channel under msb
// (dial-ask-channel-over-vsock.md, §6.13): the moved, hardened continuation of
// internal/guest/ask, now running in the krayt host process instead of inside the guest.
// krayt-ask (or its --mcp front-end) inside the sandbox dials AF_VSOCK straight to the host; msb
// bridges that to a host unix socket (Listen); Serve accepts connections on it and answers each
// with one question/answer exchange against a Bridge — the same newline-delimited JSON wire
// protocol, wire structs, and pending-question map internal/guest/ask has always used, unchanged.
//
// This is additive, not a cut-over (dial-ask-channel-over-vsock.md, "Sequencing — additive
// only"): internal/guest/ask and cmd/krayt-vsock-forward stay live until
// run-tasks-on-microsandbox.md deletes both. The redaction that guest-side wiring applies before
// push (internal/guest/service.go) moves with the caller, not into this package: the host already
// holds every secret value, so whatever constructs a Bridge here can redact in its own push
// closure before persisting a question — see NewBridge.
//
// Serve now runs in the host process, reading bytes an arbitrary sandbox process wrote, so it
// carries three bounds internal/guest/ask never needed under §10's per-VM resource limits
// (decision 8): a byte cap on one request (maxAskRequestBytes), a read deadline around decoding
// only — never around Bridge.Ask, which legitimately blocks for the run's whole
// --question-timeout — and a cap on in-flight questions (maxPendingQuestions), past which a new
// question gets the no-answer sentinel immediately rather than a queue slot.
package askbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/418-cloud/krayt/internal/sockroot"
)

// wireRequest / wireResponse are the newline-delimited JSON protocol spoken over the socket: the
// sandbox writes one request, reads one response, per connection. Unchanged from
// internal/guest/ask (dial-ask-channel-over-vsock.md decision 5).
type wireRequest struct {
	Prompt  string   `json:"prompt"`
	Choices []string `json:"choices,omitempty"`
}

type wireResponse struct {
	Response string `json:"response"`
	NoAnswer bool   `json:"no_answer"`
}

// answer is what the host delivers for a pending question.
type answer struct {
	response string
	noAnswer bool
}

// maxPendingQuestions bounds Bridge.pending (decision 8): past this many concurrently in-flight
// questions, a new Ask gets the no-answer sentinel immediately rather than a queue slot. Every
// admitted question is sandbox-driven state on the host — a questions/<id>.json file, a run
// flipped to `waiting`, a desktop notification (§6.13) — so bounding admission bounds all three
// from the one place that decides whether a question is admitted at all.
const maxPendingQuestions = 32

// Bridge routes questions from a sandboxed run to the host and answers back. One Bridge backs
// one run. push sends the question onward — writing questions/<id>.json, flipping the run to
// `waiting`, firing the desktop notification (orchestrator-level; wired at
// run-tasks-on-microsandbox.md's cut-over) — and is the place a caller redacts secret values
// before they are persisted (decision 11), since the host already holds them. It must be safe
// for concurrent use.
type Bridge struct {
	push       func(id, prompt string, choices []string) error
	onResolved func(id string)

	mu      sync.Mutex
	seq     int
	pending map[string]chan answer
}

// NewBridge returns a Bridge that emits questions via push.
func NewBridge(push func(id, prompt string, choices []string) error) *Bridge {
	return &Bridge{push: push, pending: map[string]chan answer{}}
}

// OnResolved registers a callback invoked when a pending question is answered (§6.13), so the
// caller can flip a `waiting` run back to `running` precisely rather than inferring resumption
// off the log stream. Set once before the Bridge handles any Answer.
func (b *Bridge) OnResolved(fn func(id string)) { b.onResolved = fn }

// Ask registers a question, pushes it to the host, and blocks until the host answers it or ctx is
// done (the run being torn down → treated as a no-answer sentinel so the caller can fall back
// gracefully, §6.13). It is called by Serve per sandbox connection.
func (b *Bridge) Ask(ctx context.Context, prompt string, choices []string) (string, bool, error) {
	b.mu.Lock()
	if len(b.pending) >= maxPendingQuestions {
		b.mu.Unlock()
		// Bound reached (decision 8): the no-answer sentinel, not a queue slot — an unbounded
		// pending map is not only memory, it is unbounded questions/*.json writes and desktop
		// notifications, all sandbox-driven.
		return "", true, nil
	}
	b.seq++
	id := fmt.Sprintf("q%d", b.seq)
	ch := make(chan answer, 1)
	b.pending[id] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
	}()

	if err := b.push(id, prompt, choices); err != nil {
		return "", false, err
	}
	select {
	case a := <-ch:
		return a.response, a.noAnswer, nil
	case <-ctx.Done():
		return "", true, ctx.Err()
	}
}

// Answer delivers the host's response to a pending question. It returns false if no question
// with that id is waiting (already answered, timed out, or unknown) — the caller reports that
// back so a duplicate answer is a harmless no-op.
func (b *Bridge) Answer(id, response string, noAnswer bool) bool {
	b.mu.Lock()
	ch, ok := b.pending[id]
	b.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- answer{response: response, noAnswer: noAnswer}:
		if b.onResolved != nil {
			b.onResolved(id)
		}
		return true
	default:
		return false // already answered
	}
}

// maxAskRequestBytes bounds one request read off the wire — a named constant, the same idiom as
// internal/orchestrator/egressproxy.go's maxCACertPEMBytes: a question is a few hundred bytes at
// most, so this is generous headroom, not a real limit. Over-long input is a refusal, not a
// truncation (decision 8).
const maxAskRequestBytes = 64 << 10

// askReadDeadline bounds how long Serve waits for a connection to finish writing its request,
// before Bridge.Ask is ever called (decision 8: a stalled connection is dropped here, while an
// accepted question survives a wait far longer than this once decoding has already succeeded) —
// mirroring internal/provider/firecracker/vsock.go's handshakeTimeout.
//
// Both timeouts here are constants passed down as arguments rather than vars a test can shrink in
// place. The shrink-the-package-var pattern is a data race in this package and was caught as one:
// Serve spawns handleConn goroutines the test that started them cannot join, so a later test's
// assignment races their read, with no happens-before edge even once they have finished.
const askReadDeadline = 10 * time.Second

// askLingerTimeout bounds how long handleConn waits for the sandbox to close the connection once
// the response has been written (lingerUntilPeerCloses).
const askLingerTimeout = 5 * time.Second

// Serve accepts connections on lis and answers each with one question exchange against b. One
// exchange per connection; it never keeps state between them.
func Serve(ctx context.Context, lis net.Listener, b *Bridge) error {
	go func() {
		<-ctx.Done()
		_ = lis.Close() // unblock Accept on shutdown
	}()
	for {
		conn, err := lis.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // clean shutdown
			}
			return err
		}
		go handleConn(ctx, conn, b, askReadDeadline)
	}
}

// handleConn answers one connection. readDeadline is a parameter, not a read of the package
// constant, so a test can bound it without writing to shared state these goroutines race on.
func handleConn(ctx context.Context, conn net.Conn, b *Bridge, readDeadline time.Duration) {
	defer func() { _ = conn.Close() }()

	// The read deadline covers ONLY decoding the request — never Ask, which legitimately blocks
	// for the run's whole --question-timeout (minutes). A connection-wide deadline would break
	// the feature it is meant to protect (decision 8's non-negotiable ordering).
	if err := conn.SetReadDeadline(time.Now().Add(readDeadline)); err != nil {
		return
	}
	var req wireRequest
	dec := json.NewDecoder(io.LimitReader(conn, maxAskRequestBytes+1))
	if err := dec.Decode(&req); err != nil {
		return // oversized (refusal, not a truncation), malformed, or the deadline fired
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return
	}

	resp, noAnswer, err := b.Ask(ctx, req.Prompt, req.Choices)
	if err != nil {
		noAnswer = true // run torn down mid-question; let the agent fall back
	}
	if err := json.NewEncoder(conn).Encode(wireResponse{Response: resp, NoAnswer: noAnswer}); err != nil {
		return
	}
	lingerUntilPeerCloses(conn, askLingerTimeout)
}

// lingerUntilPeerCloses holds the connection open until the sandbox closes it — or until
// askLingerTimeout elapses — instead of letting handleConn return straight into its deferred
// Close.
//
// Closing first loses the answer. msb 0.6.16's vsock relay discards a reply that is still in
// flight when the host end of the bridged unix socket closes, and it does so most of the time:
// hack/msb-probes/p1-vsock-nonroot.sh measured 21 of 75 round trips completing with the host
// closing first, against 25 of 25 with the host waiting for the guest (2026-09-02, Apple-Silicon
// Mac; KRAYT_SPEC.md §14 Phase 11's P1 bullet has the per-shape rates). Every loss looked
// identical from the guest — EOF with zero bytes read — after the host had already logged the
// bytes it wrote, so nothing the guest could observe would distinguish it from a host that never
// answered. Waiting for the sandbox to close costs nothing on the transports where the race does
// not exist: krayt-ask reads its reply and closes immediately, so this normally returns in
// microseconds.
//
// The deadline bounds a sandbox that goes silent instead of closing; the LimitReader bounds one
// that keeps talking. Both are the same fail-open shape as the rest of this package — a
// misbehaving sandbox delays only its own connection.
func lingerUntilPeerCloses(conn net.Conn, timeout time.Duration) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(conn, maxAskRequestBytes))
}

// Listen creates dir (the run's own private state directory, decision 4 — NOT vfkit's shared
// `/tmp/krayt-<uid>` root, which exists only for macOS's sockaddr_un length limit) if necessary,
// reusing sockroot.Ensure's hostile-pre-existing-directory refusal (decision 12) rather than a
// second copy of that check, then binds a unix socket at dir/ask.sock and chmods it 0600 (decision
// 10) — narrower than the in-guest bridge's 0777, which existed only so a non-root container
// could reach a root-owned directory; here the socket lives in the run's own directory on the
// host, so there is no non-root party to widen it for. net.Listen itself never unlinks a
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
