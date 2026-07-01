//go:build linux

package proxy

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/418-cloud/krayt/internal/guest"
)

const (
	defaultProxyUser = "proxyd"
	defaultListen    = "127.0.0.1:3128"
	defaultBinary    = "krayt-proxy"
)

// Controller is the linux guest.Network (§6.6): at run start it launches the allowlist
// proxy as the dedicated proxyd uid and installs the nftables lock, returning the
// HTTP(S)_PROXY env for the container. The proxy is tied to the run context, so it exits
// when the run ends.
type Controller struct {
	Binary string // krayt-proxy path or name (default: resolved on PATH)
	User   string // proxy uid name (default: proxyd)
	Listen string // proxy listen address (default: 127.0.0.1:3128)
}

// NewController returns a Controller with the production defaults.
func NewController() *Controller {
	return &Controller{Binary: defaultBinary, User: defaultProxyUser, Listen: defaultListen}
}

// Apply implements guest.Network.
func (c *Controller) Apply(ctx context.Context, policy guest.NetworkPolicy) (map[string]string, error) {
	binary, username, listen := c.Binary, c.User, c.Listen
	if binary == "" {
		binary = defaultBinary
	}
	if username == "" {
		username = defaultProxyUser
	}
	if listen == "" {
		listen = defaultListen
	}

	uid, gid, err := lookupUser(username)
	if err != nil {
		return nil, err
	}

	// Run the proxy as proxyd so the nftables `skuid "proxyd"` rule (and only that rule)
	// permits its egress; the container, running as a different uid, cannot bypass it.
	cmd := exec.CommandContext(ctx, binary,
		"--listen", listen,
		"--mode", policy.Mode,
		"--allow", strings.Join(policy.Allow, ","),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid}}
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr // surface proxy logs into the agent journal
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("proxy: start krayt-proxy as %s: %w", username, err)
	}

	// Reap the proxy so that when CommandContext kills it at run end (or via stopProxy on the
	// error paths below) it is not left as a zombie. Harmless in the one-run-per-VM model
	// today, but it prevents a per-run zombie/goroutine leak once a warm-VM pool reuses a
	// long-lived guest-agent across runs (§15).
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	stopProxy := func() {
		_ = cmd.Process.Kill()
		select {
		case <-waited:
		case <-time.After(2 * time.Second):
		}
	}

	if err := ApplyFirewall(ctx, policy.Mode); err != nil {
		stopProxy()
		return nil, err
	}
	if err := waitListening(ctx, listen, 5*time.Second); err != nil {
		stopProxy()
		return nil, fmt.Errorf("proxy: %w", err)
	}

	u := "http://" + listen
	return map[string]string{
		"HTTP_PROXY": u, "HTTPS_PROXY": u, "http_proxy": u, "https_proxy": u,
		"NO_PROXY": "localhost,127.0.0.1", "no_proxy": "localhost,127.0.0.1",
	}, nil
}

func lookupUser(name string) (uint32, uint32, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, fmt.Errorf("proxy: lookup user %s: %w", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("proxy: parse uid: %w", err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("proxy: parse gid: %w", err)
	}
	return uint32(uid), uint32(gid), nil
}

// waitListening blocks until the proxy accepts a connection on addr or the timeout passes.
func waitListening(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("proxy did not start listening on %s within %s", addr, timeout)
}

var _ guest.Network = (*Controller)(nil)
