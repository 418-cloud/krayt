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
// "krayt-*" sandbox with no LIVE run record — naming it and the command to stop it. This can
// happen because decision 5 gives a `krayt shell` session no wall-clock budget, so a crashed or
// `kill -9`'d krayt can leave a sandbox running with nothing left to reap it. A record only keeps
// its sandbox legitimately while something owns it: a kept shell session (re-attachable by
// design), or a run/session whose supervising krayt process is still alive. A finished record's
// sandbox is already gone (teardown runs before the record is marked finished), and a dead
// process's sandbox is exactly the crashed-krayt leak — so both still count as orphans even
// though a record exists.
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
	owned := make(map[string]bool, len(recs))
	deadRun := map[string]string{} // sandbox name -> id of a non-terminal run whose krayt process is gone
	for _, r := range recs {
		if r.SandboxName == "" {
			continue
		}
		switch {
		case r.State == orchestrator.StateKept, !r.Terminal() && processAlive(r.PID):
			owned[r.SandboxName] = true
		case !r.Terminal():
			deadRun[r.SandboxName] = r.ID
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
		if !strings.HasPrefix(s.Name, sandboxNamePrefix) || owned[s.Name] {
			continue
		}
		orphans = append(orphans, s.Name)
	}
	if len(orphans) == 0 {
		return checkResult{name: name, ok: true, optional: true}
	}
	details := make([]string, 0, len(orphans))
	for _, o := range orphans {
		// A dead run's record can still be cleaned up through krayt, which also marks it failed.
		// Otherwise no run needs updating, so the actionable command is the raw msb one.
		if id, ok := deadRun[o]; ok {
			details = append(details, fmt.Sprintf("%s (run %s's krayt process is gone; stop with: krayt stop --repo %s %s)", o, id, repo, id))
			continue
		}
		details = append(details, fmt.Sprintf("%s (stop with: msb stop %s && msb rm %s)", o, o, o))
	}
	return checkResult{name: name, optional: true, detail: strings.Join(details, "; ")}
}
