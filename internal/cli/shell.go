package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/sandbox"
	"github.com/418-cloud/krayt/internal/task"
)

// shellFlags holds the `krayt shell`-only flags — reuses runFlags/applyConfig for everything the
// two commands share (image, repo, config, secrets, network, resources, bundle-depth,
// include-dirty, task) rather than re-deriving the config-precedence path (§8.3).
type shellFlags struct {
	keep   bool
	attach string
	exec   string
}

// newShellCmd builds the `krayt shell` command (§13): a separate, human-driven session, not
// `krayt run --interactive` (decision 1) — it shares run's config-precedence plumbing but a
// deliberately smaller flag set. Absent on purpose: --timeout (decision 5 — no wall-clock budget
// for a session a human is sitting in), --detach, --on-question*, --agent, --transcript
// (decision 2 — krayt shell attaches a bare login shell, never an agent; it never auto-launches
// one, wires krayt-ask, or captures a transcript). What decision 2 does NOT cover: msb requires
// every secrets-file key to carry a network.inject scope, so when agent.adapter names one in
// krayt.yaml, shell still resolves that adapter's secret scope (applyAdapterSecrets) the same way
// `krayt run` does — otherwise a human starting that same agent by hand inside the shell would
// have to hand-write the scope the adapter already knows (2026-09-16 amendment, KRAYT_SPEC.md §13).
func newShellCmd() *cobra.Command {
	var f runFlags
	var sf shellFlags
	cmd := &cobra.Command{
		Use:   "shell",
		Short: "Attach an interactive terminal inside a fresh (or kept) sandbox",
		Long: "Boots a sandbox from the repo snapshot and hands you a shell in /workspace — the " +
			"same isolation `krayt run` gives an agent, driven by a human instead (§15). " +
			"Ephemeral by default; --keep leaves the sandbox running so you can `krayt shell " +
			"--attach <run-id>` back into it, until `krayt stop <run-id>` destroys it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runShell(cmd, &f, &sf) },
	}
	bindShellFlags(cmd, &f, &sf)
	return cmd
}

// bindShellFlags registers krayt shell's flag set — the subset of bindRunFlags' flags that make
// sense for a human-driven session, plus --keep/--attach/--exec, which run has no equivalent of.
func bindShellFlags(cmd *cobra.Command, f *runFlags, sf *shellFlags) {
	fl := cmd.Flags()
	fl.StringVar(&f.config, "config", "", "path to krayt.yaml (default: ./<repo>/krayt.yaml if present)")
	fl.StringVar(&f.image, "image", "", "user OCI image to boot (required unless --attach)")
	fl.StringVar(&f.taskFile, "task", "", "optional path to a task prompt file, or - to read from stdin; copied to /task/prompt.md for an agent started by hand inside the shell to read (decision 13)")
	fl.StringVar(&f.repo, "repo", ".", "host repo to bundle")
	fl.StringVar(&f.secretsFile, "secrets", "", "per-task secrets file (KEY=VALUE); with --attach, only re-used for the changes.patch secret-value scan")
	fl.BoolVar(&f.includeDirty, "include-dirty", false, "include uncommitted working-tree changes in the bundle")
	fl.StringVar(&f.netMode, "net", "allowlist", "egress policy: allowlist | full | none")
	fl.StringArrayVar(&f.allow, "allow", nil, "allowlisted egress domain (repeatable); only with --net allowlist. "+
		"A leading '*.' allows a whole domain suffix — QUOTE IT: unquoted, zsh fails the whole command with "+
		"'no matches found' before krayt ever runs")
	fl.IntVar(&f.bundleDepth, "bundle-depth", 1, "forward bundle: 1 = single-commit snapshot, 0 = full history")
	fl.IntVar(&f.cpus, "cpus", 2, "vCPUs")
	fl.Uint64Var(&f.memory, "memory", 4096, "memory (MiB)")
	fl.Uint64Var(&f.disk, "disk", 20, "disk (GiB)")
	fl.IntVar(&f.maxConc, "max-concurrency", 0, "max concurrent runs sharing this repo's .krayt (0 = unbounded); enforced across processes, same semaphore krayt run uses")
	fl.BoolVar(&f.skipResourceCheck, "skip-resource-check", false, "skip the host free-RAM/disk preflight check before booting the sandbox")

	fl.BoolVar(&sf.keep, "keep", false, "leave the sandbox running when the shell exits, instead of tearing it down (§15) — re-enter with --attach, destroy with krayt stop")
	fl.StringVar(&sf.attach, "attach", "", "re-enter a kept session's sandbox by run id, instead of creating a new one")
	fl.StringVar(&sf.exec, "exec", "", "run this command instead of an interactive shell, then exit (decision 2: a convenience, not a per-agent launcher)")

	_ = cmd.RegisterFlagCompletionFunc("net", cobra.FixedCompletions(
		[]string{string(task.NetworkAllowlist), string(task.NetworkFull), string(task.NetworkNone)},
		cobra.ShellCompDirectiveNoFileComp))
	_ = cmd.RegisterFlagCompletionFunc("image", completeImageRef)
	_ = cmd.RegisterFlagCompletionFunc("allow", completeAllowDomain)
	_ = cmd.RegisterFlagCompletionFunc("attach", completeShellAttachIDs)
}

