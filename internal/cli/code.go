package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/sandbox"
	"github.com/418-cloud/krayt/internal/task"
)

// codeFlags holds the `krayt code`-only flags — everything else is runFlags/applyConfig, reused
// exactly as `krayt shell` reuses them rather than re-deriving §8.3's config precedence.
type codeFlags struct {
	keep          bool
	noEditorAllow bool
	// stdio is the hidden ProxyCommand entry point (decision 5). It is not a user-facing verb: it
	// exists so `ssh` can run `krayt code --stdio <run-id>` and get a byte pipe to a `sshd -i`
	// inside the sandbox. Everything else on this command is ignored when it is set.
	stdio string
}

// editorAllowHosts is the built-in egress allowlist `krayt code` adds on top of the user's policy
// (decision 14). VS Code Remote-SSH downloads its own ~100MB server into ~/.vscode-server on first
// connect; under krayt's default `--net allowlist` with no `--allow`, that download fails and the
// whole feature is broken out of the box. These are the hosts that download is believed to touch —
// believed, not measured: nobody has run Remote-SSH against a real sandbox yet, so HUMAN_TODO.md
// carries an entry to capture the denied destinations from a real first connect and correct this
// list against them. It is printed on every session and removable with --no-editor-allow, so
// nothing here widens a policy silently.
var editorAllowHosts = []string{
	"update.code.visualstudio.com",
	"vscode.download.prss.microsoft.com",
	"marketplace.visualstudio.com",
	"*.vsassets.io",
	"*.vscode-unpkg.net",
}

// newCodeCmd builds `krayt code` (§13): `krayt shell`'s sibling for an editor rather than a
// terminal. The name is deliberately not `krayt vscode` (decision 1) — the mechanism is SSH, so
// Cursor, JetBrains Gateway, `ssh`, `scp` and `rsync` all work through the same session, and the
// command must not promise VS Code-specific behavior krayt does not have.
//
// Absent on purpose, for exactly `krayt shell`'s reasons: --timeout (no wall-clock budget for a
// session a human is sitting in), --detach, --on-question*, and any agent launcher — the image's
// agent CLI is on PATH and the human starts it in an editor terminal if they want it (decision 7).
// As in `krayt shell`, `agent.adapter`'s secret-scoping/env/config-seed contribution is still
// resolved host-side (applyAdapterForShell), for the reasons §13's 2026-09-16 amendment gives.
func newCodeCmd() *cobra.Command {
	var f runFlags
	var cf codeFlags
	cmd := &cobra.Command{
		Use:   "code",
		Short: "Open a sandbox as an SSH remote-dev session (VS Code Remote-SSH, Cursor, ssh, scp)",
		Long: "Boots a sandbox from the repo snapshot exactly as `krayt shell` does, then prints an " +
			"ssh alias, a one-time ~/.ssh/config Include line, and a vscode-remote:// URI for " +
			"/workspace inside it. The transport is `sshd -i` piped through `msb exec` by an ssh " +
			"ProxyCommand — krayt publishes no port and opens no ingress (§6.6). The session's " +
			"edits come back out as changes.patch like every other krayt session; --keep leaves " +
			"the sandbox running until `krayt stop <run-id>`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runCode(cmd, &f, &cf) },
	}
	bindCodeFlags(cmd, &f, &cf)
	return cmd
}

