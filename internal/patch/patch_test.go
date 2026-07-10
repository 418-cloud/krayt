package patch_test

import (
	"bytes"
	"context"
	"fmt"
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
	if err := patch.CreateBundle(ctx, src, bundle, 1, false); err != nil {
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

	// Snapshot the root-only patchgit as the guest does, before the agent touches the tree.
	pg := filepath.Join(t.TempDir(), "patchgit")
	if err := patch.SetupPatchGit(ws, pg); err != nil {
		t.Fatalf("SetupPatchGit: %v", err)
	}

	// Agent edits one tracked file and adds a new one — without committing.
	writeFile(t, filepath.Join(ws, "greeting.txt"), "hello world\n")
	writeFile(t, filepath.Join(ws, "new.txt"), "fresh\n")

	patchBytes, err := patch.Diff(ctx, pg, ws, patch.BaselineTag)
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
	if err := patch.CreateBundle(ctx, src, bundle, 1, false); err != nil {
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
	pg := filepath.Join(t.TempDir(), "patchgit")
	if err := patch.SetupPatchGit(ws, pg); err != nil {
		t.Fatalf("SetupPatchGit: %v", err)
	}
	writeFile(t, filepath.Join(ws, "a.txt"), "2\n")
	if got, err := patch.Diff(ctx, pg, ws, patch.BaselineTag); err != nil || len(got) == 0 {
		t.Fatalf("Diff from non-repo cwd: err=%v len=%d", err, len(got))
	}
}

// TestBundleCommits covers the optional reverse bundle: present only when the agent
// actually committed (§6.7).
func TestBundleCommits(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	if err := patch.CreateBundle(ctx, src, bundle, 1, false); err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}
	ws := filepath.Join(t.TempDir(), "workspace")
	if _, err := patch.Ingest(ctx, bundle, ws, patch.DefaultIdentity); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	pg := filepath.Join(t.TempDir(), "patchgit")
	if err := patch.SetupPatchGit(ws, pg); err != nil {
		t.Fatalf("SetupPatchGit: %v", err)
	}

	// No commit yet → no reverse bundle.
	out := filepath.Join(t.TempDir(), "commits.bundle")
	has, err := patch.BundleCommits(ctx, pg, ws, patch.BaselineTag, out)
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
	has, err = patch.BundleCommits(ctx, pg, ws, patch.BaselineTag, out)
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

// TestCreateBundleIncludeDirty captures uncommitted changes (modified, untracked, with
// .gitignore honored) into the bundle baseline without mutating the user's repo (§6.7).
func TestCreateBundleIncludeDirty(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t, map[string]string{
		"greeting.txt": "hello\n",
		".gitignore":   "ignored.txt\n",
	})
	writeFile(t, filepath.Join(src, "greeting.txt"), "hello dirty\n") // modified tracked
	writeFile(t, filepath.Join(src, "new.txt"), "fresh\n")            // untracked
	writeFile(t, filepath.Join(src, "ignored.txt"), "junk\n")         // gitignored

	headBefore := git(t, src, "rev-parse", "HEAD")
	refsBefore := git(t, src, "for-each-ref")
	statusBefore := git(t, src, "status", "--porcelain")

	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	if err := patch.CreateBundle(ctx, src, bundle, 1, true); err != nil {
		t.Fatalf("CreateBundle includeDirty: %v", err)
	}
	ws := filepath.Join(t.TempDir(), "ws")
	if _, err := patch.Ingest(ctx, bundle, ws, patch.DefaultIdentity); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if got := readFile(t, filepath.Join(ws, "greeting.txt")); got != "hello dirty\n" {
		t.Errorf("greeting.txt = %q, want the uncommitted edit", got)
	}
	if got := readFile(t, filepath.Join(ws, "new.txt")); got != "fresh\n" {
		t.Errorf("untracked new.txt = %q, want captured", got)
	}
	if _, err := os.Stat(filepath.Join(ws, "ignored.txt")); !os.IsNotExist(err) {
		t.Error("gitignored file leaked into the bundle")
	}

	// Non-mutating: the user's repo is untouched.
	if git(t, src, "rev-parse", "HEAD") != headBefore {
		t.Error("source HEAD changed")
	}
	if git(t, src, "for-each-ref") != refsBefore {
		t.Error("source refs changed")
	}
	if git(t, src, "status", "--porcelain") != statusBefore {
		t.Error("source index/worktree changed")
	}
}

// TestCreateBundleNoDirtyIgnoresUncommitted confirms the default (clean) bundle carries
// only committed state.
func TestCreateBundleNoDirtyIgnoresUncommitted(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})
	writeFile(t, filepath.Join(src, "greeting.txt"), "hello dirty\n")
	writeFile(t, filepath.Join(src, "new.txt"), "fresh\n")

	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	if err := patch.CreateBundle(ctx, src, bundle, 1, false); err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}
	ws := filepath.Join(t.TempDir(), "ws")
	if _, err := patch.Ingest(ctx, bundle, ws, patch.DefaultIdentity); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if got := readFile(t, filepath.Join(ws, "greeting.txt")); got != "hello\n" {
		t.Errorf("greeting.txt = %q, want committed state", got)
	}
	if _, err := os.Stat(filepath.Join(ws, "new.txt")); !os.IsNotExist(err) {
		t.Error("uncommitted file present in a clean bundle")
	}
}

