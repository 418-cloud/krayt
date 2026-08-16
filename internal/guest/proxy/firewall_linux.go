//go:build linux

package proxy

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"regexp"
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
	// Read back what nft ACTUALLY installed rather than trusting that a clean exit means the
	// intended lock is in place (§14 Phase 8's hardware "Done when"). TestEgressRulesetShape
	// only proves the constant above is well-formed; this proves the live kernel ruleset is.
	if err := verifyInstalledRuleset(ctx); err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	return nil
}

// rulesetBegin/rulesetEnd delimit the installed-ruleset dump on the guest's serial console. The
// console log is otherwise full of kernel/systemd boot text, so the host-side hardware check
// (TestEgressEnforcement) needs unambiguous markers to cut the dump out of it — the dump itself,
// on a live guest, is the §14 Phase 8 evidence that no `skuid` rule survived the move of the L7
// proxy to the host. Changing these strings breaks that test; they are a contract, not a label.
const (
	rulesetBegin = "krayt: BEGIN egress ruleset"
	rulesetEnd   = "krayt: END egress ruleset"
)

// loopbackAcceptRE matches the loopback accept as `nft list` prints it back. nft resolves the
// interface name to an index at load time and reverse-resolves it for display, so the normal
// output is `oif "lo" accept` — but a guest where that reverse lookup fails would print the raw
// index (`oif 1 accept`) for the same, still-correct rule. Both forms are accepted so the check
// fails on a genuinely missing loopback accept, never on a display quirk.
var loopbackAcceptRE = regexp.MustCompile(`oif (?:"lo"|1) accept`)

// verifyInstalledRuleset dumps the guest's live nftables ruleset to the serial console and then
// asserts the §6.6 lock is really in it. The dump is printed BEFORE the check so a failure
// arrives with the evidence that explains it instead of just an error string.
func verifyInstalledRuleset(ctx context.Context) error {
	dump, err := nftOutput(ctx, "list", "ruleset")
	if err != nil {
		return fmt.Errorf("read back egress ruleset: %w", err)
	}
	log.Printf("%s\n%s\n%s", rulesetBegin, strings.TrimRight(dump, "\n"), rulesetEnd)
	return checkRuleset(dump)
}

// checkRuleset asserts the invariants of the installed ruleset. Split out from the `nft` call so
// the logic is exercised offline against fixture dumps (firewall_internal_test.go) rather than
// only on real hardware.
//
// The `skuid` check deliberately spans the WHOLE ruleset, not just krayt's own table: §14 Phase
// 8's wording is "`nft list ruleset` in the guest contains no `skuid` rule", and the property
// worth having is that NOTHING in the guest gates egress on a process identity anymore. A uid
// accept anywhere is either the pre-Phase-8 lock resurrected or a second lock nobody reviewed;
// both mean the container-hardening controls are silently load-bearing for egress again, which
// is exactly the finding-#1 coupling this phase removed.
func checkRuleset(dump string) error {
	if strings.Contains(dump, "skuid") {
		return fmt.Errorf("installed ruleset gates egress on a uid (`skuid`) — the L7 proxy is host-side since §14 Phase 8, so no uid may be load-bearing:\n%s", dump)
	}
	block, ok := tableBlock(dump, "inet krayt_egress")
	if !ok {
		return fmt.Errorf("installed ruleset has no `table inet krayt_egress` — the default-deny egress lock is not in place:\n%s", dump)
	}
	if !strings.Contains(block, "policy drop") {
		return fmt.Errorf("krayt_egress output chain does not default-deny (no `policy drop`):\n%s", block)
	}
	if !loopbackAcceptRE.MatchString(block) {
		return fmt.Errorf("krayt_egress has no loopback accept — the vsock forwarder would be unreachable:\n%s", block)
	}
	return nil
}

// tableBlock returns the brace-delimited body of `table <name> { … }` from an `nft list ruleset`
// dump. Scoping the chain assertions to krayt's own table keeps an unrelated table's `policy
// drop` from standing in for the one that matters.
func tableBlock(dump, name string) (string, bool) {
	head := "table " + name + " {"
	i := strings.Index(dump, head)
	if i < 0 {
		return "", false
	}
	depth := 0
	for j := i + len(head) - 1; j < len(dump); j++ {
		switch dump[j] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return dump[i : j+1], true
			}
		}
	}
	return "", false // unterminated table block: treat as absent rather than guessing
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

// nftOutput runs an `nft` subcommand and returns its stdout. Unlike nft above it keeps stderr
// out of the returned value, so a warning on stderr can never be mistaken for ruleset content
// by checkRuleset.
func nftOutput(ctx context.Context, args ...string) (string, error) {
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, "nft", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("nft %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
	}
	return string(out), nil
}