// bindCodeFlags registers `krayt code`'s flag set: `krayt shell`'s, minus the ones that only mean
// something for a tty session (--attach, --exec), plus --no-editor-allow and the hidden --stdio.
func bindCodeFlags(cmd *cobra.Command, f *runFlags, cf *codeFlags) {
	fl := cmd.Flags()
	fl.StringVar(&f.config, "config", "", "path to krayt.yaml (default: ./<repo>/krayt.yaml if present)")
	fl.StringVar(&f.image, "image", "", "user OCI image to boot (required)")
	fl.StringVar(&f.repo, "repo", ".", "host repo to bundle")
	fl.StringVar(&f.secretsFile, "secrets", "", "per-task secrets file (KEY=VALUE)")
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

	fl.BoolVar(&cf.keep, "keep", false, "leave the sandbox running when the session ends, instead of tearing it down — reconnect with the same ssh alias, destroy with krayt stop")
	fl.BoolVar(&cf.noEditorAllow, "no-editor-allow", false,
		"do not add the built-in editor egress allowlist ("+strings.Join(editorAllowHosts, ", ")+") on top of --net allowlist; "+
			"VS Code Remote-SSH will then be unable to download its own server on first connect")
	fl.StringVar(&cf.stdio, "stdio", "", "internal: pipe stdin/stdout to a sandbox's sshd (used as an ssh ProxyCommand)")
	_ = fl.MarkHidden("stdio")

	_ = cmd.RegisterFlagCompletionFunc("net", cobra.FixedCompletions(
		[]string{string(task.NetworkAllowlist), string(task.NetworkFull), string(task.NetworkNone)},
		cobra.ShellCompDirectiveNoFileComp))
	_ = cmd.RegisterFlagCompletionFunc("image", completeImageRef)
	_ = cmd.RegisterFlagCompletionFunc("allow", completeAllowDomain)
}

func runCode(cmd *cobra.Command, f *runFlags, cf *codeFlags) error {
	// The ProxyCommand path resolves a run id and pipes bytes; it must not load a config, print a
	// policy banner, or touch anything on stdout but the SSH stream itself.
	if cf.stdio != "" {
		return runCodeStdio(cmd, f.repo, cf.stdio)
	}

	repoAbs, err := filepath.Abs(f.repo)
	if err != nil {
		return err
	}
	sd, err := stateDir(f.repo)
	if err != nil {
		return err
	}
	if err := applyConfig(cmd, f); err != nil {
		return err
	}
	if f.image == "" {
		return fmt.Errorf("--image is required (via flag or krayt.yaml)")
	}

	id, err := newRunID()
	if err != nil {
		return err
	}
	spec, editorAdded, err := resolveCodeSpec(cmd, f, cf, repoAbs, id)
	if err != nil {
		return err
	}

	policySource := f.configPath
	if policySource == "" {
		policySource = "flags"
	}
	if err := printNetworkPolicy(cmd.ErrOrStderr(), spec.Network, policySource); err != nil {
		return err
	}
	if err := printEditorAllow(cmd.ErrOrStderr(), spec.Network.Mode, cf.noEditorAllow, editorAdded); err != nil {
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
	if cf.keep {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(),
			"--keep: the sandbox will survive this session ending — the same ssh alias keeps working, destroy with `krayt stop %s`\n", id); err != nil {
			return err
		}
	}

	exe, err := kraytExecutable()
	if err != nil {
		return err
	}
	sb, err := sandbox.NewClient()
	if err != nil {
		return err
	}
	release, err := orchestrator.AcquireSlot(cmd.Context(), sd, f.maxConc)
	if err != nil {
		return err
	}
	defer release()

	// No LogOut: like Shell, a code session has no agent exec to stream. Warn is the writer
	// applyConfigSeeds' best-effort warnings land on — the same stderr the messages above use.
	deps := orchestrator.Deps{Sandbox: sb, Warn: cmd.ErrOrStderr()}
	res, err := orchestrator.Code(cmd.Context(), deps, spec, orchestrator.RunDir(sd, id), orchestrator.CodeOptions{
		Keep: cf.keep, StateDir: sd, KraytExe: exe,
		OnReady: func(s orchestrator.CodeSession) error { return printCodeSession(cmd.OutOrStdout(), s) },
	})
	if err != nil {
		return err
	}
	return printCodeResult(cmd, id, res)
}

