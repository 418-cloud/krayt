//go:build integration && darwin

// Real-VM integration test for the Phase 2 "Done when": on a real Apple-Silicon Mac with
// vfkit, `krayt` boots the base image, pushes a trivial user image + a repo snapshot, runs
// the container, and collects a changes.patch that applies cleanly to the host repo (§14).
// This cannot run in a cloud agent — it needs virtualization hardware, a base VM image
// whose closure includes git + containerd (§11.6), and a trivial user image that edits a
// file in /workspace — so it is gated behind the `integration` build tag and run by a
// human / CI on a Mac. See HUMAN_TODO.md.
//
// Run:
//
//	KRAYT_KERNEL=…/vmlinuz KRAYT_INITRD=…/initrd KRAYT_ROOTFS=…/rootfs.img \
//	KRAYT_IMAGE=ghcr.io/you/trivial-edit-agent:latest \
//	  go test -tags 'integration darwin' -run TestEndToEndRealVM -v ./internal/orchestrator/
package orchestrator_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/imagestore"
	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/patch"
	"github.com/418-cloud/krayt/internal/provider"
	"github.com/418-cloud/krayt/internal/provider/vfkit"
	"github.com/418-cloud/krayt/internal/task"
)

func TestEndToEndRealVM(t *testing.T) {
	kernel := os.Getenv("KRAYT_KERNEL")
	initrd := os.Getenv("KRAYT_INITRD")
	rootfs := os.Getenv("KRAYT_ROOTFS")
	image := os.Getenv("KRAYT_IMAGE")
	if kernel == "" || initrd == "" || rootfs == "" || image == "" {
		t.Skip("set KRAYT_KERNEL, KRAYT_INITRD, KRAYT_ROOTFS, KRAYT_IMAGE to run")
	}
	cmdline := os.Getenv("KRAYT_CMDLINE")
	if cmdline == "" {
		cmdline = "console=hvc0 root=/dev/vda"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})

	cacheRoot := t.TempDir()
	imgSrc, err := imagestore.Remote(image)
	if err != nil {
		t.Fatalf("imagestore.Remote: %v", err)
	}
	img, err := imagestore.Acquire(ctx, imgSrc, image, cacheRoot)
	if err != nil {
		t.Fatalf("imagestore.Acquire: %v", err)
	}

	runDir := filepath.Join(t.TempDir(), "run")
	spec := task.RunSpec{
		ID:          "run_integration",
		ImageRef:    image,
		RepoPath:    src,
		BundleDepth: 1,
		TaskPrompt:  []byte("make a trivial edit"),
		Resources:   task.Resources{CPUs: 2, MemoryMiB: 2048, Timeout: 4 * time.Minute},
	}
	deps := orchestrator.Deps{
		Provider: vfkit.New("", t.TempDir()),
		BaseVM: provider.VMSpec{
			Kernel:  kernel,
			Initrd:  initrd,
			RootFS:  rootfs,
			Cmdline: cmdline,
		},
		Image:  img,
		LogOut: os.Stderr,
	}

	res, err := orchestrator.Run(ctx, deps, spec, runDir)
	if err != nil {
		t.Fatalf("orchestrator.Run: %v", err)
	}
	t.Logf("run complete: exit=%d patch=%s", res.ExitCode, res.PatchPath)

	if fi, err := os.Stat(res.PatchPath); err != nil || fi.Size() == 0 {
		t.Fatalf("changes.patch missing/empty: err=%v", err)
	}
	// The patch must apply cleanly back onto a fresh checkout of the host repo.
	target := filepath.Join(t.TempDir(), "target")
	if out, err := exec.Command("git", "clone", "--quiet", src, target).CombinedOutput(); err != nil {
		t.Fatalf("clone target: %v\n%s", err, out)
	}
	if err := patch.Apply(ctx, target, res.PatchPath, false); err != nil {
		t.Fatalf("krayt apply failed: %v", err)
	}
}

// TestEgressEnforcement is the real-VM proof of the Phase 3 "Done when" egress clauses: with
// an allowlist policy, the container reaches an allowlisted host through the proxy but is
// blocked from a non-allowlisted host AND from a raw (non-proxied) socket — the nftables L3
// lock. This needs real virtualization + nftables + network, so it is gated and run on a
// Mac/CI (§14). KRAYT_NETPROBE_IMAGE must be a linux/arm64 image whose entrypoint probes
// egress and exits 0 only when: HTTPS to KRAYT_ALLOW_HOST via HTTPS_PROXY succeeds, HTTPS to
// a non-allowlisted host fails, and a raw TCP connect (ignoring HTTP(S)_PROXY) to a
// non-allowlisted host:443 fails. See HUMAN_TODO.md for the probe-image contract.
func TestEgressEnforcement(t *testing.T) {
	kernel, initrd, rootfs := os.Getenv("KRAYT_KERNEL"), os.Getenv("KRAYT_INITRD"), os.Getenv("KRAYT_ROOTFS")
	image := os.Getenv("KRAYT_NETPROBE_IMAGE")
	allowHost := os.Getenv("KRAYT_ALLOW_HOST")
	if kernel == "" || initrd == "" || rootfs == "" || image == "" || allowHost == "" {
		t.Skip("set KRAYT_KERNEL, KRAYT_INITRD, KRAYT_ROOTFS, KRAYT_NETPROBE_IMAGE, KRAYT_ALLOW_HOST to run")
	}
	cmdline := os.Getenv("KRAYT_CMDLINE")
	if cmdline == "" {
		cmdline = "console=hvc0 root=/dev/vda"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	src := newRepo(t, map[string]string{"probe.txt": "run\n"})
	imgSrc, err := imagestore.Remote(image)
	if err != nil {
		t.Fatalf("imagestore.Remote: %v", err)
	}
	img, err := imagestore.Acquire(ctx, imgSrc, image, t.TempDir())
	if err != nil {
		t.Fatalf("imagestore.Acquire: %v", err)
	}

	runDir := filepath.Join(t.TempDir(), "run")
	spec := task.RunSpec{
		ID: "run_egress", ImageRef: image, RepoPath: src, BundleDepth: 1,
		TaskPrompt: []byte("probe egress"),
		Network:    task.NetworkPolicy{Mode: task.NetworkAllowlist, Allow: []string{allowHost}},
		Resources:  task.Resources{CPUs: 2, MemoryMiB: 2048, Timeout: 4 * time.Minute},
	}
	deps := orchestrator.Deps{
		Provider: vfkit.New("", t.TempDir()),
		BaseVM:   provider.VMSpec{Kernel: kernel, Initrd: initrd, RootFS: rootfs, Cmdline: cmdline},
		Image:    img,
		LogOut:   os.Stderr,
	}

	res, err := orchestrator.Run(ctx, deps, spec, runDir)
	if err != nil {
		t.Fatalf("orchestrator.Run: %v", err)
	}
	// The probe image encodes the expected allow/deny/raw-socket behavior and exits 0 only
	// when the enforcement is correct.
	if res.ExitCode != 0 {
		t.Fatalf("egress probe failed (exit %d): allowlisted reach, non-allowlisted block, or "+
			"raw-socket block did not behave as expected — see logs/agent.log", res.ExitCode)
	}
}
