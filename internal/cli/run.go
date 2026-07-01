package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/imagestore"
	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/provider"
	"github.com/418-cloud/krayt/internal/task"
)

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

	onQuestion        string        // fail | wait (§6.13)
	questionTimeout   time.Duration // per-question wait limit
	onQuestionTimeout string        // sentinel | abort
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
	fl.StringVar(&f.taskFile, "task", "", "path to the task prompt file (required)")
	fl.StringVar(&f.repo, "repo", ".", "host repo to bundle")
	fl.StringVar(&f.secretsFile, "secrets", "", "per-task secrets file (KEY=VALUE), mounted on tmpfs at /run/secrets")
	fl.BoolVar(&f.includeDirty, "include-dirty", false, "include uncommitted working-tree changes in the bundle")
	fl.StringVar(&f.netMode, "net", "allowlist", "egress policy: allowlist | full | none")
	fl.StringArrayVar(&f.allow, "allow", nil, "allowlisted egress domain (repeatable); only with --net allowlist")
	fl.IntVar(&f.bundleDepth, "bundle-depth", 1, "forward-bundle shallow depth (0 = full history)")
	fl.IntVar(&f.cpus, "cpus", 2, "vCPUs")
	fl.Uint64Var(&f.memory, "memory", 4096, "memory (MiB)")
	fl.Uint64Var(&f.disk, "disk", 20, "disk (GiB)")
	fl.DurationVar(&f.timeout, "timeout", 30*time.Minute, "wall-clock run timeout")
	fl.BoolVar(&f.detach, "detach", false, "headless: do not stream logs to the terminal")
	fl.StringVar(&f.onQuestion, "on-question", "fail", "agent question mode: fail (autonomous) | wait (pause for input)")
	fl.DurationVar(&f.questionTimeout, "question-timeout", 10*time.Minute, "per-question wait timeout")
	fl.StringVar(&f.onQuestionTimeout, "on-question-timeout", "sentinel", "on question timeout: sentinel | abort")
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
	prompt, err := os.ReadFile(f.taskFile)
	if err != nil {
		return fmt.Errorf("read task file: %w", err)
	}
	repoAbs, err := filepath.Abs(f.repo)
	if err != nil {
		return err
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
	}

	// OS-specific provider + base VM image (vfkit on macOS; error elsewhere until Phase 6).
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
	}, filepath.Join(repoAbs, ".krayt"), 0)
	res, err := mgr.Run(cmd.Context(), spec)
	if err != nil {
		return err
	}

	summary := fmt.Sprintf("\nrun %s complete (exit %d)\n  patch:  %s\n", id, res.ExitCode, res.PatchPath)
	if res.CommitsBundle != "" {
		summary += fmt.Sprintf("  commits: %s\n", res.CommitsBundle)
	}
	summary += fmt.Sprintf("  apply:  krayt apply %s\n", id)
	_, err = fmt.Fprint(cmd.OutOrStdout(), summary)
	return err
}

// acquireUserImage pulls the user image into the host cache and returns it (§6.11).
func acquireUserImage(cmd *cobra.Command, ref string) (*imagestore.Image, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("resolve cache dir: %w", err)
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

// applyConfig loads krayt.yaml (explicit --config, else <repo>/krayt.yaml if present) and
// overlays it under the flags: a config value is used only when its flag was not set on the
// command line, so flags always win (§8.3).
func applyConfig(cmd *cobra.Command, f *runFlags) error {
	path := f.config
	if path == "" {
		def := filepath.Join(f.repo, "krayt.yaml")
		if _, err := os.Stat(def); err != nil {
			return nil // no config file and none requested
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
