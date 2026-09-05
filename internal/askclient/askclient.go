// Package askclient is the in-sandbox client half of the agent → human question channel
// (§6.13): the wire protocol and dial logic `krayt-ask` uses to reach the host-side
// internal/askbridge.Bridge. run-tasks-on-microsandbox.md split it out of the pre-msb in-guest
// ask package as a standalone client-only package, alongside the (now deleted) in-guest
// Bridge/Serve half, so cmd/krayt-ask — which survives the cut-over — depends on neither the
// deleted guest agent package nor internal/askbridge's server-side dependencies.
//
// The wire protocol (newline-delimited JSON, one request/response per connection) matches
// internal/askbridge's server half exactly.
package askclient

import (
	"encoding/json"
	"net"
)

// wireRequest / wireResponse mirror internal/askbridge's server-side structs — the same
// newline-delimited JSON protocol, client side.
type wireRequest struct {
	Prompt  string   `json:"prompt"`
	Choices []string `json:"choices,omitempty"`
}

type wireResponse struct {
	Response string `json:"response"`
	NoAnswer bool   `json:"no_answer"`
}

// OverSocket connects to the bridge named by socket — a bare unix path, or a vsock://cid:port URL
// (dial-ask-channel-over-vsock.md decision 2) — submits one question, and returns the answer. It
// is the client side of the protocol used by the `krayt-ask` CLI and its --mcp front-end. A
// malformed vsock:// value returns an error wrapping ErrMalformedSocket, distinct from a
// dial/connection failure, so a caller can tell "the socket value is wrong" apart from "no human
// is available" (§6.13).
func OverSocket(socket, prompt string, choices []string) (string, bool, error) {
	addr, err := parseDialAddr(socket)
	if err != nil {
		return "", false, err
	}
	conn, err := dial(addr)
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

// dial connects to addr, dispatching to a unix dial (OS-agnostic) or the platform's vsock dial
// (dialVsock, confined to a build-tagged file — see dial_vsock_linux.go / dial_vsock_other.go).
func dial(addr dialAddr) (net.Conn, error) {
	if addr.unix {
		return net.Dial("unix", addr.path)
	}
	return dialVsock(addr.cid, addr.port)
}
