package guest

import "context"

// Network policy modes (§6.6). String-valued to mirror the proto enum and internal/proxy's
// Mode* without importing that package here (which would cycle: the linux Network controller
// lives in internal/guest/proxy and imports this package).
const (
	NetAllowlist = "allowlist"
	NetFull      = "full"
	NetNone      = "none"
)

// NetworkPolicy is the per-task egress policy handed to the Network controller (§6.6).
type NetworkPolicy struct {
	Mode  string
	Allow []string
}

// Network configures per-task egress (§6.6): it starts krayt-vsock-forward (a dumb TCP<->vsock
// pipe to the host-side allowlist proxy, since move-egress-proxy-to-host.md) as the dedicated
// proxyd uid, applies the simplified loopback-only nftables lock, and returns the env vars
// (HTTP_PROXY/HTTPS_PROXY/NO_PROXY) to inject into the container. It runs for the lifetime of
// ctx. The real implementation is linux-only (internal/guest/proxy); in-process tests run
// without it, since the fake runner performs no real egress. Allow is carried on this type for
// the proto round-trip, but is no longer consulted guest-side — Mode alone shapes the guest's
// nftables lock; the L7 allowlist decision now lives entirely on the host (internal/proxy).
type Network interface {
	Apply(ctx context.Context, policy NetworkPolicy) (env map[string]string, err error)
}