// resolveCodeSpec builds and validates a `krayt code` session's RunSpec — the same resolution
// `krayt shell` does (secrets path, network mode, the adapter's env/secrets/config-seed
// contribution, both msb pre-flight validations), plus decision 14's editor allowlist. It returns
// the hosts that allowlist actually added, so the caller can name them in the policy banner rather
// than widening a policy silently. Factored out so a test can inspect the resolved spec without
// booting anything.
func resolveCodeSpec(cmd *cobra.Command, f *runFlags, cf *codeFlags, repoAbs, id string) (task.RunSpec, []string, error) {
	secretsPath := f.secretsFile
	if secretsPath != "" {
		var err error
		if secretsPath, err = filepath.Abs(secretsPath); err != nil {
			return task.RunSpec{}, nil, err
		}
	}
	netMode, err := task.ParseNetworkMode(f.netMode)
	if err != nil {
		return task.RunSpec{}, nil, fmt.Errorf("--net: %w", err)
	}
	if netMode != task.NetworkAllowlist && len(f.allow) > 0 {
		return task.RunSpec{}, nil, fmt.Errorf("--allow can only be used with --net allowlist")
	}

	allow, editorAdded := withEditorAllow(netMode, f.allow, cf.noEditorAllow)

	spec := task.RunSpec{
		ID:           id,
		ImageRef:     f.image,
		RepoPath:     repoAbs,
		SecretsPath:  secretsPath,
		IncludeDirty: f.includeDirty,
		Network: task.NetworkPolicy{
			Mode: netMode, Allow: allow,
			MITM: f.mitm, Passthrough: f.passthrough, Secrets: f.secrets,
		},
		Env:         f.env,
		BundleDepth: f.bundleDepth,
		// Resources.Timeout stays zero, exactly as for `krayt shell`: no wall-clock budget at all
		// for a session a human is sitting in, regardless of krayt.yaml's resources.timeout.
		Resources: task.Resources{CPUs: f.cpus, MemoryMiB: f.memory, DiskGiB: f.disk},
		Container: f.container,
		ExtraConf: f.extraConf,
	}

	secretKeys, err := loadSecretKeySet(spec.SecretsPath)
	if err != nil {
		return task.RunSpec{}, nil, err
	}
	if err := applyAdapterForShell(cmd.OutOrStdout(), &spec, f.agent, secretKeys); err != nil {
		return task.RunSpec{}, nil, err
	}
	if err := task.ValidateContainerPolicyForMsb(spec.Container); err != nil {
		return task.RunSpec{}, nil, err
	}
	if err := task.ValidateNetworkPolicyForMsb(spec.Network, secretKeys, secretsToInjectRules(spec.Network.Secrets)); err != nil {
		return task.RunSpec{}, nil, err
	}
	return spec, editorAdded, nil
}

// withEditorAllow appends the editor allowlist to the user's own allow list and reports what it
// added. It applies to `allowlist` only:
//
//   - under `full` every public host is already reachable, so there is nothing to add — adding it
//     anyway would print a widening that never happened (NetworkArgs ignores Allow in that mode);
//   - under `none` there is no policy to add to at all (`--no-net`, zero rules by construction),
//     so the honest thing is to add nothing and warn that Remote-SSH cannot fetch its server —
//     which printEditorAllow does.
//
// Entries the user already listed are not duplicated, and they are appended AFTER the user's own,
// so the rendered `--net-rule allow@…` order still reads as "what I asked for, then what krayt
// added". Position among the allows carries no semantics: msb is first-match-wins, but all of
// these are public destinations that no other rule matches, and they stay after the `deny@<group>`
// rules `task.NetworkArgs` emits, so the private-range guard is untouched (decision 17).
func withEditorAllow(mode task.NetworkMode, allow []string, disabled bool) (out, added []string) {
	out = append([]string(nil), allow...)
	if disabled || mode != task.NetworkAllowlist {
		return out, nil
	}
	have := make(map[string]bool, len(allow))
	for _, h := range allow {
		have[strings.ToLower(strings.TrimSpace(h))] = true
	}
	for _, h := range editorAllowHosts {
		if have[strings.ToLower(h)] {
			continue
		}
		out = append(out, h)
		added = append(added, h)
	}
	return out, added
}

