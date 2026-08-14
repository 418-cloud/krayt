//go:build linux

// Package proxy is the GUEST side of krayt's egress control (§6.6). Since
// move-egress-proxy-to-host.md it is no longer the L7 enforcement point — that is
// internal/proxy now, running as a separate process on the HOST (`krayt __egress-proxy`,
// internal/cli). What remains here is: the simplified L3 nftables lock (firewall_linux.go,
// loopback-only, keyed on no uid) and the Controller that starts krayt-vsock-forward — a dumb
// TCP<->vsock pipe with no policy of its own — as the dedicated `proxyd` uid and wires its
// listen address into the container's HTTP_PROXY/HTTPS_PROXY env (controller_linux.go).
package proxy

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/418-cloud/krayt/internal/guest"
	"github.com/418-cloud/krayt/internal/provider"
)

const (
	defaultProxyUser = "proxyd"
	defaultListen    = "127.0.0.1:3128"
	defaultBinary    = "krayt-vsock-forward"
)

// caCertPath is the contract path a compliant container entrypoint reads (§8.2,
// add-tls-mitm-credential-injection.md §5): world-readable (0644) since it is public, and never
// written when network.mitm is false, so a mitm-off run has no /run/krayt/ca.crt at all.
const caCertPath = "/run/krayt/ca.crt"

// Controller is the linux guest.Network (§6.6): at run start it launches the guest-side
// egress forwarder as the dedicated proxyd uid and installs the nftables lock, returning the
// HTTP(S)_PROXY env for the container. The forwarder is tied to the run context, so it exits
// when the run ends.
//
// Since move-egress-proxy-to-host.md this no longer starts the L7 allowlist proxy — that runs
// on the HOST now, as `krayt __egress-proxy` (internal/proxy, internal/orchestrator), reached
// over the guest→host vsock channel (provider.EgressPort). What this controller starts is
// krayt-vsock-forward, a dumb TCP<->vsock pipe (cmd/krayt-vsock-forward) that parses nothing
// and enforces nothing — the container's HTTP_PROXY points at it purely so its traffic stays
// on loopback, which is what the simplified nftables lock (firewall_linux.go) keys on.
type Controller struct {
	Binary    string // krayt-vsock-forward path or name (default: resolved on PATH)
	User      string // forwarder uid name (default: proxyd)
	Listen    string // forwarder listen address (default: 127.0.0.1:3128)
	VsockPort uint32 // host egress vsock port the forwarder dials (default: provider.EgressPort)
}

// NewController returns a Controller with the production defaults.
func NewController() *Controller {
	return &Controller{Binary: defaultBinary, User: defaultProxyUser, Listen: defaultListen, VsockPort: provider.EgressPort}
}

// Apply implements guest.Network.
func (c *Controller) Apply(ctx context.Context, policy guest.NetworkPolicy) (map[string]string, error) {
	binary, username, listen, vsockPort := c.Binary, c.User, c.Listen, c.VsockPort
	if binary == "" {
		binary = defaultBinary
	}
	if username == "" {
		username = defaultProxyUser
	}
	if listen == "" {
		listen = defaultListen
	}
	if vsockPort == 0 {
		vsockPort = provider.EgressPort
	}

	uid, gid, err := lookupUser(username)
	if err != nil {
		return nil, err
	}

	// Run the forwarder as proxyd. The nftables lock no longer keys on this uid (it only
	// needs loopback to be open, §7) — this is now defense in depth only: the one guest
	// process that touches container-controlled bytes still runs as a dedicated non-root
	// uid rather than as the guest-agent's own root identity. Keep it; a future reader
	// should not delete this as vestigial (see also images/flake.nix's proxyd user).
	cmd := exec.CommandContext(ctx, binary,
		"--listen", listen,
		"--vsock-port", strconv.FormatUint(uint64(vsockPort), 10),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid}}
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr // surface forwarder logs into the agent journal
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("proxy: start krayt-vsock-forward as %s: %w", username, err)
	}

	// Reap the forwarder so that when CommandContext kills it at run end (or via stopForwarder
	// on the error paths below) it is not left as a zombie. Harmless in the one-run-per-VM
	// model today, but it prevents a per-run zombie/goroutine leak once a warm-VM pool reuses a
	// long-lived guest-agent across runs (§15).
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	stopForwarder := func() {
		_ = cmd.Process.Kill()
		select {
		case <-waited:
		case <-time.After(2 * time.Second):
		}
	}

	if err := ApplyFirewall(ctx, policy.Mode); err != nil {
		stopForwarder()
		return nil, err
	}
	if err := waitListening(ctx, listen, 5*time.Second); err != nil {
		stopForwarder()
		return nil, fmt.Errorf("proxy: %w", err)
	}

	u := "http://" + listen
	env := map[string]string{
		"HTTP_PROXY": u, "HTTPS_PROXY": u, "http_proxy": u, "https_proxy": u,
		"NO_PROXY": "localhost,127.0.0.1", "no_proxy": "localhost,127.0.0.1",
	}
	if err := applyCACert(policy.CACert, caCertPath, env); err != nil {
		stopForwarder()
		return nil, err
	}
	return env, nil
}

// applyCACert writes the run's ephemeral MITM CA public cert to path (caCertPath in production;
// overridable here so tests don't need real root-owned /run access) and adds the KRAYT_CA_CERT
// contract var plus best-effort SSL_CERT_FILE/REQUESTS_CA_BUNDLE/NODE_EXTRA_CA_CERTS to env
// (§8.2, §5 of add-tls-mitm-credential-injection.md). A no-op when caCert is empty
// (network.mitm: false) — env gains no new keys, so a mitm-off run's container environment is
// byte-identical to before this task.
//
// SSL_CERT_FILE/REQUESTS_CA_BUNDLE here point at the krayt CA ALONE, not a distro bundle — that
// would break verification for anything on the passthrough list. This is the guest's
// best-effort default; the container entrypoint (§8.2, distro-specific, not the guest's job)
// overrides both to point at a concatenated (distro bundle + krayt CA) file instead.
// NODE_EXTRA_CA_CERTS is genuinely additive, so it can point at the krayt CA directly with no
// further work — required for all three current agent images, which are all node-based and do
// not read the system trust store at all.
func applyCACert(caCert []byte, path string, env map[string]string) error {
	if len(caCert) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("proxy: create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, caCert, 0o644); err != nil {
		return fmt.Errorf("proxy: write %s: %w", path, err)
	}
	env["KRAYT_CA_CERT"] = path
	env["SSL_CERT_FILE"] = path
	env["REQUESTS_CA_BUNDLE"] = path
	env["NODE_EXTRA_CA_CERTS"] = path
	return nil
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

// waitListening blocks until the forwarder accepts a connection on addr or the timeout passes.
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
	return fmt.Errorf("forwarder did not start listening on %s within %s", addr, timeout)
}

var _ guest.Network = (*Controller)(nil)
