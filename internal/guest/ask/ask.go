// Package ask is the in-VM bridge of the agent → human question channel (§6.13): the stable,
// agent-agnostic core. Something inside the container (the MCP server or the `krayt-ask` CLI —
// Phase 5 front-ends) connects to a local unix socket and submits a question; the Bridge hands
// it to the guest-agent, which pushes it to the host as a RunEvent.Question and blocks until
// the host calls Answer(question_id, …) (or the run is torn down), then returns the answer into
// the container. It knows nothing about which agent is running.
package ask

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
)

// wireRequest / wireResponse are the newline-delimited JSON protocol spoken over the unix
// socket: the container writes one request, reads one response, per connection.
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

// Bridge routes questions from inside the VM to the host and answers back. One Bridge backs
// one run. push sends the question to the host (the guest-agent wraps it as a
// RunEvent.Question on the Start stream); it must be safe for concurrent use.
type Bridge struct {
	push func(id, prompt string, choices []string) error

	mu      sync.Mutex
	seq     int
	pending map[string]chan answer
}

// NewBridge returns a Bridge that emits questions via push.
func NewBridge(push func(id, prompt string, choices []string) error) *Bridge {
	return &Bridge{push: push, pending: map[string]chan answer{}}
}

// Ask registers a question, pushes it to the host, and blocks until the host answers it or
// ctx is done (the run being torn down → treated as a no-answer sentinel so the caller can
// fall back gracefully, §6.13). It is called by Serve per container connection.
func (b *Bridge) Ask(ctx context.Context, prompt string, choices []string) (string, bool, error) {
	b.mu.Lock()
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
// with that id is waiting (already answered, timed out, or unknown) — the caller (the guest
// Answer RPC) reports that back so a duplicate answer is a harmless no-op.
func (b *Bridge) Answer(id, response string, noAnswer bool) bool {
	b.mu.Lock()
	ch, ok := b.pending[id]
	b.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- answer{response: response, noAnswer: noAnswer}:
		return true
	default:
		return false // already answered
	}
}

// Serve accepts container connections on ln and bridges each to Ask until ctx is canceled.
// Each connection carries exactly one question/answer exchange.
func Serve(ctx context.Context, ln net.Listener, b *Bridge) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close() // unblock Accept on shutdown
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // clean shutdown
			}
			return err
		}
		go handleConn(ctx, conn, b)
	}
}

func handleConn(ctx context.Context, conn net.Conn, b *Bridge) {
	defer func() { _ = conn.Close() }()
	var req wireRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	resp, noAnswer, err := b.Ask(ctx, req.Prompt, req.Choices)
	if err != nil {
		noAnswer = true // run torn down mid-question; let the agent fall back
	}
	_ = json.NewEncoder(conn).Encode(wireResponse{Response: resp, NoAnswer: noAnswer})
}

// OverSocket connects to a bridge unix socket, submits one question, and returns the
// answer. It is the client side of the protocol used by the `krayt-ask` CLI (Phase 5) and by
// tests to drive a stubbed agent question.
func OverSocket(socket, prompt string, choices []string) (string, bool, error) {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = conn.Close() }()
	if err := json.NewEncoder(conn).Encode(wireRequest{Prompt: prompt, Choices: choices}); err != nil {
		return "", false, err
	}
	var resp wireResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return "", false, err
	}
	return resp.Response, resp.NoAnswer, nil
}
