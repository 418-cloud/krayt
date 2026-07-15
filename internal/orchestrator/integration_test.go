//go:build integration

// Real-VM integration test for the Phase 2 "Done when": on real virtualization hardware,
// `krayt` boots the base image, pushes a trivial user image + a repo snapshot, runs the
// container, and collects a changes.patch that applies cleanly to the host repo (§14).
//
// This runs against BOTH backends, unchanged. The test body is provider-agnostic — it is the
// same orchestrator, protocol, patch and secrets code on either OS (§6.3) — so the only
// platform-specific thing here is which Provider gets constructed, and that lives behind
// newTestProvider() in integration_provider_{darwin,linux}_test.go. Phase 7's "Done when" is
// exactly this: these tests passing on Linux/firecracker with nothing but that seam swapped.
//
// It needs virtualization hardware, a base VM image whose closure includes git + containerd
// (§11.6), and a trivial user image that edits a file in /workspace, so it is gated behind the
// `integration` build tag. On macOS that means a human on an Apple-Silicon Mac (see
// HUMAN_TODO.md); on Linux any host with /dev/kvm will do, including CI.
//
// Run (macOS/vfkit):
//
//	KRAYT_KERNEL=…/vmlinuz KRAYT_INITRD=…/initrd KRAYT_ROOTFS=…/rootfs.img \
//	KRAYT_IMAGE=ghcr.io/you/trivial-edit-agent:latest \
//	  go test -tags integration -run TestEndToEndRealVM -v ./internal/orchestrator/
//
// Run (Linux/firecracker — the test binary needs CAP_NET_ADMIN for the VM's tap device):
//
//	go test -c -tags integration -o /tmp/orch.test ./internal/orchestrator/
//	sudo setcap cap_net_admin+ep /tmp/orch.test
//	KRAYT_KERNEL=… KRAYT_INITRD=… KRAYT_ROOTFS=… KRAYT_IMAGE=… \
//	  /tmp/orch.test -test.run TestEndToEndRealVM -test.v
package orchestrator_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/imagestore"
	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/patch"
	"github.com/418-cloud/krayt/internal/provider"
	"github.com/418-cloud/krayt/internal/task"
)