// printEditorAllow is the disclosure half of decision 14: the operator must be able to read, on
// every session, exactly which hosts krayt added to their policy and how to remove them — and
// under `--net none`, why the editor will not be able to install itself.
func printEditorAllow(w io.Writer, mode task.NetworkMode, disabled bool, added []string) error {
	switch {
	case mode == task.NetworkNone:
		_, err := fmt.Fprintln(w, "  editor allowlist: not added (--net none has no egress policy to add to) — "+
			"VS Code Remote-SSH cannot download its server into this sandbox; use --net allowlist for the first connect")
		return err
	case disabled:
		_, err := fmt.Fprintln(w, "  editor allowlist: disabled (--no-editor-allow) — "+
			"VS Code Remote-SSH will be unable to download its server unless you allowed those hosts yourself")
		return err
	case len(added) > 0:
		_, err := fmt.Fprintf(w, "  editor allowlist (added by krayt code, remove with --no-editor-allow): %s\n",
			strings.Join(added, ","))
		return err
	}
	return nil
}

// printCodeSession is the connection block, printed once the sandbox is up and before the session
// blocks. krayt prints; it never launches an editor (decision 13), so there is no dependency on a
// `code` binary being on PATH.
func printCodeSession(w io.Writer, s orchestrator.CodeSession) error {
	var b strings.Builder
	fmt.Fprintf(&b, "\ncode session %s ready — /workspace as %s@%s\n", s.RunID, s.User, s.Alias)
	fmt.Fprintf(&b, "  ssh:     %s\n", s.SSHCommand)
	if s.IncludeLine != "" {
		fmt.Fprintf(&b, "  one-time setup — add this line to ~/.ssh/config (krayt never edits it):\n")
		fmt.Fprintf(&b, "      %s\n", s.IncludeLine)
		fmt.Fprintf(&b, "  then:    ssh %s   |   VS Code: Remote-SSH → %s\n", s.Alias, s.Alias)
	}
	fmt.Fprintf(&b, "  vscode:  %s\n", s.VSCodeURI)
	fmt.Fprintf(&b, "\nsession is live — press Ctrl-C to end it and collect changes.patch\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func printCodeResult(cmd *cobra.Command, id string, res *orchestrator.CodeResult) error {
	var b strings.Builder
	fmt.Fprintf(&b, "\ncode session %s ended\n", id)
	fmt.Fprintf(&b, "  patch:  %s\n", res.PatchPath)
	if res.CommitsBundle != "" {
		fmt.Fprintf(&b, "  commits: %s\n", res.CommitsBundle)
	}
	if len(res.Safety) > 0 {
		fmt.Fprintf(&b, "  ⚠ safety: %d flagged change(s) — review report.md before applying\n", len(res.Safety))
	}
	if res.Kept {
		fmt.Fprintf(&b, "  kept:   sandbox still running — ssh %s | krayt stop %s\n", res.Session.Alias, id)
	} else {
		fmt.Fprintf(&b, "  apply:  krayt apply %s\n", id)
	}
	_, err := cmd.OutOrStdout().Write([]byte(b.String()))
	return err
}

// kraytExecutable resolves this binary's own absolute path for the generated ProxyCommand. ssh —
// and VS Code's own ssh, which runs with an environment krayt never sees — must not have to find
// `krayt` on PATH, so the path is baked into the config instead.
func kraytExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve the krayt binary's own path (needed for the ssh ProxyCommand): %w", err)
	}
	// Resolve symlinks so a `krayt` on PATH that points into a version-managed directory still
	// names a file that exists after the symlink is re-pointed.
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	return exe, nil
}
