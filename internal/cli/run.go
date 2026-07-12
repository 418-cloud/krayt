package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/adapter"
	"github.com/418-cloud/krayt/internal/guest"
	"github.com/418-cloud/krayt/internal/imagestore"
	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/provider"
	"github.com/418-cloud/krayt/internal/secrets"
	"github.com/418-cloud/krayt/internal/task"
)

// Env vars coordinating the detached-supervisor handoff (§6.2): the parent sets both when it
// forks; the child reads them so it supervises the same run instead of re-detaching.
const (
	envDetachChild = "KRAYT_DETACH_CHILD" // present on the child → run in the foreground, don't re-fork
	envRunID       = "KRAYT_RUN_ID"       // the run id the parent already generated and printed
	envTaskFile    = "KRAYT_TASK_FILE"    // set when the parent spooled a stdin-read prompt for the detached child
)

// stdinTaskArg is the --task value that means "read the prompt from stdin" instead of a file path.
const stdinTaskArg = "-"

// runDeps are the OS-specific collaborators for a run, assembled by the build-tagged
// newRunDeps (vfkit + the pulled base image on macOS; an error elsewhere until the
// firecracker backend, Phase 6).
type runDeps struct {
	provider provider.Provider
	baseVM   provider.VMSpec
}

// runFlags holds the Phase 2 `krayt run` flag set (a subset of §13; secrets, network, and
// questions arrive in later phases).
type runFlags struct {
	config       string
	image        string
	taskFile     string
	repo         string
	secretsFile  string
	includeDirty bool
	netMode      string
	allow        []string
	env          map[string]string
	bundleDepth  int
	cpus         int
	memory       uint64
	disk         uint64
	timeout      time.Duration
	detach       bool
	maxConc      int

	onQuestion        string        // fail | wait (§6.13)
	questionTimeout   time.Duration // per-question wait limit
	onQuestionTimeout string        // sentinel | abort

	agent string // none | claude-code | gemini-cli (§6.14, §8.1)

	// container is resolved from krayt.yaml's `container:` block (§8.1); there are no CLI flags
	// for it in v1 (config-file only), so it stays the secure zero value when no config is present.
	container task.ContainerPolicy
}

func newRunCmd() *cobra.Command {
	var f runFlags
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run an agent against a repo snapshot in a fresh micro-VM",
		Long: "Bundles the repo, boots a micro-VM, runs the user image against the task, and " +
			"collects a reviewable changes.patch into .krayt/runs/<id>/ (§7).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runRun(cmd, &f) },
	}
	bindRunFlags(cmd, &f)
	return cmd
}

