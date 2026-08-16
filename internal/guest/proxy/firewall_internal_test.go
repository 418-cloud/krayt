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

// installedDump is `nft list ruleset` output for a correctly locked guest, in the form nft
// PRINTS rather than the form egressRuleset is written in — note `priority filter;`, which nft
// normalizes `priority 0;` to on output. Using the printed form is the point: checkRuleset runs
// against what comes back out of the kernel, so a fixture in the input form would prove nothing
// about the parsing the real check has to survive.
const installedDump = `table inet krayt_egress {
	chain output {
		type filter hook output priority filter; policy drop;
		oif "lo" accept
	}
}
`

// TestCheckRulesetAcceptsInstalledShape is the baseline: the ruleset krayt actually installs,
// as the kernel reports it, passes.
func TestCheckRulesetAcceptsInstalledShape(t *testing.T) {
	if err := checkRuleset(installedDump); err != nil {
		t.Errorf("checkRuleset(installedDump) = %v, want nil", err)
	}
	// The raw index form nft falls back to when it cannot reverse-resolve `lo` is the same rule
	// and must pass too — otherwise a display quirk would fail every run closed.
	if err := checkRuleset(strings.Replace(installedDump, `oif "lo" accept`, "oif 1 accept", 1)); err != nil {
		t.Errorf("checkRuleset(raw-index loopback form) = %v, want nil", err)
	}
}

// TestCheckRulesetAcceptsGeneratedRuleset ties the checker to the constant it guards: whatever
// egressRuleset is edited to, the installed-ruleset check must still accept it. Without this a
// future edit could satisfy TestEgressRulesetShape and then fail closed on every real boot.
func TestCheckRulesetAcceptsGeneratedRuleset(t *testing.T) {
	if err := checkRuleset(egressRuleset); err != nil {
		t.Errorf("checkRuleset(egressRuleset) = %v, want nil — the generated ruleset must satisfy its own read-back check", err)
	}
}

// TestCheckRulesetRejects covers each way the live ruleset can be wrong. The skuid case is the
// §14 Phase 8 regression proper — it is the pre-Phase-8 lock, which passed every check krayt had
// before this one because the old checks only ever read the constant, never the kernel.
func TestCheckRulesetRejects(t *testing.T) {
	cases := []struct {
		name, dump, want string
	}{{
		name: "skuid rule in krayt's own table",
		dump: `table inet krayt_egress {
	chain output {
		type filter hook output priority filter; policy drop;
		oif "lo" accept
		skuid "proxyd" accept
	}
}
`,
		want: "skuid",
	}, {
		// Scoping the skuid check to krayt_egress would miss this: a second table is just as
		// able to gate egress on a uid, and just as much a return of the coupling Phase 8 cut.
		name: "skuid rule in an unrelated table",
		dump: installedDump + `table inet other {
	chain output {
		type filter hook output priority filter; policy accept;
		skuid "proxyd" accept
	}
}
`,
		want: "skuid",
	}, {
		name: "lock absent entirely",
		dump: "table inet other {\n\tchain output {\n\t\ttype filter hook output priority filter; policy drop;\n\t}\n}\n",
		want: "no `table inet krayt_egress`",
	}, {
		name: "chain does not default-deny",
		dump: strings.Replace(installedDump, "policy drop;", "policy accept;", 1),
		want: "does not default-deny",
	}, {
		// A neighbouring table that DOES default-deny must not stand in for krayt's own, which
		// is why the chain assertions are scoped to the krayt_egress block.
		name: "another table default-denies but krayt's does not",
		dump: strings.Replace(installedDump, "policy drop;", "policy accept;", 1) +
			"table inet other {\n\tchain output {\n\t\ttype filter hook output priority filter; policy drop;\n\t}\n}\n",
		want: "does not default-deny",
	}, {
		name: "loopback accept missing",
		dump: strings.Replace(installedDump, "\t\toif \"lo\" accept\n", "", 1),
		want: "no loopback accept",
	}, {
		name: "empty ruleset",
		dump: "",
		want: "no `table inet krayt_egress`",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRuleset(tc.dump)
			if err == nil {
				t.Fatalf("checkRuleset() = nil, want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("checkRuleset() = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// TestTableBlockUnterminated guards the brace scanner's fail-closed edge: a truncated dump (a
// console read cut short, say) must read as "the lock is not there", never as a block whose
// contents happen to look acceptable.
func TestTableBlockUnterminated(t *testing.T) {
	if _, ok := tableBlock("table inet krayt_egress {\n\tchain output {\n", "inet krayt_egress"); ok {
		t.Error("tableBlock() accepted an unterminated table block, want !ok")
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
