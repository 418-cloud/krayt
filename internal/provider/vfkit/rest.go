//go:build darwin

package vfkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/crc-org/vfkit/pkg/rest/define"
)

// restClient talks to a running vfkit's lifecycle REST API, which vfkit serves over a
// host unix socket (`--restful-uri unix://…`). See vfkit pkg/rest (GET/POST /vm/state).
type restClient struct {
	sock string
	http *http.Client
}

func newRESTClient(sock string) *restClient {
	return &restClient{
		sock: sock,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
			},
			Timeout: 10 * time.Second,
		},
	}
}

// stop requests a graceful guest stop via POST /vm/state {"state":"Stop"}.
func (c *restClient) stop(ctx context.Context) error {
	return c.setState(ctx, define.Stop)
}

func (c *restClient) setState(ctx context.Context, state define.StateChange) error {
	body, err := json.Marshal(define.VMState{State: string(state)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://vfkit/vm/state", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("vfkit rest: set state %q: %w", state, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vfkit rest: set state %q: status %d: %s", state, resp.StatusCode, bytes.TrimSpace(b))
	}
	return nil
}

// state returns the current VM state via GET /vm/state.
func (c *restClient) state(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://vfkit/vm/state", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("vfkit rest: get state: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("vfkit rest: get state: status %d", resp.StatusCode)
	}
	var vs define.VMState
	if err := json.NewDecoder(resp.Body).Decode(&vs); err != nil {
		return "", fmt.Errorf("vfkit rest: decode state: %w", err)
	}
	return vs.State, nil
}
