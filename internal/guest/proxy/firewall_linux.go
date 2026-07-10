//go:build linux

package proxy

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// egressRuleset is the §6.6 nftables lock: default-deny egress in the inet family (so IPv4 and
// IPv6 are both covered), permitting only loopback, the proxy's own uid (proxyd), and
// established/related return traffic. The container's only path out is therefore via the proxy
// (set through HTTP_PROXY/HTTPS_PROXY); direct sockets are dropped, closing the raw-socket bypass.
//
// SAFETY INVARIANT — the `skuid "proxyd"` accept is unbypassable ONLY BECAUSE the container cannot
// become proxyd. That is not guaranteed by this rule; it is guaranteed by the container OCI spec
// (§6.10, harden-container-oci-spec.md), which drops CAP_SETUID/CAP_SETGID and forbids running as
// root (enforced, not convention — see withEnforceNonRoot in internal/guest/runner). Without that,
// a container process could learn proxyd's numeric uid from /proc/net/tcp (the :3128 listener's
// owner is visible in the shared netns) or brute-force the small system-uid range, setuid() to it,
// and send egress that matches this accept — bypassing the L7 allowlist entirely (finding #1).
// If you ever loosen the container caps or allow root, this lock silently reopens.
//
// SINGLE-NETNS ASSUMPTION — this rule is correct only while the container shares the VM's network
// namespace (§6.6, oci.WithHostNamespace in the runner), so its sockets traverse this `output` hook.
// If a future change gives the container its own netns, the `output` hook will no longer see the
// container's traffic and a `forward` chain (plus the same uid/veth reasoning) would be required.
//
// The proxyd user must exist in the image (added by the flake) so `skuid "proxyd"` resolves.
const egressRuleset = `table inet krayt_egress {
  chain output {
    type filter hook output priority 0; policy drop;
    oif "lo" accept
    meta skuid "proxyd" accept
    ct state established,related accept
  }
}`

// ApplyFirewall installs the egress lock for the policy mode via `nft` (§6.6). For `full`
// (explicit opt-in) it removes any lock so all egress is allowed; for `allowlist`/`none` it
// installs the default-deny ruleset so only the proxy can leave the VM. The proxy then
// enforces the per-host allowlist (or denies everything for `none`) at L7.
func ApplyFirewall(ctx context.Context, mode string) error {
	if mode == ModeFull {
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
