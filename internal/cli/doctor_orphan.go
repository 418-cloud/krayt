package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/sandbox"
)

// sandboxNamePrefix is the prefix every krayt-created msb sandbox carries (orchestrator's own
// sandboxName is unexported, but the prefix itself is public knowledge — meta.json's
// sandbox_name field already documents it, §6.15 decision 8), so a "krayt-*" sandbox with no
// matching run record is recognizably krayt's own leak, not some other tool's sandbox.
const sandboxNamePrefix = "krayt-"

// orphanSandboxCheck is `krayt doctor`'s fifth check (decision 6, add-interactive-shell-session.md):
// cross-reference the live msb sandbox list against .krayt/runs/ under repo, and report any
// "krayt-*" sandbox with no matching run record — naming it and the command to stop it. This can
// happen because decision 5 gives a `krayt shell` session no wall-clock budget, so a crashed or
// `kill -9`'d krayt can leave a sandbox running with nothing left to reap it.
//
// It NEVER reaps. A krayt that kills a sandbox it does not fully understand — one deliberately
// kept across a krayt upgrade, say, or created by a version of krayt that named sandboxes
// differently — would be a destructive default; report-only is the safe one. Unlike the four
// msb checks this is a WARNING, not a FAIL: an orphan is untidy, not evidence the host itself is
// broken.
//
// It is the first doctor check that needs repo state at all, so it degrades to reporting
// nothing — not failing the command — when repo is empty or has no .krayt/ yet.
func orphanSandboxCheck(ctx context.Context, repo string) checkResult {
	const name = "no orphaned krayt-* sandboxes"
	if repo == "" {
		return checkResult{name: name, optional: true, detail: "skipped — pass --repo to check"}
	}
	sd, err := stateDir(repo)
	if err != nil {
		return checkResult{name: name, optional: true, detail: "skipped — " + err.Error()}
	}
	recs, err := orchestrator.List(sd)
	if err != nil {
		return checkResult{name: name, optional: true, detail: "skipped — " + err.Error()}
	}
	tracked := make(map[string]bool, len(recs))
	for _, r := range recs {
		if r.SandboxName != "" {
			tracked[r.SandboxName] = true
		}
	}

	sb, err := sandbox.NewClient()
	if err != nil {
		return checkResult{name: name, optional: true, detail: "skipped — " + err.Error()}
	}
	sandboxes, err := sb.List(ctx)
	if err != nil {
		return checkResult{name: name, optional: true, detail: "skipped — msb ls: " + err.Error()}
	}

	var orphans []string
	for _, s := range sandboxes {
		if !strings.HasPrefix(s.Name, sandboxNamePrefix) || tracked[s.Name] {
			continue
		}
		orphans = append(orphans, s.Name)
	}
	if len(orphans) == 0 {
		return checkResult{name: name, ok: true, optional: true}
	}
	details := make([]string, 0, len(orphans))
	for _, o := range orphans {
		// No run id survives to offer `krayt stop <run-id>` — that's the definition of orphaned
		// — so the actionable command is the raw msb one.
		details = append(details, fmt.Sprintf("%s (stop with: msb stop %s && msb rm %s)", o, o, o))
	}
	return checkResult{name: name, optional: true, detail: strings.Join(details, "; ")}
}