// bindRunFlags registers the `krayt run` flags onto cmd (extracted so config-precedence is
// testable against a real flag set).
func bindRunFlags(cmd *cobra.Command, f *runFlags) {
	fl := cmd.Flags()
	fl.StringVar(&f.config, "config", "", "path to krayt.yaml (default: ./<repo>/krayt.yaml if present)")
	fl.StringVar(&f.image, "image", "", "user OCI image to run (required)")
	fl.StringVar(&f.taskFile, "task", "", "path to the task prompt file, or - to read from stdin (required)")
	fl.StringVar(&f.repo, "repo", ".", "host repo to bundle")
	fl.StringVar(&f.secretsFile, "secrets", "", "per-task secrets file (KEY=VALUE), mounted on tmpfs at /run/secrets")
	fl.BoolVar(&f.includeDirty, "include-dirty", false, "include uncommitted working-tree changes in the bundle")
	fl.StringVar(&f.netMode, "net", "allowlist", "egress policy: allowlist | full | none")
	fl.StringArrayVar(&f.allow, "allow", nil, "allowlisted egress domain (repeatable); only with --net allowlist")
	fl.IntVar(&f.bundleDepth, "bundle-depth", 1, "forward bundle: 1 = single-commit snapshot, 0 = full history")
	fl.IntVar(&f.cpus, "cpus", 2, "vCPUs")
	fl.Uint64Var(&f.memory, "memory", 4096, "memory (MiB)")
	fl.Uint64Var(&f.disk, "disk", 20, "disk (GiB)")
	fl.DurationVar(&f.timeout, "timeout", 30*time.Minute, "wall-clock run timeout")
	fl.BoolVar(&f.detach, "detach", false, "run in the background: a detached supervisor owns the VM to completion, so this command returns immediately and the run survives the terminal closing (§6.2). Track it with krayt ls/attach/answer")
	fl.IntVar(&f.maxConc, "max-concurrency", 0, "max concurrent runs sharing this repo's .krayt (0 = unbounded); enforced across processes")
	fl.StringVar(&f.onQuestion, "on-question", "fail", "agent question mode: fail (autonomous) | wait (pause for input)")
	fl.DurationVar(&f.questionTimeout, "question-timeout", 10*time.Minute, "per-question wait timeout")
	fl.StringVar(&f.onQuestionTimeout, "on-question-timeout", "sentinel", "on question timeout: sentinel | abort")
	fl.StringVar(&f.agent, "agent", "none", "agent adapter: none | claude-code | gemini-cli")

	// Dynamic shell completion for run's flag values (§13). Enum flags complete their fixed value
	// sets, sourced from the same constants Parse*/Get validate against so completion can't drift;
	// --image/--allow complete from this repo's run history. RegisterFlagCompletionFunc only errors
	// on an unknown flag name or a double-registration — both programmer errors caught immediately
	// by any test/run — so the errors are safely discarded, matching this codebase's other
	// best-effort `_ = ...` writes.
	_ = cmd.RegisterFlagCompletionFunc("net", cobra.FixedCompletions(
		[]string{string(task.NetworkAllowlist), string(task.NetworkFull), string(task.NetworkNone)},
		cobra.ShellCompDirectiveNoFileComp))
	_ = cmd.RegisterFlagCompletionFunc("on-question", cobra.FixedCompletions(
		[]string{string(task.QuestionFail), string(task.QuestionWait)},
		cobra.ShellCompDirectiveNoFileComp))
	_ = cmd.RegisterFlagCompletionFunc("on-question-timeout", cobra.FixedCompletions(
		[]string{string(task.OnTimeoutSentinel), string(task.OnTimeoutAbort)},
		cobra.ShellCompDirectiveNoFileComp))
	_ = cmd.RegisterFlagCompletionFunc("agent", cobra.FixedCompletions(
		adapter.Names(), cobra.ShellCompDirectiveNoFileComp))
	_ = cmd.RegisterFlagCompletionFunc("image", completeImageRef)
	_ = cmd.RegisterFlagCompletionFunc("allow", completeAllowDomain)
}

// wellKnownAllowDomains seeds --allow completion with domains already documented in this repo
// as common egress needs (KRAYT_SPEC.md §6.6 line 409, hack/krayt-dev/README.md lines 70/116).
// Not authoritative or exhaustive — a completion convenience layered under the repo's own run
// history, which always takes priority.
var wellKnownAllowDomains = []string{
	"api.anthropic.com",
	"generativelanguage.googleapis.com",
	"proxy.golang.org",
	"sum.golang.org",
	"cache.nixos.org",
	"github.com",
	"codeload.github.com",
}