// completeShellAttachIDs completes --attach with run ids of LIVE `kind: shell` sessions — ones
// `krayt shell --keep` left running (orchestrator.StateKept) — since only those have a sandbox
// left to re-enter. A mid-session record (state `running`) belongs to another shell process
// already attached to it, not a candidate for this flag.
func completeShellAttachIDs(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	repo, _ := cmd.Flags().GetString("repo")
	sd, err := stateDir(repo)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	recs, err := orchestrator.List(sd)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, rec := range recs {
		if rec.EffectiveKind() != orchestrator.KindShell || rec.State != orchestrator.StateKept {
			continue
		}
		out = append(out, cobra.CompletionWithDesc(rec.ID, "kept, "+truncate(rec.ImageRef, 40)))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

func runShell(cmd *cobra.Command, f *runFlags, sf *shellFlags) error {
	repoAbs, err := filepath.Abs(f.repo)
	if err != nil {
		return err
	}
	sd, err := stateDir(f.repo)
	if err != nil {
		return err
	}

	if sf.attach != "" {
		return runShellAttach(cmd, sd, sf.attach, f.secretsFile, sf.exec)
	}
	return runShellFresh(cmd, f, sf, repoAbs, sd)
}

// runShellAttach re-enters a kept session — decision 4. It intentionally skips everything
// runShellFresh does to resolve a policy (config, network, resources, adapter): the sandbox
// already exists exactly as it was left, so there is no policy left to decide.
func runShellAttach(cmd *cobra.Command, stateDirPath, runID, secretsFile, execFlag string) error {
	sb, err := sandbox.NewClient()
	if err != nil {
		return err
	}
	secretsPath := secretsFile
	if secretsPath != "" {
		if secretsPath, err = filepath.Abs(secretsPath); err != nil {
			return err
		}
	}
	// No LogOut: unlike Run, Shell/AttachShell never write to a log sink — the tty attach goes
	// straight to the inherited terminal (sandbox.ExecTTY), not through this package at all.
	deps := orchestrator.Deps{Sandbox: sb}
	runDir := orchestrator.RunDir(stateDirPath, runID)
	res, err := orchestrator.AttachShell(cmd.Context(), deps, runDir, secretsPath, execCommand(execFlag))
	if err != nil {
		return err
	}
	return printShellResult(cmd, runID, res)
}

func runShellFresh(cmd *cobra.Command, f *runFlags, sf *shellFlags, repoAbs, stateDirPath string) error {
	if err := applyConfig(cmd, f); err != nil {
		return err
	}
	if f.image == "" {
		return fmt.Errorf("--image is required (via flag or krayt.yaml)")
	}
	var prompt []byte
	if f.taskFile != "" {
		var err error
		if prompt, err = readTaskPrompt(cmd, f.taskFile); err != nil {
			return err
		}
	}

	id, err := newRunID()
	if err != nil {
		return err
	}
	secretsPath := f.secretsFile
	if secretsPath != "" {
		if secretsPath, err = filepath.Abs(secretsPath); err != nil {
			return err
		}
	}
	netMode, err := task.ParseNetworkMode(f.netMode)
	if err != nil {
		return fmt.Errorf("--net: %w", err)
	}
	if netMode != task.NetworkAllowlist && len(f.allow) > 0 {
		return fmt.Errorf("--allow can only be used with --net allowlist")
	}

	spec := task.RunSpec{
		ID:           id,
		ImageRef:     f.image,
		RepoPath:     repoAbs,
		SecretsPath:  secretsPath,
		IncludeDirty: f.includeDirty,
		Network: task.NetworkPolicy{
			Mode: netMode, Allow: f.allow,
			MITM: f.mitm, Passthrough: f.passthrough, Secrets: f.secrets,
		},
		Env:         f.env,
		BundleDepth: f.bundleDepth,
		TaskPrompt:  prompt,
		// Resources.Timeout is deliberately left zero — decision 5, no wall-clock budget at all
		// in shell mode, regardless of what a krayt.yaml's resources.timeout says. Shell() never
		// reads it, but leaving it unset here too means a `krayt shell` invocation never even
		// LOOKS like it inherited a timeout from the file.
		Resources: task.Resources{CPUs: f.cpus, MemoryMiB: f.memory, DiskGiB: f.disk},
		Container: f.container,
		ExtraConf: f.extraConf,
	}

	secretKeys, err := loadSecretKeySet(spec.SecretsPath)
	if err != nil {
		return err
	}
	// Secret scope only (2026-09-16 amendment, KRAYT_SPEC.md §13): agent.adapter still resolves
	// which secrets-file key is the agent's model credential and which hosts msb may substitute it
	// into — msb requires that scope regardless of whether the agent is launched automatically or
	// by hand — but shell never runs the rest of applyAdapter (no launcher, no krayt-ask wiring, no
	// transcript capture; decision 2 in internal/orchestrator/shell.go still holds for those).
	if err := applyAdapterSecrets(cmd.OutOrStdout(), &spec, f.agent, secretKeys); err != nil {
		return err
	}
	if err := task.ValidateContainerPolicyForMsb(spec.Container); err != nil {
		return err
	}
	if err := task.ValidateNetworkPolicyForMsb(spec.Network, secretKeys, secretsToInjectRules(spec.Network.Secrets)); err != nil {
		return err
	}

	policySource := f.configPath
	if policySource == "" {
		policySource = "flags"
	}
	if err := printNetworkPolicy(cmd.ErrOrStderr(), spec.Network, policySource); err != nil {
		return err
	}
	if spec.ExtraConf != "" {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(),
			"extra msb config (unvalidated, not parsed by krayt — may add mounts or widen secret scoping, §10): %s\n",
			spec.ExtraConf); err != nil {
			return err
		}
	}

	if !f.skipResourceCheck {
		freeMemMiB, freeDiskGiB, err := hostFreeResources()
		if err != nil {
			return fmt.Errorf("resource preflight: %w", err)
		}
		if err := checkHostResources(freeMemMiB, freeDiskGiB, spec.Resources.MemoryMiB, spec.Resources.DiskGiB); err != nil {
			return err
		}
	}

	if sf.keep {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(),
			"--keep: the sandbox will survive this shell exiting — re-enter with `krayt shell --attach %s`, destroy with `krayt stop %s`\n", id, id); err != nil {
			return err
		}
	}
	if sf.exec != "" {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "running %q instead of an interactive shell\n", sf.exec); err != nil {
			return err
		}
	}

	sb, err := sandbox.NewClient()
	if err != nil {
		return err
	}

	release, err := orchestrator.AcquireSlot(cmd.Context(), stateDirPath, f.maxConc)
	if err != nil {
		return err
	}
	defer release()

	// No LogOut: unlike Run, Shell/AttachShell never write to a log sink — the tty attach goes
	// straight to the inherited terminal (sandbox.ExecTTY), not through this package at all.
	deps := orchestrator.Deps{Sandbox: sb}
	res, err := orchestrator.Shell(cmd.Context(), deps, spec, orchestrator.RunDir(stateDirPath, id), sf.keep, execCommand(sf.exec))
	if err != nil {
		return err
	}
	return printShellResult(cmd, id, res)
}

