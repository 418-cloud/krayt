// Package patch implements the git-bundle code transfer and patch generation of §6.7.
// Everything here is OS-agnostic: it shells out to the `git` binary (no git library is
// pinned in §9.1), so the same code runs on the host (bundle create, apply) and inside
// the guest (verify, clone, diff). The guest-side helpers also back the in-process
// fakeProvider round-trip in unit tests, so the patch logic is exercised without a VM
// (§14 test strategy).
//
// Direction matters (§6.7): the forward host→guest bundle must be self-contained (no
// prerequisites) so it clones into an empty repo; the optional reverse guest→host bundle
// is a range bundle, which is correct because the host already has the baseline.
package patch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// BaselineTag is the tag the guest puts on the imported HEAD before the agent runs; the
// final diff is computed against it, not HEAD~1 (§6.7).
const BaselineTag = "krayt-baseline"

// Identity is the git author/committer the guest configures before any commit. A bot
// identity is used so no host git identity ever runs in the VM (§6.7, §10).
type Identity struct {
	Name  string
	Email string
}

// DefaultIdentity is the krayt bot identity used in the guest workspace.
var DefaultIdentity = Identity{Name: "krayt", Email: "krayt@418.cloud"}

// CreateBundle writes a **self-contained** git bundle of repoPath to outBundle — one the guest can
// `git clone` with no prerequisites (§6.7). `depth` selects the shape:
//   - depth <= 0: full history — a full clone bundled as-is (real commit SHAs, all reachable
//     objects included).
//   - depth >= 1 (default): a single-commit **snapshot** of the current state — a *parentless* root
//     commit whose tree is HEAD's (plus dirty, if requested), bundled alone.
//
// The snapshot exists because a shallow clone cannot be bundled self-contained: `git bundle create`
// does not record the shallow boundary, so bundling a shallow clone references parents it omits and
// the guest clone fails ("remote did not send all necessary objects"). A parentless snapshot has no
// such boundary. Because a snapshot's baseline is synthetic, the optional reverse commits.bundle
// (§6.7) isn't host-fetchable for a snapshot — use depth 0 if you need faithful multi-commit
// application; changes.patch (the primary deliverable) is unaffected either way.
//
// When includeDirty is set, uncommitted changes are folded into the tip's tree without mutating the
// user's repo: the working tree is captured via a temporary index, writing objects into our own
// clone (not the source), leaving the user's index/worktree/refs untouched.
//
// BundleResult reports both the real HEAD and the commit actually bundled, so a run can record its
// provenance (§8.4): the two coincide only in the full-history/no-dirty case; every other shape
// bundles a synthetic tip, so the caller must not conflate them.
func CreateBundle(ctx context.Context, repoPath, outBundle string, depth int, includeDirty bool) (BundleResult, error) {
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return BundleResult{}, fmt.Errorf("patch: resolve repo path: %w", err)
	}
	hasCommits := gitOK(ctx, absRepo, "rev-parse", "--verify", "HEAD")
	if !hasCommits && !includeDirty {
		return BundleResult{}, fmt.Errorf("patch: repo %s has no commits to bundle (unborn HEAD)", absRepo)
	}

	tmp, err := os.MkdirTemp("", "krayt-bundle-src-")
	if err != nil {
		return BundleResult{}, fmt.Errorf("patch: temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	src := filepath.Join(tmp, "src")

	// depth >= 1 → a self-contained single-commit snapshot; depth <= 0 → full history.
	snapshot := depth >= 1

	var branch string
	if hasCommits {
		cloneArgs := []string{"clone", "--quiet"}
		if snapshot {
			cloneArgs = append(cloneArgs, "--depth", "1") // only the tip's tree is needed to snapshot it
		}
		cloneArgs = append(cloneArgs, "file://"+absRepo, src)
		if _, err := runGit(ctx, "", cloneArgs...); err != nil {
			return BundleResult{}, fmt.Errorf("patch: clone source: %w", err)
		}
		if branch, err = currentBranch(ctx, src); err != nil {
			return BundleResult{}, err
		}
	} else {
		// Unborn HEAD + includeDirty: nothing to clone, so start a fresh empty repo we own
		// and capture the working tree as a root commit.
		if _, err := runGit(ctx, "", "init", "--quiet", "-b", "main", src); err != nil {
			return BundleResult{}, fmt.Errorf("patch: init empty bundle repo: %w", err)
		}
		branch = "main"
	}

	// The real, permanent HEAD (empty for an unborn-HEAD repo). A depth-1 clone still records the
	// tip's true SHA, so this is the checkoutable commit the run was based on regardless of shape.
	// Resolved once here so every switch branch reports it, even the ones whose bundled tip differs.
	var headSHA string
	if hasCommits {
		h, err := runGit(ctx, src, "rev-parse", "HEAD")
		if err != nil {
			return BundleResult{}, fmt.Errorf("patch: resolve HEAD: %w", err)
		}
		headSHA = strings.TrimSpace(h)
	}

	gitDir := filepath.Join(src, ".git")

	// Build the tip commit the bundle checks out. It must be self-contained so the guest clones
	// cleanly: a snapshot tip is parentless; a full-history tip keeps the real chain.
	var tip string
	switch {
	case includeDirty:
		tree, err := captureWorkTree(ctx, src, absRepo, hasCommits, filepath.Join(tmp, "idx"))
		if err != nil {
			return BundleResult{}, err
		}
		var parents []string
		if hasCommits && !snapshot { // full history keeps the real parent; a snapshot is rootless
			parents = []string{headSHA}
		}
		if tip, err = commitTree(ctx, gitDir, tree, "krayt: include uncommitted changes", parents...); err != nil {
			return BundleResult{}, err
		}
	case snapshot:
		// Snapshot committed HEAD's tree as a parentless root commit (hasCommits holds here).
		tree, err := runGit(ctx, src, "rev-parse", "HEAD^{tree}")
		if err != nil {
			return BundleResult{}, fmt.Errorf("patch: resolve HEAD tree: %w", err)
		}
		if tip, err = commitTree(ctx, gitDir, strings.TrimSpace(tree), "krayt: workspace snapshot"); err != nil {
			return BundleResult{}, err
		}
	default:
		// Full history, no dirty: the real HEAD is already self-contained; bundle it as-is.
	}

	if tip != "" {
		// HEAD is a symbolic ref to the branch, so moving the branch moves what HEAD resolves to.
		if _, err := runGit(ctx, src, "update-ref", "refs/heads/"+branch, tip); err != nil {
			return BundleResult{}, fmt.Errorf("patch: point branch at bundle tip: %w", err)
		}
	}

	// Name a ref (the branch) plus HEAD so `git clone` of the bundle has something to check out (§6.7).
	if _, err := runGit(ctx, src, "bundle", "create", outBundle, "HEAD", branch); err != nil {
		return BundleResult{}, fmt.Errorf("patch: create bundle: %w", err)
	}
	// The bundled tip is the synthetic snapshot/dirty commit when one was built; only the full
	// history/no-dirty case leaves tip empty, and there the real HEAD is exactly what was bundled.
	bundleSHA := tip
	if bundleSHA == "" {
		bundleSHA = headSHA
	}
	return BundleResult{HeadSHA: headSHA, BundleSHA: bundleSHA}, nil
}

// BundleResult reports the two distinct commits a CreateBundle call is defined by (§8.4 provenance):
//   - HeadSHA is the real `git rev-parse HEAD` at bundle time ("" for an unborn HEAD) — permanent
//     and checkoutable, answering "what named commit was this run based on".
//   - BundleSHA is the commit actually imported as the guest's krayt-baseline and diffed against for
//     changes.patch. It equals HeadSHA only in the full-history/no-dirty case; every other shape
//     bundles a synthetic commit not reachable from any of the user's branches.
type BundleResult struct {
	HeadSHA   string // "" if unborn HEAD
	BundleSHA string // always set on success — the tip actually bundled
}

// captureWorkTree writes a tree of the source working tree (committed state + uncommitted,
// .gitignore honored) into cloneDir's object database, without mutating the source repo (§6.7): it
// reads files from srcWorkTree through GIT_INDEX_FILE while writing objects into cloneDir, so the
// user's index/worktree/refs stay untouched. Returns the tree SHA.
func captureWorkTree(ctx context.Context, cloneDir, srcWorkTree string, hasCommits bool, indexFile string) (string, error) {
	env := []string{
		"GIT_DIR=" + filepath.Join(cloneDir, ".git"),
		"GIT_WORK_TREE=" + srcWorkTree,
		"GIT_INDEX_FILE=" + indexFile,
	}
	if hasCommits {
		if _, err := runGitEnv(ctx, env, "read-tree", "HEAD"); err != nil {
			return "", fmt.Errorf("patch: seed temp index: %w", err)
		}
	}
	if _, err := runGitEnv(ctx, env, "add", "-A"); err != nil {
		return "", fmt.Errorf("patch: stage working tree: %w", err)
	}
	tree, err := runGitEnv(ctx, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("patch: write tree: %w", err)
	}
	return strings.TrimSpace(tree), nil
}

// commitTree makes a commit object for tree with the given parents (none = a root commit), using
// the krayt bot identity via env so a config-less host/container still commits (§6.7).
func commitTree(ctx context.Context, gitDir, tree, message string, parents ...string) (string, error) {
	args := []string{"commit-tree", tree, "-m", message}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	env := []string{
		"GIT_DIR=" + gitDir,
		"GIT_AUTHOR_NAME=" + DefaultIdentity.Name, "GIT_AUTHOR_EMAIL=" + DefaultIdentity.Email,
		"GIT_COMMITTER_NAME=" + DefaultIdentity.Name, "GIT_COMMITTER_EMAIL=" + DefaultIdentity.Email,
	}
	out, err := runGitEnv(ctx, env, args...)
	if err != nil {
		return "", fmt.Errorf("patch: commit-tree: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// Ingest performs the guest-side bundle ingest (§6.7), in order: verify the bundle (catch
// truncation/corruption and unexpected prerequisites early), clone it into workspaceDir,
// configure the bot identity before any commit, record + tag the baseline (the imported
// HEAD), and drop the origin remote (it points at the now-removed temp bundle). It returns
// the recorded baseline commit. The bundle must already be a file on disk: you cannot
// `git clone` from a pipe (§6.7).
func Ingest(ctx context.Context, bundlePath, workspaceDir string, id Identity) (baseline string, err error) {
	if err := verifyBundle(ctx, bundlePath); err != nil {
		return "", err
	}
	if _, err := runGit(ctx, "", "clone", "--quiet", bundlePath, workspaceDir); err != nil {
		return "", fmt.Errorf("patch: clone bundle: %w", err)
	}
	if _, err := runGit(ctx, workspaceDir, "config", "user.name", id.Name); err != nil {
		return "", fmt.Errorf("patch: set user.name: %w", err)
	}
	if _, err := runGit(ctx, workspaceDir, "config", "user.email", id.Email); err != nil {
		return "", fmt.Errorf("patch: set user.email: %w", err)
	}
	rev, err := runGit(ctx, workspaceDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("patch: record baseline: %w", err)
	}
	baseline = strings.TrimSpace(rev)
	if _, err := runGit(ctx, workspaceDir, "tag", BaselineTag); err != nil {
		return "", fmt.Errorf("patch: tag baseline: %w", err)
	}
	// origin points at the now-deleted temp bundle file; drop it so later git ops never
	// try to reach it. Missing origin is not an error worth failing the run over.
	_, _ = runGit(ctx, workspaceDir, "remote", "remove", "origin")
	return baseline, nil
}

// SetupPatchGit snapshots the pristine, root-only git dir used for guest patch generation
// (§6.7, §10 finding #2). It copies workspaceDir/.git — as freshly cloned by Ingest, before
// the container ever runs — into patchGitDir, which the guest keeps OUTSIDE the workspace,
// never bind-mounts into the container, and never makes container-writable. Diff/BundleCommits
// then resolve the baseline and run git against THIS dir (with the workspace as a detached
// work tree), so a container that later rewrites the workspace's `.git/config`, `.git/hooks/*`,
// or `.gitattributes` can never get the root guest-agent's git to trust or execute it. The copy
// is pristine because `git clone` generates the config/hooks itself — none of it comes from the
// untrusted source repo, and the container has not run yet. patchGitDir must not already exist:
// this is asserted explicitly (rather than left as a caller invariant) because copyTree only
// overwrites paths present in the source — a stale pre-existing patchGitDir (e.g. a future
// refactor that reuses a guest root across runs) could silently leave old hooks/config behind,
// reintroducing the exact trust problem this snapshot exists to prevent.
func SetupPatchGit(workspaceDir, patchGitDir string) error {
	srcGit := filepath.Join(workspaceDir, ".git")
	if info, err := os.Stat(srcGit); err != nil {
		return fmt.Errorf("patch: snapshot patchgit: source %s: %w", srcGit, err)
	} else if !info.IsDir() {
		return fmt.Errorf("patch: snapshot patchgit: source %s is not a directory", srcGit)
	}
	if _, err := os.Lstat(patchGitDir); err == nil {
		return fmt.Errorf("patch: snapshot patchgit: %s already exists", patchGitDir)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("patch: snapshot patchgit: stat %s: %w", patchGitDir, err)
	}
	if err := copyTree(srcGit, patchGitDir); err != nil {
		return fmt.Errorf("patch: snapshot patchgit: %w", err)
	}
	return nil
}

// Diff produces changes.patch: everything in the workspace working tree since the baseline,
// whether the agent committed or only edited the working tree. It runs entirely against the
// root-only patchGitDir (§6.7, §10 finding #2) — the container-writable `workspace/.git` is
// never trusted — with the workspace as a detached GIT_WORK_TREE. The baseline is resolved and
// the index seeded from patchGitDir, so a container that rewrote refs/tags in `workspace/.git`
// cannot change what we diff against. We seed the index from the baseline, stage all working-tree
// changes, then diff the index against the baseline, so an agent that edits a file without
// committing — the common case — still yields a non-empty patch (broader than §6.7's
// `git diff baseline..HEAD`, which would miss uncommitted edits). `--no-textconv` plus the
// force-cleared knobs (patchGenGitArgs) neutralize any diff-driver/fsmonitor/hook execution.
func Diff(ctx context.Context, patchGitDir, workspaceDir, baselineRef string) ([]byte, error) {
	env := patchGenEnv(patchGitDir, workspaceDir)
	if _, err := runGitEnv(ctx, env, patchGenGitArgs("read-tree", baselineRef)...); err != nil {
		return nil, fmt.Errorf("patch: seed baseline index: %w", err)
	}
	if _, err := runGitEnv(ctx, env, patchGenGitArgs("add", "-A")...); err != nil {
		return nil, fmt.Errorf("patch: stage changes: %w", err)
	}
	out, err := runGitRawEnv(ctx, env, patchGenGitArgs("diff", "--cached", "--binary", "--no-textconv", baselineRef)...)
	if err != nil {
		return nil, fmt.Errorf("patch: diff vs baseline: %w", err)
	}
	return out, nil
}

// BundleCommits writes the optional reverse range bundle of the agent's new commits
// (baseline..HEAD) to outBundle, so multi-commit work applies faithfully on the host via
// `git fetch` (§6.7). It returns false (and writes nothing) when the agent made no commits —
// HEAD still equals the baseline — in which case changes.patch is the only artifact.
//
// The baseline is resolved from the root-only patchGitDir (so a container that moved the
// workspace's `krayt-baseline` tag can't skew the range), while HEAD and the objects come from
// the container-writable `workspace/.git` (untrusted, but `git bundle create` runs no hooks,
// fsmonitor, or textconv — and patchGenGitArgs force-clears those knobs regardless). The
// resolved baseline SHA still exists in `workspace/.git` because it is an ancestor of the
// agent's HEAD. commits.bundle stays best-effort; a corrupt workspace `.git` only fails this
// optional artifact, never the security-critical changes.patch (which never trusts it).
func BundleCommits(ctx context.Context, patchGitDir, workspaceDir, baselineRef, outBundle string) (bool, error) {
	base, err := runGitEnv(ctx, patchGenEnv(patchGitDir, workspaceDir), patchGenGitArgs("rev-parse", baselineRef)...)
	if err != nil {
		return false, fmt.Errorf("patch: rev-parse baseline: %w", err)
	}
	wsEnv := patchGenEnv(filepath.Join(workspaceDir, ".git"), workspaceDir)
	head, err := runGitEnv(ctx, wsEnv, patchGenGitArgs("rev-parse", "HEAD")...)
	if err != nil {
		return false, fmt.Errorf("patch: rev-parse HEAD: %w", err)
	}
	baseSHA, headSHA := strings.TrimSpace(base), strings.TrimSpace(head)
	if headSHA == baseSHA {
		return false, nil // no new commits; nothing to bundle
	}
	// `git bundle create` needs a NAMED ref on the positive side to advertise (a bare SHA yields
	// "Refusing to create empty bundle"), so pass `HEAD`; the negative boundary is the baseline
	// SHA resolved from the root-only patchgit, so a container that moved the workspace tag can't
	// widen or empty the range.
	if _, err := runGitEnv(ctx, wsEnv, patchGenGitArgs("bundle", "create", outBundle, baseSHA+"..HEAD")...); err != nil {
		return false, fmt.Errorf("patch: create commits bundle: %w", err)
	}
	return true, nil
}

// Apply applies a changes.patch onto repoPath with `git apply` (optionally --3way), the
// host-side helper behind `krayt apply` (§6.7). The human reviews the diff first; nothing
// auto-applies.
func Apply(ctx context.Context, repoPath, patchPath string, threeWay bool) error {
	args := []string{"apply"}
	if threeWay {
		args = append(args, "--3way")
	}
	args = append(args, patchPath)
	if _, err := runGit(ctx, repoPath, args...); err != nil {
		return fmt.Errorf("patch: git apply: %w", err)
	}
	return nil
}

// Stats are the diffstat of a changes.patch, for meta.json / report.md (§8.4).
type Stats struct {
	Path         string // artifact-relative name, e.g. "changes.patch"
	FilesChanged int
	Insertions   int
	Deletions    int
}

// Stat computes the diffstat of a patch file with `git apply --numstat`, which only parses
// the diff (no repo, no working tree touched), so it is safe to run on the host against a
// collected changes.patch. An empty patch reports zero changes; binary hunks ("-\t-") count
// the file but add no line counts.
func Stat(ctx context.Context, patchPath string) (Stats, error) {
	st := Stats{Path: filepath.Base(patchPath)}
	info, err := os.Stat(patchPath)
	if err != nil {
		return st, fmt.Errorf("patch: stat %s: %w", patchPath, err)
	}
	if info.Size() == 0 {
		return st, nil // no diff → zero stats
	}
	abs, err := filepath.Abs(patchPath)
	if err != nil {
		return st, err
	}
	// Run from a throwaway non-repo dir: inside a work tree `git apply --numstat` prepends the
	// cwd's path prefix and matches nothing when invoked from a subdirectory, so parse the diff
	// in isolation. --numstat only reads the patch; no tree is touched.
	tmp, err := os.MkdirTemp("", "krayt-stat-")
	if err != nil {
		return st, fmt.Errorf("patch: stat temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	out, err := runGit(ctx, tmp, "apply", "--numstat", abs)
	if err != nil {
		return st, fmt.Errorf("patch: numstat: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 3)
		if len(f) < 3 {
			continue
		}
		st.FilesChanged++
		if n, err := strconv.Atoi(f[0]); err == nil { // "-" (binary) → skip the count
			st.Insertions += n
		}
		if n, err := strconv.Atoi(f[1]); err == nil {
			st.Deletions += n
		}
	}
	return st, nil
}

// verifyBundle runs `git bundle verify` to catch truncation/corruption and unexpected
// prerequisites before cloning (§6.7). `git bundle verify` is a repository command — it
// fails with "need a repository to verify a bundle" if run outside one — so we run it from
// a throwaway bare repo rather than the guest-agent's cwd (which is not a repo). A
// self-contained forward bundle has no prerequisites, so an empty repo satisfies the check.
func verifyBundle(ctx context.Context, bundlePath string) error {
	tmp, err := os.MkdirTemp("", "krayt-bundle-verify-")
	if err != nil {
		return fmt.Errorf("patch: verify temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if _, err := runGit(ctx, "", "init", "--quiet", "--bare", tmp); err != nil {
		return fmt.Errorf("patch: init verify repo: %w", err)
	}
	if _, err := runGit(ctx, tmp, "bundle", "verify", bundlePath); err != nil {
		return fmt.Errorf("patch: bundle verify: %w", err)
	}
	return nil
}

// gitOK reports whether a git command succeeds in dir (used as a predicate, e.g. for an
// unborn-HEAD check).
func gitOK(ctx context.Context, dir string, args ...string) bool {
	_, err := runGit(ctx, dir, args...)
	return err == nil
}

// currentBranch returns the checked-out branch name of repoPath. A fresh `git clone`
// always checks out the default branch, so this is well-defined for the shallow source.
func currentBranch(ctx context.Context, repoPath string) (string, error) {
	out, err := runGit(ctx, repoPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("patch: resolve current branch: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// runGit runs git in dir (cwd if empty) and returns its raw stdout (callers trim as needed),
// wrapping failures with stderr so a broken git invocation is diagnosable.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := runGitRaw(ctx, dir, args...)
	return string(out), err
}

// runGitRaw is runGit without trimming, for commands whose stdout is binary (diff/bundle).
func runGitRaw(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Keep git non-interactive and independent of the user's global/system config so a
	// stray credential helper or hook config can't perturb the sandboxed run.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}

// runGitEnv runs git with extraEnv appended (e.g. GIT_DIR / GIT_WORK_TREE / GIT_INDEX_FILE
// for the non-mutating dirty capture) and returns its raw stdout (callers trim as needed).
// No working directory is set; the location is controlled entirely by the git env vars.
func runGitEnv(ctx context.Context, extraEnv []string, args ...string) (string, error) {
	out, err := runGitRawEnv(ctx, extraEnv, args...)
	return string(out), err
}

// runGitRawEnv is runGitEnv without trimming, for commands whose stdout is binary (diff/bundle).
func runGitRawEnv(ctx context.Context, extraEnv []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	cmd.Env = append(cmd.Env, extraEnv...)
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}

// patchGenGitArgs prepends the config knobs that are force-cleared on EVERY guest
// patch-generation git invocation so no repo-local config can execute code as root (§6.7,
// §10 finding #2): `core.fsmonitor=` disables the fsmonitor program run on index refresh and
// `core.hooksPath=/dev/null` disables all hooks. A command-line `-c` beats any value a
// container-written `.git/config` might carry. A fresh slice is returned each call so callers
// can append safely.
func patchGenGitArgs(extra ...string) []string {
	return append([]string{"-c", "core.fsmonitor=", "-c", "core.hooksPath=/dev/null"}, extra...)
}

// patchGenEnv isolates a patch-generation git run to the root-only gitDir with workTree as a
// detached work tree, and neutralizes every out-of-repo config/attribute source (§6.7, §10):
// GIT_CONFIG_GLOBAL=/dev/null drops global config and GIT_ATTR_NOSYSTEM=1 drops system
// attributes (runGitRawEnv already sets GIT_CONFIG_NOSYSTEM=1). Only the pristine patchgit
// config — which the container never touched — is honored.
func patchGenEnv(gitDir, workTree string) []string {
	return []string{
		"GIT_DIR=" + gitDir,
		"GIT_WORK_TREE=" + workTree,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ATTR_NOSYSTEM=1",
	}
}

// copyTree recursively copies src to dst, creating dirs 0700 and files 0600 (root-only), and
// skipping symlinks/special files. A freshly-cloned `.git` contains only regular files and
// dirs, so nothing worth copying is skipped; refusing symlinks keeps the copy from following a
// link out of the tree. dst must not already exist as a populated tree.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case d.Type().IsRegular():
			return copyFile(p, target)
		default:
			return nil
		}
	})
}

// copyFile copies a single regular file to dst, 0600.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
