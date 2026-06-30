package patch_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/418-cloud/krayt/internal/patch"
)

// TestRoundTrip is the patch-package proof of the Phase 2 "Done when": a self-contained
// bundle of a host repo is ingested into a workspace, an agent edits one file, the diff
// against the recorded baseline is produced, and that changes.patch applies cleanly back
// onto a fresh checkout of the host repo (§6.7). The orchestrator e2e test drives the same
// path through gRPC + the fakeProvider.
func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t, map[string]string{
		"greeting.txt": "hello\n",
		"keep.txt":     "unchanged\n",
	})

	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	if err := patch.CreateBundle(ctx, src, bundle, 1); err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}

	ws := filepath.Join(t.TempDir(), "workspace")
	baseline, err := patch.Ingest(ctx, bundle, ws, patch.DefaultIdentity)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if baseline == "" {
		t.Fatal("Ingest returned empty baseline")
	}
	// Baseline tag must exist and origin must be gone (§6.7).
	if out := git(t, ws, "tag", "--list", patch.BaselineTag); out == "" {
		t.Errorf("baseline tag %q not created", patch.BaselineTag)
	}
	if out := git(t, ws, "remote"); out != "" {
		t.Errorf("origin remote not dropped, remotes = %q", out)
	}

	// Agent edits one tracked file and adds a new one — without committing.
	writeFile(t, filepath.Join(ws, "greeting.txt"), "hello world\n")
	writeFile(t, filepath.Join(ws, "new.txt"), "fresh\n")

	patchBytes, err := patch.Diff(ctx, ws, patch.BaselineTag)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(patchBytes) == 0 {
		t.Fatal("Diff produced an empty patch for a non-empty edit")
	}

	// Apply onto a fresh checkout of the source repo and assert the edit landed.
	target := filepath.Join(t.TempDir(), "target")
	if _, err := exec.Command("git", "clone", "--quiet", src, target).CombinedOutput(); err != nil {
		t.Fatalf("clone target: %v", err)
	}
	patchFile := filepath.Join(t.TempDir(), "changes.patch")
	writeFile(t, patchFile, string(patchBytes))
	if err := patch.Apply(ctx, target, patchFile, false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := readFile(t, filepath.Join(target, "greeting.txt")); got != "hello world\n" {
		t.Errorf("greeting.txt after apply = %q, want %q", got, "hello world\n")
	}
	if got := readFile(t, filepath.Join(target, "new.txt")); got != "fresh\n" {
		t.Errorf("new.txt after apply = %q, want %q", got, "fresh\n")
	}
}

// TestIngestOutsideGitRepo guards the regression where `git bundle verify` failed in the
// guest with "need a repository to verify a bundle": it is a repository command, but the
// guest-agent's cwd is not a repo. The other tests masked this by running inside the krayt
// repo, so this one explicitly changes into a non-repo directory first (§6.7).
func TestIngestOutsideGitRepo(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	if err := patch.CreateBundle(ctx, src, bundle, 1); err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}

	// Run the rest from a directory that is NOT a git repository, like the guest.
	t.Chdir(t.TempDir())

	ws := filepath.Join(t.TempDir(), "workspace")
	baseline, err := patch.Ingest(ctx, bundle, ws, patch.DefaultIdentity)
	if err != nil {
		t.Fatalf("Ingest from non-repo cwd: %v", err)
	}
	if baseline == "" {
		t.Fatal("empty baseline")
	}
	writeFile(t, filepath.Join(ws, "a.txt"), "2\n")
	if got, err := patch.Diff(ctx, ws, patch.BaselineTag); err != nil || len(got) == 0 {
		t.Fatalf("Diff from non-repo cwd: err=%v len=%d", err, len(got))
	}
}

// TestBundleCommits covers the optional reverse bundle: present only when the agent
// actually committed (§6.7).
func TestBundleCommits(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	if err := patch.CreateBundle(ctx, src, bundle, 1); err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}
	ws := filepath.Join(t.TempDir(), "workspace")
	if _, err := patch.Ingest(ctx, bundle, ws, patch.DefaultIdentity); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// No commit yet → no reverse bundle.
	out := filepath.Join(t.TempDir(), "commits.bundle")
	has, err := patch.BundleCommits(ctx, ws, patch.BaselineTag, out)
	if err != nil {
		t.Fatalf("BundleCommits (no commit): %v", err)
	}
	if has {
		t.Error("BundleCommits reported commits when the agent made none")
	}

	// Agent commits → reverse bundle is produced.
	writeFile(t, filepath.Join(ws, "a.txt"), "2\n")
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "--quiet", "-m", "agent change")
	has, err = patch.BundleCommits(ctx, ws, patch.BaselineTag, out)
	if err != nil {
		t.Fatalf("BundleCommits (commit): %v", err)
	}
	if !has {
		t.Fatal("BundleCommits reported no commits after a commit")
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Fatalf("commits bundle missing/empty: err=%v", err)
	}
}

// --- helpers ---

// newRepo creates a git repo on branch main with the given files in one commit, returning
// its path. Local identity is set so commits succeed in a clean test environment.
func newRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "--quiet", "-b", "main")
	git(t, dir, "config", "user.name", "tester")
	git(t, dir, "config", "user.email", "tester@example.com")
	for name, content := range files {
		writeFile(t, filepath.Join(dir, name), content)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", "initial")
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, errb.String())
	}
	return trim(out.String())
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