// TestCreateBundleIncludeDirtyUnbornHead handles a repo with uncommitted files but no
// commits yet: the working tree becomes a root commit (§6.7).
func TestCreateBundleIncludeDirtyUnbornHead(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	git(t, dir, "init", "--quiet", "-b", "main")
	git(t, dir, "config", "user.name", "tester")
	git(t, dir, "config", "user.email", "tester@example.com")
	writeFile(t, filepath.Join(dir, "a.txt"), "content\n")

	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	if err := patch.CreateBundle(ctx, dir, bundle, 1, true); err != nil {
		t.Fatalf("CreateBundle includeDirty unborn: %v", err)
	}
	ws := filepath.Join(t.TempDir(), "ws")
	if _, err := patch.Ingest(ctx, bundle, ws, patch.DefaultIdentity); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if got := readFile(t, filepath.Join(ws, "a.txt")); got != "content\n" {
		t.Errorf("a.txt = %q, want 'content'", got)
	}
}

// TestRoundTripMultiCommitMerge is the regression for the shallow-bundle bug: a repo whose HEAD is
// a *merge commit* on top of real history. The old shallow-clone-then-bundle produced a bundle that
// referenced HEAD's parents without including them, so the guest clone failed with "remote did not
// send all necessary objects". The single-commit repos in the other tests hid it. Both the snapshot
// (depth 1) and full-history (depth 0) shapes must ingest cleanly and round-trip a diff.
func TestRoundTripMultiCommitMerge(t *testing.T) {
	ctx := context.Background()
	src := newRepoWithHistory(t)

	for _, depth := range []int{1, 0} {
		t.Run(fmt.Sprintf("depth=%d", depth), func(t *testing.T) {
			bundle := filepath.Join(t.TempDir(), "repo.bundle")
			if err := patch.CreateBundle(ctx, src, bundle, depth, false); err != nil {
				t.Fatalf("CreateBundle: %v", err)
			}
			ws := filepath.Join(t.TempDir(), "ws")
			baseline, err := patch.Ingest(ctx, bundle, ws, patch.DefaultIdentity)
			if err != nil {
				t.Fatalf("Ingest (failed before the self-contained-bundle fix): %v", err)
			}
			if baseline == "" {
				t.Fatal("empty baseline")
			}
			// The merged HEAD tree is present in the workspace.
			if got := readFile(t, filepath.Join(ws, "feature.txt")); got != "feature\n" {
				t.Errorf("feature.txt = %q, want merged HEAD tree", got)
			}

			pg := filepath.Join(t.TempDir(), "patchgit")
			if err := patch.SetupPatchGit(ws, pg); err != nil {
				t.Fatalf("SetupPatchGit: %v", err)
			}
			// Edit → diff → apply onto a fresh checkout of the source.
			writeFile(t, filepath.Join(ws, "base.txt"), "edited\n")
			patchBytes, err := patch.Diff(ctx, pg, ws, patch.BaselineTag)
			if err != nil || len(patchBytes) == 0 {
				t.Fatalf("Diff: err=%v len=%d", err, len(patchBytes))
			}
			target := filepath.Join(t.TempDir(), "target")
			if out, err := exec.Command("git", "clone", "--quiet", src, target).CombinedOutput(); err != nil {
				t.Fatalf("clone target: %v\n%s", err, out)
			}
			patchFile := filepath.Join(t.TempDir(), "changes.patch")
			writeFile(t, patchFile, string(patchBytes))
			if err := patch.Apply(ctx, target, patchFile, false); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if got := readFile(t, filepath.Join(target, "base.txt")); got != "edited\n" {
				t.Errorf("base.txt after apply = %q, want 'edited'", got)
			}
		})
	}
}