func printShellResult(cmd *cobra.Command, id string, res *orchestrator.ShellResult) error {
	var b strings.Builder
	fmt.Fprintf(&b, "\nshell session %s ended (exit %d)\n", id, res.ExitCode)
	fmt.Fprintf(&b, "  patch:  %s\n", res.PatchPath)
	if res.CommitsBundle != "" {
		fmt.Fprintf(&b, "  commits: %s\n", res.CommitsBundle)
	}
	if len(res.Safety) > 0 {
		fmt.Fprintf(&b, "  ⚠ safety: %d flagged change(s) — review report.md before applying\n", len(res.Safety))
	}
	if res.Kept {
		fmt.Fprintf(&b, "  kept:   sandbox still running — krayt shell --attach %s | krayt stop %s\n", id, id)
	} else {
		fmt.Fprintf(&b, "  apply:  krayt apply %s\n", id)
	}
	_, err := cmd.OutOrStdout().Write([]byte(b.String()))
	return err
}

// execCommand splits --exec's single string into the argv TTYExecSpec.Command wants, via a
// shell so the user can write ordinary shell syntax ("cd sub && go test ./...") rather than a
// krayt-specific argv-splitting convention. Empty means "no override" (msb attaches the default
// shell, decision 2's actual interactive path).
func execCommand(exec string) []string {
	if exec == "" {
		return nil
	}
	return []string{"sh", "-c", exec}
}
