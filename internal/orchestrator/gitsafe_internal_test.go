package orchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runTrustScript runs trustWorkspaceScript with this host's sh and git, the way the guest runs it:
// as the user whose $HOME holds the global config. XDG_CONFIG_HOME is pinned too, since git also
// reads a global config from there.
func runTrustScript(t *testing.T, home, path, dir string) (string, error) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not installed")
	}
	cmd := exec.Command(sh, "-c", trustWorkspaceScript, "sh", dir)
	cmd.Env = []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "PATH=" + path, "GIT_CONFIG_NOSYSTEM=1"}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestTrustWorkspaceScript runs the real script against the real git: it adds the directory once,
// is idempotent, turns a "dubious ownership" refusal into a working git (simulated with git's own
// GIT_TEST_ASSUME_DIFFERENT_OWNER), is a no-op without git, and fails loudly without a writable
// $HOME.
func TestTrustWorkspaceScript(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	path := filepath.Dir(gitPath) + ":/usr/bin:/bin"
	home := t.TempDir()
	repo := filepath.Join(t.TempDir(), "workspace")
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	gitStatus := func() error {
		cmd := exec.Command("git", "-C", repo, "status")
		cmd.Env = []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "PATH=" + path,
			"GIT_CONFIG_NOSYSTEM=1", "GIT_TEST_ASSUME_DIFFERENT_OWNER=1"}
		out, err := cmd.CombinedOutput()
		if err != nil && !strings.Contains(string(out), "dubious ownership") {
			t.Fatalf("git status failed for another reason: %v: %s", err, out)
		}
		return err
	}
	if gitStatus() == nil {
		t.Fatal("precondition: git status should refuse the repo before it is trusted")
	}

	if out, err := runTrustScript(t, home, path, repo); err != nil || out != "trusted" {
		t.Fatalf("first run: %q, %v", out, err)
	}
	if out, err := runTrustScript(t, home, path, repo); err != nil || out != "already trusted" {
		t.Fatalf("second run: %q, %v", out, err)
	}
	cfg, err := os.ReadFile(filepath.Join(home, ".gitconfig"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(cfg), repo); n != 1 {
		t.Errorf(".gitconfig names %s %d times, want exactly once:\n%s", repo, n, cfg)
	}
	if err := gitStatus(); err != nil {
		t.Errorf("git status still refuses the trusted repo: %v", err)
	}

	emptyPath := t.TempDir()
	if out, err := runTrustScript(t, t.TempDir(), emptyPath, repo); err != nil || out != "no git" {
		t.Errorf("without git: %q, %v; want a clean no-op", out, err)
	}

	out, err := runTrustScript(t, filepath.Join(t.TempDir(), "missing", "home"), path, repo)
	if err == nil || !strings.Contains(out, "krayt-git-safe-directory") {
		t.Errorf("unwritable HOME: %q, %v; want a failure naming the script", out, err)
	}
}
