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
	image        string
	taskFile     string
	repo         string
	secretsFile  string
	includeDirty bool
	netMode      string
	allow        []string
	bundleDepth  int
	cpus         int
	memory       uint64
	disk         uint64
	timeout      time.Duration
	detach       bool
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
	fl := cmd.Flags()
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
	return cmd
}

func runRun(cmd *cobra.Command, f *runFlags) error {
	if f.image == "" || f.taskFile == "" {
		return fmt.Errorf("--image and --task are required")
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
	netMode := task.NetworkMode(f.netMode)
	switch netMode {
	case task.NetworkAllowlist, task.NetworkFull, task.NetworkNone:
	default:
		return fmt.Errorf("--net must be allowlist, full, or none (got %q)", f.netMode)
	}
	// --allow only means anything under allowlist mode; `full` allows all and `none` denies
	// all regardless, so reject the combination rather than silently ignoring it (§6.6).
	if netMode != task.NetworkAllowlist && len(f.allow) > 0 {
		return fmt.Errorf("--allow can only be used with --net allowlist")
	}
	spec := task.RunSpec{
		ID:           id,
		ImageRef:     f.image,
		RepoPath:     repoAbs,
		SecretsPath:  secretsPath,
		IncludeDirty: f.includeDirty,
		Network:      task.NetworkPolicy{Mode: netMode, Allow: f.allow},
		BundleDepth:  f.bundleDepth,
		TaskPrompt:   prompt,
		Detach:       f.detach,
		Resources: task.Resources{
			CPUs:      f.cpus,
			MemoryMiB: f.memory,
			DiskGiB:   f.disk,
			Timeout:   f.timeout,
		},
	}

	runDir := filepath.Join(repoAbs, ".krayt", "runs", id)

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
	res, err := orchestrator.Run(cmd.Context(), orchestrator.Deps{
		Provider: deps.provider,
		BaseVM:   deps.baseVM,
		Image:    img,
		LogOut:   logOut,
	}, spec, runDir)
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

// newRunID returns a short unique run identifier, e.g. "run_2f9c1a3b".
func newRunID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate run id: %w", err)
	}
	return "run_" + hex.EncodeToString(b[:]), nil
}
