//go:build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/418-cloud/krayt/internal/patch"
)

// setupResult is the JSON object `krayt-helper setup` writes to stdout on success.
type setupResult struct {
	Baseline  string `json:"baseline"`
	Workspace string `json:"workspace"`
	PatchGit  string `json:"patch_git"`
	AgentUser string `json:"agent_user"`
}

func runSetupCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bundle := fs.String("bundle", "", "path to the git bundle to ingest")
	workspace := fs.String("workspace", "", "workspace directory to clone the bundle into")
	patchGit := fs.String("patch-git", "", "root-only git dir snapshotted for patch generation")
	agentUser := fs.String("agent-user", "", "name of the non-root agent user the workspace is relaxed for")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *bundle == "" || *workspace == "" || *patchGit == "" || *agentUser == "" {
		_, _ = fmt.Fprintln(stderr, "krayt-helper setup: --bundle, --workspace, --patch-git, and --agent-user are all required")
		return exitUsage
	}

	res, err := runSetup(context.Background(), *bundle, *workspace, *patchGit, *agentUser)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "krayt-helper setup:", err)
		return exitFailure
	}
	return writeJSON(stdout, stderr, res)
}

// runSetup is setup's testable core (§6.7): ingest the bundle, snapshot the root-only patchgit
// BEFORE relaxing the tree, then relax the workspace so the non-root agent can edit it. The
// ordering is not stylistic — the pristine copy must be taken before the tree becomes
// agent-writable, or the isolation fix-guest-git-config-rce.md bought is void
// (patch.MakeContainerWritable's doc comment states the same rule).
//
// agentUser is not used to change MakeContainerWritable's mechanics: that relaxation is
// uid-agnostic (group+other read/write) precisely because the specific agent uid varies per
// image and this helper has no more visibility into it than the guest-agent it replaces did. It
// is echoed on the result purely for observability of what this invocation was run for.
func runSetup(ctx context.Context, bundle, workspace, patchGitDir, agentUser string) (setupResult, error) {
	baseline, err := patch.Ingest(ctx, bundle, workspace, patch.DefaultIdentity)
	if err != nil {
		return setupResult{}, err
	}
	if err := patch.SetupPatchGit(workspace, patchGitDir); err != nil {
		return setupResult{}, err
	}
	if err := patch.MakeContainerWritable(workspace); err != nil {
		return setupResult{}, err
	}
	return setupResult{Baseline: baseline, Workspace: workspace, PatchGit: patchGitDir, AgentUser: agentUser}, nil
}
