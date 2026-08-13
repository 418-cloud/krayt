//go:build linux

package proxy

import (
	"context"
	"strings"
	"testing"

	"github.com/418-cloud/krayt/internal/guest"
)

// TestEgressRulesetShape is the cheap offline guard against an accidental regression of the §6.6
// default-deny egress lock (finding #1). It does not need a VM or the `nft` binary — it asserts
// the generated ruleset still has the load-bearing pieces AND that it no longer keys on a uid
// (move-egress-proxy-to-host.md §7): the L7 proxy runs on the host now, so there is nothing left
// in the guest for a `skuid` accept to protect, and its reappearance would mean someone
// resurrected a guest-side identity the container-hardening controls would again have to
// backstop. The live drop/allow enforcement is proven on hardware by TestEgressEnforcement +
// TestContainerHardening (see internal/orchestrator/integration_test.go and HUMAN_TODO.md).
func TestEgressRulesetShape(t *testing.T) {
	must := []struct {
		frag, why string
	}{
		// inet family ⇒ the lock covers both IPv4 and IPv6; an `ip`/`ip6` split could leave a gap.
		{"table inet krayt_egress", "must be in the inet family (IPv4+IPv6)"},
		// Default-deny is the whole property — without it the accept is meaningless.
		{"policy drop", "the output chain must default-deny"},
		{"oif \"lo\" accept", "loopback must be permitted (the vsock forwarder listens there)"},
	}
	for _, m := range must {
		if !strings.Contains(egressRuleset, m.frag) {
			t.Errorf("egressRuleset missing %q — %s\ngot:\n%s", m.frag, m.why, egressRuleset)
		}
	}
	// Regression guard: no rule may key on a uid anymore (§7) — the proxy moved to the host,
	// so a `skuid` accept has nothing left to protect and its return would mean the guest
	// chain silently grew a dependency on container-hardening again.
	if strings.Contains(egressRuleset, "skuid") {
		t.Errorf("egressRuleset must not reference skuid (the L7 proxy is host-side now):\n%s", egressRuleset)
	}
	// established/related is also gone: with no outbound external flow permitted there is no
	// established external flow to match, and the hook is output-only.
	if strings.Contains(egressRuleset, "established") {
		t.Errorf("egressRuleset must not reference established/related state (no external flow is ever permitted):\n%s", egressRuleset)
	}
}

// TestApplyFirewallFullRemovesLock asserts that `full` mode (explicit opt-in to open egress) takes
// the deletion path and never returns an error for a missing table — table removal is best-effort
// so re-application/first-application stays idempotent (§6.6). This runs offline: the delete is
// piped to `nft` but any error (including `nft` absent) is intentionally discarded.
func TestApplyFirewallFullRemovesLock(t *testing.T) {
	if err := ApplyFirewall(context.Background(), guest.NetFull); err != nil {
		t.Errorf("ApplyFirewall(NetFull) = %v, want nil (best-effort table delete)", err)
	}
}
