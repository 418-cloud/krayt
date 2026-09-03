//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/418-cloud/krayt/internal/patch"
)

func TestMainRunUsageErrors(t *testing.T) {
	cases := [][]string{
		nil,
		{"bogus"},
		{"setup"},               // missing all required flags
		{"finish"},              // missing all required flags
		{"setup", "--bundle=x"}, // missing the rest
	}
	for _, args := range cases {
		var out, errBuf bytes.Buffer
		if code := mainRun(args, &out, &errBuf); code != exitUsage {
			t.Errorf("mainRun(%v) = %d, want exitUsage(%d); stderr=%q", args, code, exitUsage, errBuf.String())
		}
		if errBuf.Len() == 0 {
			t.Errorf("mainRun(%v) produced no stderr message", args)
		}
	}
}

// TestRunSetupBaselineMatchesBundleTip is the Done-when: setup produces a krayt-baseline tag
// matching the bundle's tip.
func TestRunSetupBaselineMatchesBundleTip(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	bundleResult, err := patch.CreateBundle(ctx, src, bundle, 1, false)
	if err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}

	ws := filepath.Join(t.TempDir(), "workspace")
	pg := filepath.Join(t.TempDir(), "patchgit")
	res, err := runSetup(ctx, bundle, ws, pg, "agent")
	if err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	if res.Baseline != bundleResult.BundleSHA {
		t.Errorf("baseline = %q, want bundle tip %q", res.Baseline, bundleResult.BundleSHA)
	}
	if out := git(t, ws, "tag", "--list", patch.BaselineTag); out == "" {
		t.Errorf("baseline tag %q not created in workspace", patch.BaselineTag)
	}
	if res.AgentUser != "agent" {
		t.Errorf("AgentUser = %q, want %q", res.AgentUser, "agent")
	}
}

// TestRunSetupOrderingPatchGitUnaffectedByRelax is the Done-when ordering assertion:
// SetupPatchGit runs before the tree is relaxed, so the patch-git dir's mode is untouched by it.
func TestRunSetupOrderingPatchGitUnaffectedByRelax(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	if _, err := patch.CreateBundle(ctx, src, bundle, 1, false); err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}
	ws := filepath.Join(t.TempDir(), "workspace")
	pg := filepath.Join(t.TempDir(), "patchgit")
	if _, err := runSetup(ctx, bundle, ws, pg, "agent"); err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	pgInfo, err := os.Stat(filepath.Join(pg, "config"))
	if err != nil {
		t.Fatalf("stat patchgit config: %v", err)
	}
	if pgInfo.Mode().Perm() != 0o600 {
		t.Errorf("patchgit config mode = %v, want 0600 (untouched by the workspace relax)", pgInfo.Mode().Perm())
	}
	wsInfo, err := os.Stat(filepath.Join(ws, "a.txt"))
	if err != nil {
		t.Fatalf("stat workspace file: %v", err)
	}
	if wsInfo.Mode().Perm()&0o006 == 0 {
		t.Errorf("workspace file mode = %v, was not relaxed for the agent", wsInfo.Mode().Perm())
	}
}

// TestRunFinishGoldenMatchesDirectPatchCalls is the golden comparison against the pre-msb guest
// agent's own buildArtifacts (as it stood before run-tasks-on-microsandbox.md deleted it): the
// same Ingest/SetupPatchGit/Diff/BundleCommits sequence, driven once through the CLI and once by
// calling internal/patch directly, must produce a byte-identical changes.patch and an
// equivalent commits.bundle.
func TestRunFinishGoldenMatchesDirectPatchCalls(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})
	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	if _, err := patch.CreateBundle(ctx, src, bundle, 1, false); err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}

	ws := filepath.Join(t.TempDir(), "workspace")
	pg := filepath.Join(t.TempDir(), "patchgit")
	setupRes, err := runSetup(ctx, bundle, ws, pg, "agent")
	if err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	// Agent edits + commits, exercising both Diff (uncommitted-capable) and BundleCommits.
	writeFile(t, filepath.Join(ws, "greeting.txt"), "hello world\n")
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "--quiet", "-m", "agent change")

	wantDiff, err := patch.Diff(ctx, pg, ws, patch.BaselineTag)
	if err != nil {
		t.Fatalf("direct patch.Diff: %v", err)
	}
	wantBundle := filepath.Join(t.TempDir(), "want-commits.bundle")
	wantHas, err := patch.BundleCommits(ctx, pg, ws, patch.BaselineTag, wantBundle)
	if err != nil {
		t.Fatalf("direct patch.BundleCommits: %v", err)
	}
	if !wantHas {
		t.Fatal("direct BundleCommits reported no commits after a commit")
	}

	out := filepath.Join(t.TempDir(), "out")
	finishRes, err := runFinish(ctx, ws, pg, setupRes.Baseline, out)
	if err != nil {
		t.Fatalf("runFinish: %v", err)
	}

	gotDiff := readFileBytes(t, filepath.Join(out, fileChangesPatch))
	if !bytes.Equal(gotDiff, wantDiff) {
		t.Errorf("changes.patch differs from the direct patch.Diff call:\ngot:\n%s\nwant:\n%s", gotDiff, wantDiff)
	}
	if finishRes.DiffBytes != len(wantDiff) {
		t.Errorf("DiffBytes = %d, want %d", finishRes.DiffBytes, len(wantDiff))
	}
	if !finishRes.CommitsBundle {
		t.Error("finish reported no commits bundle after a commit")
	}
	// Bundles of the same object set aren't guaranteed byte-identical (pack encoding is not a
	// stable contract), so compare the head each resolves to rather than raw bytes.
	if got, want := bundleHead(t, filepath.Join(out, fileCommitsBundle)), bundleHead(t, wantBundle); got != want {
		t.Errorf("commits.bundle head = %q, want %q", got, want)
	}
}

