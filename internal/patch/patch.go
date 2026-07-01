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
	"os"
	"os/exec"
	"path/filepath"
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

// CreateBundle writes a self-contained git bundle of repoPath to outBundle, using the
// shallow-clone-then-bundle technique (§6.7): `git bundle create` has no --depth, so we
// shallow-clone the source over the file:// transport (which honors --depth, unlike a
// local-path clone) and bundle that shallow clone. The bundle inherits the shallow
// boundary, so it carries no prerequisites and clones cleanly into the empty guest.
// depth<=0 means full history (no --depth). (verify current)
//
// When includeDirty is set, uncommitted changes are folded in without mutating the user's
// repo: a throwaway commit is built from the source's working tree via a temporary index,
// writing objects into our own clone (not the source), and the clone's branch is moved to
// that commit so the bundle's HEAD imports the dirty state as the baseline (§6.7). The
// user's index, working tree, and refs are never touched.
func CreateBundle(ctx context.Context, repoPath, outBundle string, depth int, includeDirty bool) error {
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("patch: resolve repo path: %w", err)
	}
	hasCommits := gitOK(ctx, absRepo, "rev-parse", "--verify", "HEAD")
	if !hasCommits && !includeDirty {
		return fmt.Errorf("patch: repo %s has no commits to bundle (unborn HEAD)", absRepo)
	}

	tmp, err := os.MkdirTemp("", "krayt-bundle-src-")
	if err != nil {
		return fmt.Errorf("patch: temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	src := filepath.Join(tmp, "src")

	var branch string
	if hasCommits {
		cloneArgs := []string{"clone", "--quiet"}
		if depth > 0 {
			cloneArgs = append(cloneArgs, "--depth", fmt.Sprint(depth))
		}
		cloneArgs = append(cloneArgs, "file://"+absRepo, src)
		if _, err := runGit(ctx, "", cloneArgs...); err != nil {
			return fmt.Errorf("patch: shallow clone source: %w", err)
		}
		if branch, err = currentBranch(ctx, src); err != nil {
			return err
		}
	} else {
		// Unborn HEAD + includeDirty: nothing to clone, so start a fresh empty repo we own
		// and capture the working tree as a root commit.
		if _, err := runGit(ctx, "", "init", "--quiet", "-b", "main", src); err != nil {
			return fmt.Errorf("patch: init empty bundle repo: %w", err)
		}
		branch = "main"
	}

	if includeDirty {
		dirty, err := captureDirty(ctx, src, absRepo, hasCommits, filepath.Join(tmp, "idx"))
		if err != nil {
			return err
		}
		// Move our clone's branch to the dirty commit so HEAD (symbolic → branch) imports
		// it. The clone is ours, so this mutates nothing in the user's repo.
		if _, err := runGit(ctx, src, "update-ref", "refs/heads/"+branch, dirty); err != nil {
			return fmt.Errorf("patch: point branch at dirty commit: %w", err)
		}
	}

	// Name a ref (the branch) plus HEAD so `git clone` of the bundle has something to
	// check out (§6.7).
	if _, err := runGit(ctx, src, "bundle", "create", outBundle, "HEAD", branch); err != nil {
		return fmt.Errorf("patch: create bundle: %w", err)
	}
	return nil
}

// captureDirty builds a throwaway commit of the source working tree (committed state +
// uncommitted, .gitignore honored) without mutating the source repo (§6.7). It writes all
// objects into cloneDir's object database while reading files from srcWorkTree via a
// temporary index, so the user's index/worktree/refs stay untouched. The commit's parent is
// the clone's HEAD when the repo has commits, or it is a root commit on an unborn HEAD. It
// returns the new commit's SHA.
func captureDirty(ctx context.Context, cloneDir, srcWorkTree string, hasCommits bool, indexFile string) (string, error) {
	gitDir := filepath.Join(cloneDir, ".git")
	env := []string{
		"GIT_DIR=" + gitDir,
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
	tree = strings.TrimSpace(tree)

	args := []string{"commit-tree", tree, "-m", "krayt: include uncommitted changes"}
	if hasCommits {
		head, err := runGit(ctx, cloneDir, "rev-parse", "HEAD")
		if err != nil {
			return "", fmt.Errorf("patch: resolve clone HEAD: %w", err)
		}
		args = append(args, "-p", strings.TrimSpace(head))
	}
	// commit-tree needs only GIT_DIR (+ an identity); reuse the bot identity via env so a
	// fresh container/host with no git config still commits.
	commitEnv := []string{
		"GIT_DIR=" + gitDir,
		"GIT_AUTHOR_NAME=" + DefaultIdentity.Name, "GIT_AUTHOR_EMAIL=" + DefaultIdentity.Email,
		"GIT_COMMITTER_NAME=" + DefaultIdentity.Name, "GIT_COMMITTER_EMAIL=" + DefaultIdentity.Email,
	}
	dirty, err := runGitEnv(ctx, commitEnv, args...)
	if err != nil {
		return "", fmt.Errorf("patch: commit dirty tree: %w", err)
	}
	return strings.TrimSpace(dirty), nil
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

// Diff produces changes.patch: everything in workspaceDir since the baseline, whether the
// agent committed or only edited the working tree. We stage all changes into the (throwaway
// guest) index and diff that against the baseline, so an agent that edits a file without
// committing — the common case — still yields a non-empty patch. This is broader than
// §6.7's `git diff baseline..HEAD`, which would miss uncommitted edits (see SPEC flag).
func Diff(ctx context.Context, workspaceDir, baselineRef string) ([]byte, error) {
	if _, err := runGit(ctx, workspaceDir, "add", "-A"); err != nil {
		return nil, fmt.Errorf("patch: stage changes: %w", err)
	}
	out, err := runGitRaw(ctx, workspaceDir, "diff", "--cached", "--binary", baselineRef)
	if err != nil {
		return nil, fmt.Errorf("patch: diff vs baseline: %w", err)
	}
	return out, nil
}

// BundleCommits writes the optional reverse range bundle of the agent's new commits
// (baselineRef..HEAD) to outBundle, so multi-commit work applies faithfully on the host
// via `git fetch` (§6.7). It returns false (and writes nothing) when the agent made no
// commits — HEAD still equals the baseline — in which case changes.patch is the only
// artifact. A range bundle is correct here because the host already has the baseline.
func BundleCommits(ctx context.Context, workspaceDir, baselineRef, outBundle string) (bool, error) {
	head, err := runGit(ctx, workspaceDir, "rev-parse", "HEAD")
	if err != nil {
		return false, fmt.Errorf("patch: rev-parse HEAD: %w", err)
	}
	base, err := runGit(ctx, workspaceDir, "rev-parse", baselineRef)
	if err != nil {
		return false, fmt.Errorf("patch: rev-parse baseline: %w", err)
	}
	if strings.TrimSpace(head) == strings.TrimSpace(base) {
		return false, nil // no new commits; nothing to bundle
	}
	if _, err := runGit(ctx, workspaceDir, "bundle", "create", outBundle, baselineRef+"..HEAD"); err != nil {
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
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}
