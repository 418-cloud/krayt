package cli

import (
	"log"
	"os"
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

// egressProxyCACertFD is the fixed fd the parent hands this subcommand to report back its
// ephemeral MITM CA's PUBLIC certificate, once, at startup (§2b, §5 of
// add-tls-mitm-credential-injection.md): this process writes CACertPEM() (or nothing, if
// --mitm is off / MITM init failed) and closes it. The parent needs the cert to push into the
// guest's NetworkPolicy; this is the only channel that leaves this process carrying it — never
// stdout/stderr (proxy.log), which is redacted-and-persisted, not structured.
const egressProxyCACertFD = 4

// newEgressProxyCmd is `krayt __egress-proxy`: the host-side L7 egress allowlist proxy,
// spawned as a separate process by the run supervisor (never invoked directly by a user).
// It must not share an address space with the process that holds the user's real credentials,
// writes their repo, and runs their run supervisor.
//
// This is also the stable interface KRAYT_EGRESS_PROXY_BIN swaps out (internal/orchestrator):
// any replacement binary — e.g. a future memory-safe reimplementation, §6.6 — must honor the
// same contract: --mode/--allow/--dns flags in, always; --mitm and --run-id in too, but ONLY
// when MITM is enabled for the run (internal/orchestrator's spawnEgressProxy never sends
// --run-id on a mitm:false invocation, so mitm:false stays byte-identical to the pre-MITM
// contract); a listener on fd 3; a JSON StdinConfig
// (passthrough + resolved inject rules — the ONLY place secret material reaches this process,
// §2b) on stdin, read to EOF; the CA's public cert PEM (or nothing) written once to fd 4, then
// closed; logs (timestamps, hostnames, allow/deny verdicts, dial errors — never bodies, never
// secret values) on stdout/stderr.
//
// Hidden so it never appears in help output or shell completion (cobra excludes Hidden
// commands from both automatically, verified by TestEgressProxyCmdHidden).
func newEgressProxyCmd() *cobra.Command {
	var mode, allowCSV, dns, runID string
	var mitm bool
	cmd := &cobra.Command{
		Use:           "__egress-proxy",
		Short:         "internal: host-side egress allowlist/MITM proxy child process (§6.6)",
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
			stdinCfg, err := proxy.ReadStdinConfig(cmd.InOrStdin())
			if err != nil {
				return err
			}
			policy := proxy.Policy{
				Mode: mode, Allow: allow,
				MITM: mitm, Passthrough: stdinCfg.Passthrough, Inject: stdinCfg.Inject,
			}
			h, ca, err := proxy.BuildHandler(policy, dns, runID)
			if err != nil {
				return err
			}
			// Best-effort: a failure here means the parent won't learn the CA (so it proceeds
			// without pushing one to the guest, §5) but must NOT stop this process from serving
			// egress traffic — the run's only egress path must not die over a plumbing hiccup on
			// a side channel.
			reportCACert(egressProxyCACertFD, ca)
			return proxy.ServeHandler(cmd.Context(), lis, h)
		},
	}
	cmd.Flags().StringVar(&mode, "mode", proxy.ModeAllowlist, "policy mode: allowlist | full | none")
	cmd.Flags().StringVar(&allowCSV, "allow", "", "comma-separated allowlist of egress hosts")
	cmd.Flags().StringVar(&dns, "dns", "", "DNS server to resolve through (host:port); empty uses the host's system resolver")
	cmd.Flags().BoolVar(&mitm, "mitm", false, "terminate TLS and allow header injection (§6.6); default off")
	cmd.Flags().StringVar(&runID, "run-id", "", "run id, folded into the ephemeral MITM CA's CN for operator legibility only")
	return cmd
}

// reportCACert writes ca's public cert PEM (or nothing, if ca is nil — MITM off) to fd, then
// closes it, per the fd-4 contract above. Never fatal: a caller that predates this contract (an
// older KRAYT_EGRESS_PROXY_BIN, or a direct invocation without fd 4 open) simply gets a write
// error here, logged and discarded — the parent times out waiting and proceeds without a CA
// (§5), which must not take down this process's actual egress serving.
func reportCACert(fd uintptr, ca *proxy.CA) {
	f := os.NewFile(fd, "ca-cert")
	if f == nil {
		return
	}
	defer func() { _ = f.Close() }()
	if ca == nil {
		return
	}
	if _, err := f.Write(ca.CACertPEM()); err != nil {
		log.Printf("krayt-egress-proxy: write CA cert to fd %d: %v", fd, err)
	}
}