// TestCreateBundleMultiCommitIncludeDirty pairs the multi-commit/merge history with uncommitted
// changes: the snapshot must fold the dirty edit in and still clone cleanly.
func TestCreateBundleMultiCommitIncludeDirty(t *testing.T) {
	ctx := context.Background()
	src := newRepoWithHistory(t)
	writeFile(t, filepath.Join(src, "base.txt"), "base dirty\n") // uncommitted edit on a merge HEAD

	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	if err := patch.CreateBundle(ctx, src, bundle, 1, true); err != nil {
		t.Fatalf("CreateBundle includeDirty: %v", err)
	}
	ws := filepath.Join(t.TempDir(), "ws")
	if _, err := patch.Ingest(ctx, bundle, ws, patch.DefaultIdentity); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if got := readFile(t, filepath.Join(ws, "base.txt")); got != "base dirty\n" {
		t.Errorf("base.txt = %q, want the uncommitted edit", got)
	}
	if got := readFile(t, filepath.Join(ws, "feature.txt")); got != "feature\n" {
		t.Errorf("feature.txt = %q, want committed merge state", got)
	}
}

// TestDiffConfigInjectionInert is the regression for §10 finding #2 (container→guest-root
// escape). A container with a writable `workspace/.git` writes a malicious `.git/config`
// (`core.fsmonitor` → a script that drops a sentinel) and a malicious `.gitattributes` +
// `[diff "evil"] textconv` driver, then the guest generates the patch. Because Diff runs against
// the root-only patchgit (snapshotted pristine) with fsmonitor/hooks force-cleared and
// --no-textconv, no attacker script runs and the diff is still correct for the real edit.
func TestDiffConfigInjectionInert(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})
	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	if err := patch.CreateBundle(ctx, src, bundle, 1, false); err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}
	ws := filepath.Join(t.TempDir(), "ws")
	if _, err := patch.Ingest(ctx, bundle, ws, patch.DefaultIdentity); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// Guest snapshots the pristine patchgit at ingest, before the container runs.
	pg := filepath.Join(t.TempDir(), "patchgit")
	if err := patch.SetupPatchGit(ws, pg); err != nil {
		t.Fatalf("SetupPatchGit: %v", err)
	}

	// Now the untrusted container writes attack payloads into the writable workspace .git.
	sentinel := filepath.Join(t.TempDir(), "pwned")
	pwn := filepath.Join(t.TempDir(), "pwn.sh")
	writeFile(t, pwn, "#!/bin/sh\ntouch "+sentinel+"\n")
	if err := os.Chmod(pwn, 0o755); err != nil {
		t.Fatalf("chmod pwn: %v", err)
	}
	// core.fsmonitor is invoked on index refresh (git add -A); [diff "evil"] textconv would run on
	// a diff of a file marked `diff=evil` by .gitattributes.
	appendFile(t, filepath.Join(ws, ".git", "config"),
		"[core]\n\tfsmonitor = "+pwn+"\n[diff \"evil\"]\n\ttextconv = "+pwn+"\n")
	writeFile(t, filepath.Join(ws, ".gitattributes"), "* diff=evil\n")

	// Agent makes a normal edit.
	writeFile(t, filepath.Join(ws, "greeting.txt"), "hello world\n")

	patchBytes, err := patch.Diff(ctx, pg, ws, patch.BaselineTag)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("attacker script executed: sentinel %s exists (err=%v)", sentinel, err)
	}
	// The real edit is still captured (plus the added .gitattributes), and the diff applies.
	if !bytes.Contains(patchBytes, []byte("hello world")) {
		t.Errorf("diff missing the real edit:\n%s", patchBytes)
	}
	target := filepath.Join(t.TempDir(), "target")
	if out, err := exec.Command("git", "clone", "--quiet", src, target).CombinedOutput(); err != nil {
		t.Fatalf("clone target: %v\n%s", err, out)
	}
	patchFile := filepath.Join(t.TempDir(), "changes.patch")
	writeFile(t, patchFile, string(patchBytes))
	if err := patch.Apply(ctx, target, patchFile, false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := readFile(t, filepath.Join(target, "greeting.txt")); got != "hello world\n" {
		t.Errorf("greeting.txt after apply = %q, want %q", got, "hello world\n")
	}
}

