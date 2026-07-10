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
	"strings"
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

// TestContainerHardening is the real-VM proof of the least-privilege OCI spec (§6.10, §10,
// findings #1/#3): the default container drops all capabilities, runs a non-root uid, has the
// seccomp filter engaged, keeps no-new-privs, and cannot setuid to proxyd (the egress bypass).
// It needs real virtualization + containerd + nftables, so it is gated and run on a Mac/CI (§14).
// KRAYT_HARDENING_IMAGE must be a linux/arm64, NON-ROOT (e.g. USER 1000) image whose entrypoint
// asserts and exits 0 ONLY when all of the following hold (see HUMAN_TODO.md for the contract):
//   - /proc/self/status: CapEff == 0000000000000000, CapAmb == 0000000000000000, NoNewPrivs == 1,
//     Seccomp == 2 (SECCOMP_MODE_FILTER)
//   - `id -u` != 0
//   - setuid(<proxyd uid, read from /proc/net or brute-forced over the system-uid range>) fails
//     with EPERM (the egress-allowlist-bypass regression, shared with fix-egress-allowlist-bypass)
func TestContainerHardening(t *testing.T) {
	kernel, initrd, rootfs := os.Getenv("KRAYT_KERNEL"), os.Getenv("KRAYT_INITRD"), os.Getenv("KRAYT_ROOTFS")
	image := os.Getenv("KRAYT_HARDENING_IMAGE")
	if kernel == "" || initrd == "" || rootfs == "" || image == "" {
		t.Skip("set KRAYT_KERNEL, KRAYT_INITRD, KRAYT_ROOTFS, KRAYT_HARDENING_IMAGE to run")
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
		ID: "run_hardening", ImageRef: image, RepoPath: src, BundleDepth: 1,
		TaskPrompt: []byte("probe hardening"),
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
	if res.ExitCode != 0 {
		t.Fatalf("hardening probe failed (exit %d): caps, non-root, seccomp, no-new-privs, or the "+
			"setuid(proxyd)=EPERM check did not hold — see logs/agent.log", res.ExitCode)
	}
}

// TestRootImageFailsClosed is the negative proof of the enforced-non-root control (§8.2): an image
// whose USER is root (uid 0) must fail the run with a clear error, never launch. KRAYT_ROOT_IMAGE
// must be a linux/arm64 image with `USER root` (or no USER). Gated + run on hardware (§14).
func TestRootImageFailsClosed(t *testing.T) {
	kernel, initrd, rootfs := os.Getenv("KRAYT_KERNEL"), os.Getenv("KRAYT_INITRD"), os.Getenv("KRAYT_ROOTFS")
	image := os.Getenv("KRAYT_ROOT_IMAGE")
	if kernel == "" || initrd == "" || rootfs == "" || image == "" {
		t.Skip("set KRAYT_KERNEL, KRAYT_INITRD, KRAYT_ROOTFS, KRAYT_ROOT_IMAGE to run")
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
		ID: "run_rootimg", ImageRef: image, RepoPath: src, BundleDepth: 1,
		TaskPrompt: []byte("should not run"),
		Resources:  task.Resources{CPUs: 2, MemoryMiB: 2048, Timeout: 4 * time.Minute},
	}
	deps := orchestrator.Deps{
		Provider: vfkit.New("", t.TempDir()),
		BaseVM:   provider.VMSpec{Kernel: kernel, Initrd: initrd, RootFS: rootfs, Cmdline: cmdline},
		Image:    img,
		LogOut:   os.Stderr,
	}
	// The run reports the non-root error on the terminal Status (surfaced as a run failure), so
	// orchestrator.Run returns an error mentioning uid 0 — the container never executes.
	if _, err := orchestrator.Run(ctx, deps, spec, runDir); err == nil {
		t.Fatal("expected a root (uid 0) image to fail the run; it did not")
	} else if !strings.Contains(err.Error(), "root") && !strings.Contains(err.Error(), "uid 0") {
		t.Fatalf("error should name the non-root requirement; got %v", err)
	}
}

// TestGuestGitConfigInjectionInert is the real-VM proof that a container cannot use its writable
// `/workspace/.git` to make the ROOT guest-agent's git execute attacker-controlled config (§6.7,
// §10 finding #2 — the container→guest-root escape). The guest generates the patch from a root-only
// `patchgit` snapshot with `core.fsmonitor`/`core.hooksPath` force-cleared and `--no-textconv`, so
// the injected config must be inert.
//
// KRAYT_GITCONFIG_IMAGE must be a linux/arm64, non-root image whose entrypoint (see HUMAN_TODO.md
// for the exact contract):
//   - writes an executable `/workspace/pwn.sh` that, if it ever runs, creates a sentinel file
//     `/workspace/PWNED_BY_ROOT` (a path that would land in the collected changes.patch);
//   - appends `[core]\n\tfsmonitor = /workspace/pwn.sh` (and a `[diff "evil"] textconv = /workspace/pwn.sh`
//     driver) to `/workspace/.git/config`, plus `* diff=evil` to `/workspace/.gitattributes`;
//   - makes one normal tracked edit (e.g. append to an existing file);
//   - exits 0.
//
// After the run, the guest's root git ran `add -A` + `diff` during patch generation. If the fix
// regressed, fsmonitor/textconv would have executed as root and created the sentinel, which would
// appear in changes.patch. The test asserts the sentinel is ABSENT from the patch (root code never
// ran) while the normal edit IS present (the patch is still faithful).
func TestGuestGitConfigInjectionInert(t *testing.T) {
	kernel, initrd, rootfs := os.Getenv("KRAYT_KERNEL"), os.Getenv("KRAYT_INITRD"), os.Getenv("KRAYT_ROOTFS")
	image := os.Getenv("KRAYT_GITCONFIG_IMAGE")
	if kernel == "" || initrd == "" || rootfs == "" || image == "" {
		t.Skip("set KRAYT_KERNEL, KRAYT_INITRD, KRAYT_ROOTFS, KRAYT_GITCONFIG_IMAGE to run")
	}
	cmdline := os.Getenv("KRAYT_CMDLINE")
	if cmdline == "" {
		cmdline = "console=hvc0 root=/dev/vda"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})
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
		ID: "run_gitconfig", ImageRef: image, RepoPath: src, BundleDepth: 1,
		TaskPrompt: []byte("inject git config"),
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
	patchBytes, err := os.ReadFile(res.PatchPath)
	if err != nil || len(patchBytes) == 0 {
		t.Fatalf("changes.patch missing/empty: err=%v", err)
	}
	// The escape sentinel must NOT be in the patch — root git never executed the injected config.
	if strings.Contains(string(patchBytes), "PWNED_BY_ROOT") {
		t.Fatalf("container→guest-root escape: injected git config executed as root (sentinel present)\n%s", patchBytes)
	}
	// The patch is still faithful — the normal edit landed.
	if !strings.Contains(string(patchBytes), "greeting.txt") {
		t.Errorf("changes.patch missing the real edit:\n%s", patchBytes)
	}
}