// TestRunFinishWritesOnlyToOutAndPatchGit is the Done-when: finish writes nothing outside --out
// and the patch-git dir. The workspace is read-only from finish's perspective (Diff/BundleCommits
// only stage into the patch-git index and read the workspace tree/objects).
func TestRunFinishWritesOnlyToOutAndPatchGit(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	if _, err := patch.CreateBundle(ctx, src, bundle, 1, false); err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}
	ws := filepath.Join(t.TempDir(), "workspace")
	pg := filepath.Join(t.TempDir(), "patchgit")
	setupRes, err := runSetup(ctx, bundle, ws, pg, "agent")
	if err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	writeFile(t, filepath.Join(ws, "a.txt"), "2\n")

	before := snapshotTree(t, ws)
	out := filepath.Join(t.TempDir(), "out")
	if _, err := runFinish(ctx, ws, pg, setupRes.Baseline, out); err != nil {
		t.Fatalf("runFinish: %v", err)
	}
	if after := snapshotTree(t, ws); before != after {
		t.Errorf("runFinish modified the workspace tree:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestMainRunSetupAndFinishJSON drives both subcommands through mainRun (full argv + JSON
// contract), not just the testable cores, so the flag wiring itself is covered.
func TestMainRunSetupAndFinishJSON(t *testing.T) {
	ctx := context.Background()
	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	bundle := filepath.Join(t.TempDir(), "repo.bundle")
	if _, err := patch.CreateBundle(ctx, src, bundle, 1, false); err != nil {
		t.Fatalf("CreateBundle: %v", err)
	}
	ws := filepath.Join(t.TempDir(), "workspace")
	pg := filepath.Join(t.TempDir(), "patchgit")

	var out, errBuf bytes.Buffer
	setupArgs := []string{"setup", "--bundle", bundle, "--workspace", ws, "--patch-git", pg, "--agent-user", "agent"}
	if code := mainRun(setupArgs, &out, &errBuf); code != exitOK {
		t.Fatalf("mainRun(setup) = %d, stderr=%s", code, errBuf.String())
	}
	var setupOut setupResult
	if err := json.Unmarshal(out.Bytes(), &setupOut); err != nil {
		t.Fatalf("unmarshal setup stdout %q: %v", out.String(), err)
	}
	if setupOut.Baseline == "" {
		t.Fatal("setup JSON missing baseline")
	}

	writeFile(t, filepath.Join(ws, "a.txt"), "2\n")

	outDir := filepath.Join(t.TempDir(), "out")
	out.Reset()
	errBuf.Reset()
	finishArgs := []string{"finish", "--workspace", ws, "--patch-git", pg, "--baseline", setupOut.Baseline, "--out", outDir}
	if code := mainRun(finishArgs, &out, &errBuf); code != exitOK {
		t.Fatalf("mainRun(finish) = %d, stderr=%s", code, errBuf.String())
	}
	var finishOut finishResult
	if err := json.Unmarshal(out.Bytes(), &finishOut); err != nil {
		t.Fatalf("unmarshal finish stdout %q: %v", out.String(), err)
	}
	if finishOut.DiffBytes == 0 {
		t.Error("finish JSON reports zero diff bytes for a real edit")
	}
	if finishOut.CommitsBundle {
		t.Error("finish JSON reports a commits bundle when the agent made no commit")
	}
}

func bundleHead(t *testing.T, bundlePath string) string {
	t.Helper()
	out, err := exec.Command("git", "bundle", "list-heads", bundlePath).CombinedOutput()
	if err != nil {
		t.Fatalf("git bundle list-heads %s: %v\n%s", bundlePath, err, out)
	}
	return strings.TrimSpace(string(out))
}

func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		fmt.Fprintf(&b, "%s %d %s\n", rel, info.Size(), info.ModTime())
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return b.String()
}

// newRepo creates a git repo on branch main with the given files in one commit, mirroring
// internal/patch's test helper of the same name.
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
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, errBuf.String())
	}
	return strings.TrimSpace(out.String())
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