// logConsoleOnFailure prints the guest's serial console log — the guest-agent's own
// stdout/stderr and anything it execs (proxyd included) — which is not part of
// logs/agent.log (that file is the container's stdout/stderr only, streamed over the control
// protocol). orchestrator.Run copies it out of the VM's directory before Destroy removes it,
// but t.TempDir() still deletes the whole run dir when this test function returns, so a
// failure that needs it must log it now, in the same -test.v output the failure itself
// appears in, or it is gone before any human or CI log viewer could ever read it.
func logConsoleOnFailure(t *testing.T, runDir string) {
	t.Helper()
	if b, err := os.ReadFile(orchestrator.ConsoleLogPath(runDir)); err == nil && len(b) > 0 {
		t.Logf("guest console log:\n%s", b)
	}
}

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
		Provider: newTestProvider(t),
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
		logConsoleOnFailure(t, runDir)
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
//
// Together with TestContainerHardening's setuid(proxyd)=EPERM assertion, this is the on-hardware
// egress-allowlist-bypass regression for finding #1 (fix-egress-allowlist-bypass.md): the direct
// non-allowlisted connect being dropped proves the L3 `skuid "proxyd"` lock holds, and the EPERM
// proves the container cannot assume proxyd's uid to satisfy it. The cheap offline counterpart is
// TestEgressRulesetShape in internal/guest/proxy.
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
		Provider: newTestProvider(t),
		BaseVM:   provider.VMSpec{Kernel: kernel, Initrd: initrd, RootFS: rootfs, Cmdline: cmdline},
		Image:    img,
		LogOut:   os.Stderr,
	}

	res, err := orchestrator.Run(ctx, deps, spec, runDir)
	if err != nil {
		logConsoleOnFailure(t, runDir)
		t.Fatalf("orchestrator.Run: %v", err)
	}
	// The probe image encodes the expected allow/deny/raw-socket behavior and exits 0 only
	// when the enforcement is correct.
	if res.ExitCode != 0 {
		logConsoleOnFailure(t, runDir)
		t.Fatalf("egress probe failed (exit %d): allowlisted reach, non-allowlisted block, or "+
			"raw-socket block did not behave as expected — see the guest console log above", res.ExitCode)
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
		Provider: newTestProvider(t),
		BaseVM:   provider.VMSpec{Kernel: kernel, Initrd: initrd, RootFS: rootfs, Cmdline: cmdline},
		Image:    img,
		LogOut:   os.Stderr,
	}
	res, err := orchestrator.Run(ctx, deps, spec, runDir)
	if err != nil {
		logConsoleOnFailure(t, runDir)
		t.Fatalf("orchestrator.Run: %v", err)
	}
	if res.ExitCode != 0 {
		logConsoleOnFailure(t, runDir)
		t.Fatalf("hardening probe failed (exit %d): caps, non-root, seccomp, no-new-privs, or the "+
			"setuid(proxyd)=EPERM check did not hold — see the guest console log above", res.ExitCode)
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
		Provider: newTestProvider(t),
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
// regressed, fsmonitor/textconv would have executed as root and created the sentinel file, which
// `git add -A` would then stage as its own new-file entry in changes.patch. The test asserts that
// entry is ABSENT (root code never ran) while the normal edit IS present (the patch is still
// faithful). It checks for the sentinel's own diff header rather than a bare substring match on
// "PWNED_BY_ROOT", because pwn.sh's own source necessarily contains that path as text — pwn.sh
// itself always lands in the patch as a normal added file, and a loose substring check would
// false-positive on that, not on the sentinel actually having run.
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
		Provider: newTestProvider(t),
		BaseVM:   provider.VMSpec{Kernel: kernel, Initrd: initrd, RootFS: rootfs, Cmdline: cmdline},
		Image:    img,
		LogOut:   os.Stderr,
	}
	res, err := orchestrator.Run(ctx, deps, spec, runDir)
	if err != nil {
		logConsoleOnFailure(t, runDir)
		t.Fatalf("orchestrator.Run: %v", err)
	}
	patchBytes, err := os.ReadFile(res.PatchPath)
	if err != nil || len(patchBytes) == 0 {
		t.Fatalf("changes.patch missing/empty: err=%v", err)
	}
	// The sentinel FILE (not just pwn.sh's source text, which always contains this path) must not
	// be a new-file entry in the patch — that would mean root git ran the injected config.
	if strings.Contains(string(patchBytes), "diff --git a/PWNED_BY_ROOT b/PWNED_BY_ROOT") {
		t.Fatalf("container→guest-root escape: injected git config executed as root (sentinel file present)\n%s", patchBytes)
	}
	// The patch is still faithful — the normal edit landed.
	if !strings.Contains(string(patchBytes), "greeting.txt") {
		t.Errorf("changes.patch missing the real edit:\n%s", patchBytes)
	}
}

