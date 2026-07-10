//go:build linux

package proxy

import (
	"context"
	"strings"
	"testing"
)

// TestEgressRulesetShape is the cheap offline guard against an accidental regression of the §6.6
// default-deny egress lock (finding #1). It does not need a VM or the `nft` binary — it asserts
// the generated ruleset still has the load-bearing pieces, so a well-meaning edit that (say) flips
// the policy to `accept`, drops the loopback/skuid accepts, or narrows the family to IPv4-only is
// caught in CI. The live drop/allow enforcement is proven on hardware by TestEgressEnforcement +
// TestContainerHardening (see internal/orchestrator/integration_test.go and HUMAN_TODO.md).
func TestEgressRulesetShape(t *testing.T) {
	must := []struct {
		frag, why string
	}{
		// inet family ⇒ the lock covers both IPv4 and IPv6; an `ip`/`ip6` split could leave a gap.
		{"table inet krayt_egress", "must be in the inet family (IPv4+IPv6)"},
		// Default-deny is the whole property — without it the accepts are meaningless.
		{"policy drop", "the output chain must default-deny"},
		{"oif \"lo\" accept", "loopback must be permitted (the proxy listens there)"},
		// The uid gate is only safe because the container cannot become proxyd (caps/non-root, §6.10).
		{"meta skuid \"proxyd\" accept", "only proxyd may egress"},
		{"ct state established,related accept", "return traffic must be permitted"},
	}
	for _, m := range must {
		if !strings.Contains(egressRuleset, m.frag) {
			t.Errorf("egressRuleset missing %q — %s\ngot:\n%s", m.frag, m.why, egressRuleset)
		}
	}
}

// TestApplyFirewallFullRemovesLock asserts that `full` mode (explicit opt-in to open egress) takes
// the deletion path and never returns an error for a missing table — table removal is best-effort
// so re-application/first-application stays idempotent (§6.6). This runs offline: the delete is
// piped to `nft` but any error (including `nft` absent) is intentionally discarded.
func TestApplyFirewallFullRemovesLock(t *testing.T) {
	if err := ApplyFirewall(context.Background(), ModeFull); err != nil {
		t.Errorf("ApplyFirewall(ModeFull) = %v, want nil (best-effort table delete)", err)
	}
}