// completeImageRef completes --image with the distinct ImageRef values from this repo's run
// history, most-recently-used first (ImageRef is the raw --image string a prior run used).
func completeImageRef(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	repo, _ := cmd.Flags().GetString("repo")
	sd, err := stateDir(repo)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	recs, err := orchestrator.List(sd)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	seen := map[string]bool{}
	var out []string
	for _, rec := range recs { // already newest-first
		if rec.ImageRef == "" || seen[rec.ImageRef] {
			continue
		}
		seen[rec.ImageRef] = true
		out = append(out, rec.ImageRef)
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// completeAllowDomain completes --allow with the union of this repo's run-history allow domains
// (newest-first) and the well-known seed list, deduplicated. History takes priority.
func completeAllowDomain(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	seen := map[string]bool{}
	var out []string
	add := func(d string) {
		if d == "" || seen[d] {
			return
		}
		seen[d] = true
		out = append(out, d)
	}
	if repo, _ := cmd.Flags().GetString("repo"); repo != "" {
		if sd, err := stateDir(repo); err == nil {
			if recs, err := orchestrator.List(sd); err == nil {
				for _, rec := range recs {
					for _, d := range rec.Network.Allow {
						add(d)
					}
				}
			}
		}
	}
	for _, d := range wellKnownAllowDomains {
		add(d)
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

func runRun(cmd *cobra.Command, f *runFlags) error {
	// Overlay krayt.yaml under the flags (defaults → file → flags; §8.3) before validation,
	// so the file can supply required fields like image/task.
	if err := applyConfig(cmd, f); err != nil {
		return err
	}
	if f.image == "" || f.taskFile == "" {
		return fmt.Errorf("--image and --task are required (via flags or krayt.yaml)")
	}
	prompt, err := readTaskPrompt(cmd, f.taskFile)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(prompt)) == 0 {
		return fmt.Errorf("task prompt is empty")
	}
	repoAbs, err := filepath.Abs(f.repo)
	if err != nil {
		return err
	}

	// A detached supervisor child inherits the run id its parent already printed (envRunID),
	// so both name the same run dir; a fresh invocation generates one.
	id := os.Getenv(envRunID)
	if id == "" {
		if id, err = newRunID(); err != nil {
			return err
		}
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
	// --allow only means anything under allowlist mode; `full` allows all and `none` denies
	// all regardless, so reject the combination rather than silently ignoring it (§6.6).
	if netMode != task.NetworkAllowlist && len(f.allow) > 0 {
		return fmt.Errorf("--allow can only be used with --net allowlist")
	}
	qMode, err := task.ParseQuestionMode(f.onQuestion)
	if err != nil {
		return fmt.Errorf("--on-question: %w", err)
	}
	qOnTimeout, err := task.ParseQuestionTimeoutAction(f.onQuestionTimeout)
	if err != nil {
		return fmt.Errorf("--on-question-timeout: %w", err)
	}
	spec := task.RunSpec{
		ID:           id,
		ImageRef:     f.image,
		RepoPath:     repoAbs,
		SecretsPath:  secretsPath,
		IncludeDirty: f.includeDirty,
		Network:      task.NetworkPolicy{Mode: netMode, Allow: f.allow},
		Env:          f.env,
		BundleDepth:  f.bundleDepth,
		TaskPrompt:   prompt,
		Detach:       f.detach,
		Resources: task.Resources{
			CPUs:      f.cpus,
			MemoryMiB: f.memory,
			DiskGiB:   f.disk,
			Timeout:   f.timeout,
		},
		Questions: task.QuestionsPolicy{Mode: qMode, Timeout: f.questionTimeout, OnTimeout: qOnTimeout},
		Container: f.container,
	}

	// Optional per-agent adapter (§6.14): validate auth (exactly-one, fail fast before any VM
	// boots or image pull) and merge its env additions — e.g. wiring krayt-ask when questions
	// are enabled — under the user's env, which wins.
	if err := applyAdapter(&spec, f.agent); err != nil {
		return err
	}

	// --detach: hand the run to a session-detached supervisor child and return, so the run
	// survives this terminal closing and its `waiting` question can be answered later (§6.2).
	// The child re-enters here with envDetachChild set and runs the same spec in the foreground.
	if f.detach && os.Getenv(envDetachChild) == "" {
		// A stdin-sourced prompt can't be re-read by the detached child (its stdin is gone),
		// so spool the bytes we already read to a file the child reads instead (§6.2).
		var spooledTaskFile string
		if f.taskFile == stdinTaskArg {
			if spooledTaskFile, err = spoolTaskPrompt(filepath.Join(repoAbs, ".krayt"), spec.ID, prompt); err != nil {
				return err
			}
		}
		return spawnDetachedRun(cmd, filepath.Join(repoAbs, ".krayt"), spec.ID, spooledTaskFile)
	}

	// OS-specific provider + base VM image (vfkit on macOS; error elsewhere until Phase 7).
	deps, err := newRunDeps()
	if err != nil {
		return err
	}

	// Acquire the user image on the host and pre-load it over vsock (§6.11).
	img, err := acquireUserImage(cmd, spec.ImageRef)
	if err != nil {
		return err
	}

	var logOut = cmd.OutOrStdout()
	if f.detach {
		logOut = nil
	}
	// Drive the run through the Manager so it writes state under <repo>/.krayt (§6.2) and the
	// management commands can observe it. v1 supervises in the foreground (this process).
	mgr := orchestrator.NewManager(orchestrator.Deps{
		Provider: deps.provider,
		BaseVM:   deps.baseVM,
		Image:    img,
		LogOut:   logOut,
	}, filepath.Join(repoAbs, ".krayt"), f.maxConc)
	res, err := mgr.Run(cmd.Context(), spec)
	if err != nil {
		return err
	}

	summary := fmt.Sprintf("\nrun %s complete (exit %d)\n  patch:  %s\n", id, res.ExitCode, res.PatchPath)
	if res.CommitsBundle != "" {
		summary += fmt.Sprintf("  commits: %s\n", res.CommitsBundle)
	}
	summary += fmt.Sprintf("  report: %s\n", filepath.Join(res.RunDir, "report.md"))
	summary += fmt.Sprintf("  apply:  krayt apply %s\n", id)
	// Flag patch changes that can execute outside the workspace edit (§14 Phase 5); details
	// are in report.md's Safety section.
	if len(res.Safety) > 0 {
		summary += fmt.Sprintf("  ⚠ safety: %d flagged change(s) — review report.md before applying\n", len(res.Safety))
	}
	_, err = fmt.Fprint(cmd.OutOrStdout(), summary)
	return err
}

// applyAdapter runs the optional per-agent adapter's host-side pre-flight (§6.14): it loads the
// per-task secrets file and passes only the credential key names — never the values — to the
// adapter, which enforces the agent's exactly-one auth rule; then it merges the adapter's
// non-secret env additions (e.g. the krayt-ask socket) under spec.Env so a user-set value always
// wins. Called before the VM boots so a bad credential set fails fast.
func applyAdapter(spec *task.RunSpec, name string) error {
	ad, err := adapter.Get(name)
	if err != nil {
		return err
	}
	var secretKeys []string
	if spec.SecretsPath != "" {
		vals, err := secrets.Load(spec.SecretsPath)
		if err != nil {
			return fmt.Errorf("read secrets: %w", err)
		}
		for k := range vals {
			secretKeys = append(secretKeys, k)
		}
	}
	plan, err := ad.Prepare(adapter.Input{
		SecretKeys:    secretKeys,
		QuestionsWait: spec.Questions.Mode == task.QuestionWait,
		AskSocket:     guest.ContainerAskSocket,
	})
	if err != nil {
		return err
	}
	if len(plan.Env) > 0 && spec.Env == nil {
		spec.Env = map[string]string{}
	}
	for k, v := range plan.Env {
		if _, set := spec.Env[k]; !set {
			spec.Env[k] = v
		}
	}
	return nil
}

// acquireUserImage pulls the user image into the host cache and returns it (§6.11).
func acquireUserImage(cmd *cobra.Command, ref string) (*imagestore.Image, error) {
	base, err := krayCacheBase()
	if err != nil {
		return nil, err
	}
	cacheRoot := filepath.Join(base, "krayt", "imagestore")
	src, err := imagestore.Remote(ref)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "acquiring image %s …\n", ref); err != nil {
		return nil, err
	}
	return imagestore.Acquire(cmd.Context(), src, ref, cacheRoot)
}

// spoolTaskPrompt writes a stdin-read task prompt to a file in the run dir so a detached
// supervisor child — whose stdin is gone after re-exec — can still read it (§6.2). Doubles as a
// record of what was run.
func spoolTaskPrompt(stateDir, id string, prompt []byte) (string, error) {
	runDir := orchestrator.RunDir(stateDir, id)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return "", fmt.Errorf("create run dir: %w", err)
	}
	path := filepath.Join(runDir, "prompt.md")
	if err := os.WriteFile(path, prompt, 0o644); err != nil {
		return "", fmt.Errorf("spool task prompt: %w", err)
	}
	return path, nil
}

// readTaskPrompt resolves the task prompt from, in order: a spooled file left by the parent for
// a detached child (envTaskFile — only honored when envDetachChild marks us as that child, so a
// stray KRAYT_TASK_FILE in a user's shell can't hijack --task), stdin (taskFile == "-"), or the
// given file path — mirroring exactly how the prompt is sourced today except for the new stdin
// case (§13).
func readTaskPrompt(cmd *cobra.Command, taskFile string) ([]byte, error) {
	if spooled := os.Getenv(envTaskFile); spooled != "" && os.Getenv(envDetachChild) != "" {
		b, err := os.ReadFile(spooled)
		if err != nil {
			return nil, fmt.Errorf("read spooled task file: %w", err)
		}
		return b, nil
	}
	if taskFile == stdinTaskArg {
		b, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, fmt.Errorf("read task prompt from stdin: %w", err)
		}
		return b, nil
	}
	b, err := os.ReadFile(taskFile)
	if err != nil {
		return nil, fmt.Errorf("read task file: %w", err)
	}
	return b, nil
}

// spawnDetachedRun launches a session-detached copy of this krayt invocation to supervise the
// run in the background, then returns after printing how to track it (§6.2). The child re-execs
// the same argv with envDetachChild + envRunID set, so it names the same run dir and runs the
// identical spec in the foreground; its own stdout/stderr go to the run's supervisor log.
// spooledTaskFile, if non-empty, is a stdin-read prompt already spooled to disk by the parent;
// the child can't re-read stdin after re-exec, so it's handed the file via envTaskFile instead.
func spawnDetachedRun(cmd *cobra.Command, stateDir, id, spooledTaskFile string) error {
	runDir := orchestrator.RunDir(stateDir, id)
	if err := os.MkdirAll(filepath.Join(runDir, "logs"), 0o755); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	env := append(os.Environ(), envDetachChild+"=1", envRunID+"="+id)
	if spooledTaskFile != "" {
		env = append(env, envTaskFile+"="+spooledTaskFile)
	}
	logPath := filepath.Join(runDir, "logs", "supervisor.log")
	pid, err := spawnDetached(exe, os.Args[1:], env, logPath)
	if err != nil {
		return fmt.Errorf("start detached supervisor: %w", err)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(),
		"run %s started in background (supervisor pid %d)\n"+
			"  track:  krayt ls\n"+
			"  attach: krayt attach %s\n"+
			"  answer: krayt answer %s <response>\n"+
			"  stop:   krayt stop %s\n",
		id, pid, id, id, id)
	return err
}

// spawnDetached starts exe (args, env) as a new-session background process whose stdio is
// redirected to logPath (stdin from /dev/null), returning its pid. Setsid puts it in its own
// session so it detaches from the controlling terminal and outlives the launching shell (§6.2).
func spawnDetached(exe string, args, env []string, logPath string) (int, error) {
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer func() { _ = logf.Close() }()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return 0, err
	}
	defer func() { _ = devnull.Close() }()

	c := exec.Command(exe, args...)
	c.Env = env
	c.Stdin = devnull
	c.Stdout = logf
	c.Stderr = logf
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return 0, err
	}
	pid := c.Process.Pid
	_ = c.Process.Release() // detach: don't wait, let the parent return while the child runs
	return pid, nil
}