// TestDiffBaselineTamperInert asserts the baseline is resolved from the root-only patchgit, not
// the container-writable workspace .git: even after the container deletes/rewrites the
// `krayt-baseline` tag in the workspace, Diff still diffs against the true recorded baseline.
func TestDiffBaselineTamperInert(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})
	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	if err := patch.CreateBundle(ctx, src, bundle, 1, false); err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}
	ws := filepath.Join(t.TempDir(), "ws")
	if _, err := patch.Ingest(ctx, bundle, ws, patch.DefaultIdentity); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	pg := filepath.Join(t.TempDir(), "patchgit")
	if err := patch.SetupPatchGit(ws, pg); err != nil {
		t.Fatalf("SetupPatchGit: %v", err)
	}

	// Container edits a file, commits over the baseline, and moves/deletes the workspace tag so a
	// naive diff against `krayt-baseline` in the workspace would use the wrong (moved) baseline.
	writeFile(t, filepath.Join(ws, "greeting.txt"), "hello world\n")
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "--quiet", "-m", "agent commit")
	git(t, ws, "tag", "-f", patch.BaselineTag, "HEAD") // move the tag onto the new commit

	patchBytes, err := patch.Diff(ctx, pg, ws, patch.BaselineTag)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	// If the baseline had been read from the tampered workspace tag (now == HEAD's tree), the diff
	// would be empty. Resolved from patchgit, it still shows the real change.
	if !bytes.Contains(patchBytes, []byte("hello world")) {
		t.Errorf("baseline tamper skewed the diff (empty/wrong):\n%s", patchBytes)
	}
}

// --- helpers ---

// newRepoWithHistory builds a repo whose HEAD is a merge commit on top of several commits — the
// shape that broke the old shallow-clone-then-bundle (HEAD with parents whose objects the bundle
// omitted). Returns the repo path.
func newRepoWithHistory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "--quiet", "-b", "main")
	git(t, dir, "config", "user.name", "tester")
	git(t, dir, "config", "user.email", "tester@example.com")

	writeFile(t, filepath.Join(dir, "base.txt"), "base\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", "c1")
	writeFile(t, filepath.Join(dir, "base.txt"), "base2\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", "c2")

	// Diverge on a feature branch, then merge with --no-ff so HEAD is a real merge commit.
	git(t, dir, "checkout", "--quiet", "-b", "feature")
	writeFile(t, filepath.Join(dir, "feature.txt"), "feature\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", "feat")
	git(t, dir, "checkout", "--quiet", "main")
	git(t, dir, "merge", "--no-ff", "--quiet", "-m", "merge feature", "feature")
	return dir
}

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

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("append %s: %v", path, err)
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
