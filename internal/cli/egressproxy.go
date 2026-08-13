package cli

import (
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/proxy"
)

// egressProxyListenerFD is the fixed fd the parent (internal/orchestrator) hands this hidden
// subcommand across exec — see internal/orchestrator's spawn helper. The parent owns socket
// creation (correct mode, correct dir, fail-fast on bind); this process needs no filesystem
// access to the socket directory at all, which is what makes it sandboxable later (§4 of
// move-egress-proxy-to-host.md).
const egressProxyListenerFD = 3

// newEgressProxyCmd is `krayt __egress-proxy`: the host-side L7 egress allowlist proxy,
// spawned as a separate process by the run supervisor (never invoked directly by a user).
// It must not share an address space with the process that (from step 2 of this task's arc
// onward) holds the user's real credentials, writes their repo, and runs their run
// supervisor.
//
// This is also the stable interface KRAYT_EGRESS_PROXY_BIN swaps out (internal/orchestrator):
// any replacement binary — e.g. a future memory-safe reimplementation, §6.6 — must honor the
// same contract: --mode/--allow/--dns flags in, a listener on fd 3, logs (timestamps,
// hostnames, allow/deny verdicts, dial errors — never bodies) on stdout/stderr.
//
// Hidden so it never appears in help output or shell completion (cobra excludes Hidden
// commands from both automatically, verified by TestEgressProxyCmdHidden).
func newEgressProxyCmd() *cobra.Command {
	var mode, allowCSV, dns string
	cmd := &cobra.Command{
		Use:           "__egress-proxy",
		Short:         "internal: host-side egress allowlist proxy child process (§6.6)",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			lis, err := proxy.ListenerFromFD(egressProxyListenerFD)
			if err != nil {
				return err
			}
			var allow []string
			if allowCSV != "" {
				allow = strings.Split(allowCSV, ",")
			}
			var factory proxy.Factory
			if dns != "" {
				factory = func(p proxy.Policy) http.Handler { return proxy.HandRolledDNS(p, dns) }
			}
			return proxy.Serve(cmd.Context(), lis, proxy.Policy{Mode: mode, Allow: allow}, factory)
		},
	}
	cmd.Flags().StringVar(&mode, "mode", proxy.ModeAllowlist, "policy mode: allowlist | full | none")
	cmd.Flags().StringVar(&allowCSV, "allow", "", "comma-separated allowlist of egress hosts")
	cmd.Flags().StringVar(&dns, "dns", "", "DNS server to resolve through (host:port); empty uses the host's system resolver")
	return cmd
}
