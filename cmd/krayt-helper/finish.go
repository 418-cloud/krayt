//go:build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/418-cloud/krayt/internal/patch"
)

// Artifact file names written under --out, matching internal/guest's fileChangesPatch /
// fileCommitsBundle (§6.7) — the same names both the running guest-agent and this helper produce.
const (
	fileChangesPatch  = "changes.patch"
	fileCommitsBundle = "commits.bundle"
)

// finishResult is the JSON object `krayt-helper finish` writes to stdout on success.
type finishResult struct {
	Baseline      string `json:"baseline"`
	CommitsBundle bool   `json:"commits_bundle"`
	DiffBytes     int    `json:"diff_bytes"`
}

func runFinishCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("finish", flag.ContinueOnError)
	fs.SetOutput(stderr)
	workspace := fs.String("workspace", "", "workspace directory to diff")
	patchGit := fs.String("patch-git", "", "root-only git dir to diff against")
	baseline := fs.String("baseline", "", "baseline ref (tag or SHA), resolved from --patch-git")
	out := fs.String("out", "", "output directory for changes.patch and commits.bundle")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *workspace == "" || *patchGit == "" || *baseline == "" || *out == "" {
		_, _ = fmt.Fprintln(stderr, "krayt-helper finish: --workspace, --patch-git, --baseline, and --out are all required")
		return exitUsage
	}

	res, err := runFinish(context.Background(), *workspace, *patchGit, *baseline, *out)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "krayt-helper finish:", err)
		return exitFailure
	}
	return writeJSON(stdout, stderr, res)
}

// runFinish is finish's testable core (§6.7): diff the workspace against the baseline resolved
// from the root-only patchGitDir into <out>/changes.patch, and bundle the agent's own commits
// (read from the untrusted workspace .git, per patch.BundleCommits' doc comment) into
// <out>/commits.bundle when there are any. It writes nothing outside outDir and patchGitDir
// (the latter only via Diff's git index, e.g. patch.Diff's read-tree/add).
func runFinish(ctx context.Context, workspace, patchGitDir, baseline, outDir string) (finishResult, error) {
	if err := os.MkdirAll(outDir, 0o777); err != nil {
		return finishResult{}, fmt.Errorf("create output dir: %w", err)
	}
	// MkdirAll's mode is subject to umask, so chmod explicitly — mirrors
	// internal/guest/service.go's outputDir handling, since --out is shared with a non-root
	// process the same way.
	if err := os.Chmod(outDir, 0o777); err != nil {
		return finishResult{}, fmt.Errorf("chmod output dir: %w", err)
	}
	diff, err := patch.Diff(ctx, patchGitDir, workspace, baseline)
	if err != nil {
		return finishResult{}, err
	}
	if err := os.WriteFile(filepath.Join(outDir, fileChangesPatch), diff, 0o644); err != nil {
		return finishResult{}, fmt.Errorf("write %s: %w", fileChangesPatch, err)
	}
	hasCommits, err := patch.BundleCommits(ctx, patchGitDir, workspace, baseline, filepath.Join(outDir, fileCommitsBundle))
	if err != nil {
		return finishResult{}, err
	}
	return finishResult{Baseline: baseline, CommitsBundle: hasCommits, DiffBytes: len(diff)}, nil
}