// TestSecretConfinementInArtifacts is the on-metal proof of the §6.8/§10 secret-confinement
// extension (finding: redaction only covered live logs). It cannot run in a cloud agent — it
// needs virtualization hardware, the base VM image, and a linux/arm64 NON-ROOT probe image that
// leaks its mounted credential three ways. The unit tests
// (`internal/orchestrator`: TestSecretRedactedInReportAndFlaggedInPatch, TestSecretRedactedInQuestion;
// `internal/secrets`: TestScanKeys) already prove the guest logic against the fake provider; this
// confirms it end-to-end through real containerd + the tmpfs `/run/secrets` mount. See HUMAN_TODO.md.
//
// KRAYT_SECRETS_IMAGE must be a linux/arm64, non-root image whose entrypoint reads
// `/run/secrets/ANTHROPIC_API_KEY` (value $K) and then:
//   - writes `/output/report.md` containing $K (e.g. "key is $K"),
//   - writes a tracked source file, e.g. `/workspace/config.txt` = "api_key=$K",
//   - asks via `krayt-ask --choices "use $K,skip" "Use the key $K?"` (may ignore the answer),
//   - exits 0.
//
// Run:
//
//	KRAYT_KERNEL=…/vmlinuz KRAYT_INITRD=…/initrd KRAYT_ROOTFS=…/rootfs.img \
//	KRAYT_SECRETS_IMAGE=ghcr.io/you/krayt-secrets-probe:latest \
//	  go test -tags 'integration darwin' -run TestSecretConfinementInArtifacts -v ./internal/orchestrator/
func TestSecretConfinementInArtifacts(t *testing.T) {
	kernel, initrd, rootfs := os.Getenv("KRAYT_KERNEL"), os.Getenv("KRAYT_INITRD"), os.Getenv("KRAYT_ROOTFS")
	image := os.Getenv("KRAYT_SECRETS_IMAGE")
	if kernel == "" || initrd == "" || rootfs == "" || image == "" {
		t.Skip("set KRAYT_KERNEL, KRAYT_INITRD, KRAYT_ROOTFS, KRAYT_SECRETS_IMAGE to run")
	}
	cmdline := os.Getenv("KRAYT_CMDLINE")
	if cmdline == "" {
		cmdline = "console=hvc0 root=/dev/vda"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The value the probe image will read from /run/secrets and (carelessly) spray around. It must
	// be distinctive so the assertions below can look for it verbatim.
	const secretVal = "sk-ant-integration-secret-0123456789"
	secretsFile := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(secretsFile, []byte("ANTHROPIC_API_KEY="+secretVal+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

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
		ID: "run_secret_confine", ImageRef: image, RepoPath: src, BundleDepth: 1,
		TaskPrompt:  []byte("leak the secret three ways"),
		SecretsPath: secretsFile,
		// Wait mode so the ask_human prompt is persisted to questions/<id>.json; a short per-question
		// timeout sentinels it (default on-timeout = continue) so the run never blocks with no human.
		Questions: task.QuestionsPolicy{Mode: task.QuestionWait, Timeout: 20 * time.Second},
		Resources: task.Resources{CPUs: 2, MemoryMiB: 2048, Timeout: 4 * time.Minute},
	}
	deps := orchestrator.Deps{
		Provider: newTestProvider(t),
		BaseVM:   provider.VMSpec{Kernel: kernel, Initrd: initrd, RootFS: rootfs, Cmdline: cmdline},
		Image:    img,
		LogOut:   os.Stderr,
	}
	res, err := orchestrator.Run(ctx, deps, spec, runDir)
	if err != nil {
		logConsoleOnFailure(t, runDir)
		t.Fatalf("orchestrator.Run: %v", err)
	}

	// report.md (agent notes, folded by the host) is redacted in the guest.
	rep, err := os.ReadFile(filepath.Join(runDir, "report.md"))
	if err != nil {
		t.Fatalf("read report.md: %v", err)
	}
	if strings.Contains(string(rep), secretVal) {
		t.Errorf("secret value leaked into report.md:\n%s", rep)
	}

	// The persisted ask_human question is redacted (prompt + choices crossed the bridge).
	qs, err := orchestrator.ReadQuestions(runDir)
	if err != nil || len(qs) == 0 {
		t.Fatalf("expected a persisted question; got %+v (err %v)", qs, err)
	}
	if strings.Contains(qs[0].Prompt, secretVal) {
		t.Errorf("secret leaked into the persisted question prompt: %q", qs[0].Prompt)
	}
	for _, c := range qs[0].Choices {
		if strings.Contains(c, secretVal) {
			t.Errorf("secret leaked into a persisted choice: %q", c)
		}
	}

	// changes.patch is byte-exact (secret present, NOT redacted) but flagged in Safety…
	patchBytes, err := os.ReadFile(res.PatchPath)
	if err != nil {
		t.Fatalf("read changes.patch: %v", err)
	}
	if !strings.Contains(string(patchBytes), secretVal) {
		t.Errorf("changes.patch should be byte-exact with the secret present; got:\n%s", patchBytes)
	}
	flagged := false
	for _, s := range res.Safety {
		if strings.Contains(s, "ANTHROPIC_API_KEY") && strings.Contains(s, "changes.patch") {
			flagged = true
		}
	}
	if !flagged {
		t.Errorf("Safety should flag the secret in the patch; got %v", res.Safety)
	}

	// …and the value never reaches meta.json (or secret-scan.json, which names the key only).
	meta, err := os.ReadFile(filepath.Join(runDir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(meta), secretVal) {
		t.Errorf("secret value leaked into meta.json")
	}
	scan, err := os.ReadFile(filepath.Join(runDir, "secret-scan.json"))
	if err != nil {
		t.Fatalf("secret-scan.json missing: %v", err)
	}
	if !strings.Contains(string(scan), "ANTHROPIC_API_KEY") || strings.Contains(string(scan), secretVal) {
		t.Errorf("secret-scan.json should name the key only; got: %s", scan)
	}
}

// TestConcurrentRealVMs is the on-hardware counterpart of TestConcurrentRuns (which proves the
// same property against the fakeProvider): several runs execute at once, each in its own VM,
// and each comes back with a patch for its own repo and nobody else's.
//
// On the firecracker backend this is also the regression test for the provider's per-VM
// resource allocation, which has no macOS analogue: every VM needs its own tap device, its own
// /30, its own vsock CID and its own unix sockets, all handed out atomically (§6.12,
// firecracker/tap.go). Get any of that wrong and the failure is not a crash — it is two VMs
// quietly sharing a network or a control channel, which is exactly the kind of bug that would
// otherwise surface as a mysteriously cross-contaminated patch.
func TestConcurrentRealVMs(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cacheRoot := t.TempDir()
	imgSrc, err := imagestore.Remote(image)
	if err != nil {
		t.Fatalf("imagestore.Remote: %v", err)
	}
	img, err := imagestore.Acquire(ctx, imgSrc, image, cacheRoot)
	if err != nil {
		t.Fatalf("imagestore.Acquire: %v", err)
	}

	const runs = 3
	type result struct {
		src   string
		patch string
		err   error
	}
	results := make([]result, runs)

	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each run gets a repo with distinct content, so a patch that leaked from another
			// VM cannot apply here — isolation is checked by construction, not by inspection.
			marker := fmt.Sprintf("run-%d-marker\n", i)
			src := newRepo(t, map[string]string{"greeting.txt": marker})
			results[i].src = src

			runDir := filepath.Join(t.TempDir(), "run")
			spec := task.RunSpec{
				ID:          fmt.Sprintf("run_concurrent_%d", i),
				ImageRef:    image,
				RepoPath:    src,
				BundleDepth: 1,
				TaskPrompt:  []byte("make a trivial edit"),
				Resources:   task.Resources{CPUs: 2, MemoryMiB: 2048, DiskGiB: 10, Timeout: 8 * time.Minute},
			}
			deps := orchestrator.Deps{
				Provider: newTestProvider(t),
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
				logConsoleOnFailure(t, runDir) // t.Logf is goroutine-safe; t.Fatalf is not, hence results[i].err below
				results[i].err = err
				return
			}
			results[i].patch = res.PatchPath
		}()
	}
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Fatalf("run %d: %v", i, r.err)
		}
		b, err := os.ReadFile(r.patch)
		if err != nil || len(b) == 0 {
			t.Fatalf("run %d: changes.patch missing/empty: %v", i, err)
		}
		// The patch must carry THIS run's marker: proof the VMs did not share a workspace.
		if want := fmt.Sprintf("run-%d-marker", i); !strings.Contains(string(b), want) {
			t.Errorf("run %d: patch does not mention %q — patches crossed between VMs:\n%s", i, want, b)
		}
		// And it must still apply cleanly to its own repo.
		target := filepath.Join(t.TempDir(), fmt.Sprintf("target-%d", i))
		if out, err := exec.Command("git", "clone", "--quiet", r.src, target).CombinedOutput(); err != nil {
			t.Fatalf("run %d: clone target: %v\n%s", i, err, out)
		}
		if err := patch.Apply(ctx, target, r.patch, false); err != nil {
			t.Fatalf("run %d: krayt apply failed: %v", i, err)
		}
		t.Logf("run %d: patch applies cleanly to its own repo", i)
	}
}
