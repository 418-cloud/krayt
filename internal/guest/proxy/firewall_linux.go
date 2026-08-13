//go:build linux

package proxy

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/418-cloud/krayt/internal/guest"
)

// egressRuleset is the §6.6 L3 lock, in the inet family (so IPv4 and IPv6 are both covered):
// default-deny egress, permitting only loopback. Since the move-egress-proxy-to-host task the
// L7 proxy runs on the HOST, reached over vsock — the container's only path out is
// 127.0.0.1:3128 (krayt-vsock-forward, a dumb pipe with no policy of its own) via
// HTTP_PROXY/HTTPS_PROXY, so "permit loopback, drop everything else" is the entire guest-side
// lock. There is no uid to key on anymore: krayt-vsock-forward parses nothing and enforces
// nothing, so its identity is not load-bearing for this rule (it still runs as the
// non-root `proxyd` uid for defense in depth — see controller_linux.go — but the firewall
// does not depend on that).
//
// SINGLE-NETNS ASSUMPTION — this rule is correct only while the container shares the VM's
// network namespace (§6.6, oci.WithHostNamespace in the runner), so its sockets traverse this
// `output` hook. If a future change gives the container its own netns, the `output` hook will
// no longer see the container's traffic and a `forward` chain (plus veth-based addressing)
// would be required.
const egressRuleset = `table inet krayt_egress {
  chain output {
    type filter hook output priority 0; policy drop;
    oif "lo" accept
  }
}`

// ApplyFirewall installs the egress lock for the policy mode via `nft` (§6.6). For `full`
// (explicit opt-in) it removes any lock so all egress is allowed; for `allowlist`/`none` it
// installs the default-deny ruleset so only loopback (i.e. the forwarder at 127.0.0.1:3128)
// may leave the guest. The host-side proxy then enforces the per-host allowlist (or denies
// everything for `none`) at L7, on the far side of the vsock channel.
func ApplyFirewall(ctx context.Context, mode string) error {
	if mode == guest.NetFull {
		// Best-effort removal; absent table is not an error worth failing the run over.
		_ = nft(ctx, "delete table inet krayt_egress")
		return nil
	}
	// Replace any prior table so re-application is idempotent.
	_ = nft(ctx, "delete table inet krayt_egress")
	if err := nft(ctx, egressRuleset); err != nil {
		return fmt.Errorf("proxy: apply egress firewall: %w", err)
	}
	return nil
}

// nft pipes a ruleset/command to `nft -f -`.
func nft(ctx context.Context, rules string) error {
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