// applyConfig loads krayt.yaml (explicit --config, else <repo>/krayt.yaml if present) and
// overlays it under the flags: a config value is used only when its flag was not set on the
// command line, so flags always win (§8.3).
func applyConfig(cmd *cobra.Command, f *runFlags) error {
	path := f.config
	if path == "" {
		def := filepath.Join(f.repo, "krayt.yaml")
		if _, err := os.Stat(def); err != nil {
			if os.IsNotExist(err) {
				return nil // no config file and none requested
			}
			return fmt.Errorf("config %s: %w", def, err) // surface a real IO/permission error
		}
		path = def
	}
	cfg, err := task.LoadConfig(path)
	if err != nil {
		return err
	}
	changed := func(name string) bool { return cmd.Flags().Changed(name) }
	str := func(name, v string, dst *string) {
		if !changed(name) && v != "" {
			*dst = v
		}
	}
	str("image", cfg.Image, &f.image)
	str("task", cfg.Task, &f.taskFile)
	str("repo", cfg.Repo, &f.repo)
	str("secrets", cfg.Secrets, &f.secretsFile)
	str("net", cfg.Network.Mode, &f.netMode)
	if !changed("allow") && len(cfg.Network.Allow) > 0 {
		f.allow = cfg.Network.Allow
	}
	if !changed("include-dirty") && cfg.IncludeDirty != nil {
		f.includeDirty = *cfg.IncludeDirty
	}
	if !changed("bundle-depth") && cfg.BundleDepth != nil {
		f.bundleDepth = *cfg.BundleDepth
	}
	if !changed("cpus") && cfg.Resources.CPUs != nil {
		f.cpus = *cfg.Resources.CPUs
	}
	if !changed("memory") && cfg.Resources.Memory != "" {
		m, err := task.ParseMiB(cfg.Resources.Memory)
		if err != nil {
			return err
		}
		f.memory = m
	}
	if !changed("disk") && cfg.Resources.Disk != "" {
		d, err := task.ParseGiB(cfg.Resources.Disk)
		if err != nil {
			return err
		}
		f.disk = d
	}
	if !changed("timeout") && cfg.Resources.Timeout != "" {
		d, err := time.ParseDuration(cfg.Resources.Timeout)
		if err != nil {
			return fmt.Errorf("config timeout %q: %w", cfg.Resources.Timeout, err)
		}
		f.timeout = d
	}
	str("agent", cfg.Agent.Adapter, &f.agent)
	str("on-question", cfg.Questions.Mode, &f.onQuestion)
	str("on-question-timeout", cfg.Questions.OnTimeout, &f.onQuestionTimeout)
	if !changed("question-timeout") && cfg.Questions.Timeout != "" {
		d, err := time.ParseDuration(cfg.Questions.Timeout)
		if err != nil {
			return fmt.Errorf("config question timeout %q: %w", cfg.Questions.Timeout, err)
		}
		f.questionTimeout = d
	}
	f.env = cfg.Env // non-secret container env comes from the file (§8.1)

	// Resolve the container hardening policy from the file (§8.1). No flags overlay it in v1, so
	// this is config-file only; validation (seccomp mode, capability allow/deny-list) runs here so
	// a bad value fails fast at load, before any VM boots.
	seccomp, err := task.ParseSeccompMode(cfg.Container.Seccomp)
	if err != nil {
		return fmt.Errorf("config container.seccomp: %w", err)
	}
	caps, err := task.NormalizeCapabilities(cfg.Container.Capabilities)
	if err != nil {
		return fmt.Errorf("config container.capabilities: %w", err)
	}
	f.container = task.ContainerPolicy{
		AddCapabilities:   caps,
		SeccompUnconfined: seccomp == task.SeccompUnconfined,
		ReadonlyRootfs:    cfg.Container.ReadonlyRootfs != nil && *cfg.Container.ReadonlyRootfs,
	}
	return nil
}

// newRunID returns a short unique run identifier, e.g. "run_2f9c1a3b".
func newRunID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate run id: %w", err)
	}
	return "run_" + hex.EncodeToString(b[:]), nil
}
