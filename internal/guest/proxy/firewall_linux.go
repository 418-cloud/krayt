//go:build linux

package proxy

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// egressRuleset is the §6.6 nftables lock: default-deny egress, permitting only loopback,
// the proxy's own uid (proxyd), and established/related return traffic. Because the
// container does not run as proxyd, its only path out is via the proxy — direct sockets are
// dropped, closing the raw-socket bypass. The proxyd user must exist in the image (added by
// the flake) so `skuid "proxyd"` resolves.
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
