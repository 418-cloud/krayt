package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/sandbox"
)

// stateDir resolves the .krayt directory for a repo (default cwd). All management commands
// read/manipulate on-disk state there, so they work regardless of which process supervises a
// run (§6.2).
func stateDir(repo string) (string, error) {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return "", err
	}
	return filepath.Join(abs, ".krayt"), nil
}

func newLsCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List runs and their state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sd, err := stateDir(repo)
			if err != nil {
				return err
			}
			recs, err := orchestrator.List(sd)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "RUN\tSTATE\tEXIT\tIMAGE\tSTARTED\tKIND")
			for _, r := range recs {
				// The exit code is only meaningful for a clean/killed container exit; a
				// `failed` run errored in orchestration (no exit code) — show "-", not a
				// misleading 0. The reason is in the run's meta.json `error`.
				exit := "-"
				if r.State == orchestrator.StateDone || r.State == orchestrator.StateTimedOut {
					exit = fmt.Sprint(r.ExitCode)
				}
				// Hint that a `waiting` run has outstanding questions, nudging toward
				// `krayt questions <id>` (§6.13).
				state := r.State
				if r.State == orchestrator.StateWaiting {
					if n := pendingQuestions(sd, r.ID); n > 0 {
						state = fmt.Sprintf("%s (%d?)", r.State, n)
					}
				}
				// KIND is trailing so this new column can't shift the position of any of the
				// existing ones anything might already be parsing by field index.
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, state, exit, r.ImageRef, r.StartedAt, r.EffectiveKind())
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "repo whose .krayt state to read")
	return cmd
}

func newLogsCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:               "logs <run-id>",
		Short:             "Print a run's persisted logs",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRunIDs(nil),
		RunE: func(cmd *cobra.Command, args []string) error {
			sd, err := stateDir(repo)
			if err != nil {
				return err
			}
			b, err := os.ReadFile(orchestrator.LogPath(orchestrator.RunDir(sd, args[0])))
			if err != nil {
				return fmt.Errorf("no logs for run %q: %w", args[0], err)
			}
			_, err = cmd.OutOrStdout().Write(b)
			return err
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "repo whose .krayt state to read")
	return cmd
}

func newAttachCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:               "attach <run-id>",
		Short:             "Live-stream a running agent's logs (until it finishes or Ctrl-C)",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRunIDs(func(rec orchestrator.RunRecord, _ *cobra.Command) bool { return !rec.Terminal() }),
		RunE: func(cmd *cobra.Command, args []string) error {
			sd, err := stateDir(repo)
			if err != nil {
				return err
			}
			runDir := orchestrator.RunDir(sd, args[0])
			if _, err := orchestrator.ReadRecord(runDir); err != nil {
				return fmt.Errorf("no such run %q: %w", args[0], err)
			}
			err = orchestrator.FollowLog(cmd.Context(), runDir, cmd.OutOrStdout(), 200*time.Millisecond)
			if errors.Is(err, context.Canceled) {
				return nil // Ctrl-C is a clean detach, not an error
			}
			return err
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "repo whose .krayt state to read")
	return cmd
}

func newStopCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "stop <run-id>",
		Short: "Stop a running run, or destroy a kept `krayt shell`/`krayt code` sandbox (signals its supervisor to tear the VM down)",
		Args:  cobra.ExactArgs(1),
		ValidArgsFunction: completeRunIDs(func(rec orchestrator.RunRecord, _ *cobra.Command) bool {
			return !rec.Terminal() || rec.State == orchestrator.StateKept
		}),
		RunE: func(cmd *cobra.Command, args []string) error {
			sd, err := stateDir(repo)
			if err != nil {
				return err
			}
			runDir := orchestrator.RunDir(sd, args[0])
			rec, err := orchestrator.ReadRecord(runDir)
			if err != nil {
				return fmt.Errorf("no such run %q: %w", args[0], err)
			}
			// A `--keep` session the human has already exited — `krayt shell`
			// (add-interactive-shell-session.md decision 3) or `krayt code`: both clear PID on the
			// way to `kept` because nothing supervises the sandbox any more, so there is no
			// process left to signal — stop the sandbox directly by name instead.
			if rec.State == orchestrator.StateKept {
				if err := stopKeptSession(cmd, runDir, args[0], rec); err != nil {
					return err
				}
				// The session's ssh alias must stop being offered the moment its sandbox is gone
				// (`krayt code`); a no-op for a repo that has never run one.
				return orchestrator.PruneAggregateSSHConfig(sd)
			}
			if rec.Terminal() {
				return fmt.Errorf("run %q already finished (%s)", args[0], rec.State)
			}
			// The supervising krayt process is gone (kill -9, a crash) — there is nothing left
			// to signal and nothing that will ever tear the sandbox down, so clean it up here.
			if !processAlive(rec.PID) {
				return stopOrphanedRun(cmd, runDir, args[0], rec)
			}
			// Signal the supervising `krayt run`/`krayt shell` to stop (proc_unix.go/
			// proc_windows.go); on unix its SIGTERM handler cancels the run context, which
			// guarantees VM teardown (§6.2, §7) — Windows has no such graceful path
			// (proc_windows.go).
			if err := killSupervisor(rec.PID); err != nil {
				return fmt.Errorf("signal run %q (pid %d): %w", args[0], rec.PID, err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "stopping %s (pid %d)\n", args[0], rec.PID)
			return err
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "repo whose .krayt state to read")
	return cmd
}

// processAlive is supervisorAlive (proc_unix.go/proc_windows.go), swappable in tests.
var processAlive = supervisorAlive

// stopOrphanedRun cleans up a non-terminal run whose supervising krayt process died without
// tearing its sandbox down: it stops and removes the sandbox by name if msb still lists it, then
// marks the record failed so it no longer reads as live to `krayt ls` or `krayt doctor`.
func stopOrphanedRun(cmd *cobra.Command, runDir, id string, rec orchestrator.RunRecord) error {
	removed := false
	if rec.SandboxName != "" {
		sb, err := sandbox.NewClient()
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		sandboxes, err := sb.List(ctx)
		if err != nil {
			return fmt.Errorf("list sandboxes: %w", err)
		}
		for _, s := range sandboxes {
			if s.Name != rec.SandboxName {
				continue
			}
			if err := sb.Stop(ctx, rec.SandboxName); err != nil {
				return fmt.Errorf("stop sandbox %q: %w", rec.SandboxName, err)
			}
			if err := sb.Remove(ctx, rec.SandboxName); err != nil {
				return fmt.Errorf("remove sandbox %q: %w", rec.SandboxName, err)
			}
			removed = true
			break
		}
	}
	pid := rec.PID
	rec.State, rec.PID = orchestrator.StateFailed, 0
	if rec.Error == "" {
		rec.Error = fmt.Sprintf("krayt process (pid %d) exited without tearing the sandbox down; cleaned up by `krayt stop`", pid)
	}
	if err := orchestrator.WriteRecord(runDir, rec); err != nil {
		return err
	}
	outcome := "no sandbox was left to remove"
	if removed {
		outcome = "removed sandbox " + rec.SandboxName
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "stopped %s: its krayt process (pid %d) was no longer running; %s\n", id, pid, outcome)
	return err
}

// stopKeptSession destroys a `--keep` session's sandbox directly (msb stop + rm by name) —
// decision 3's escape hatch, since a kept session has no supervising process left to signal. It
// updates the run record to `done` rather than leaving a stale `kept` record pointing at a
// sandbox that no longer exists, which is exactly the confusion `krayt doctor`'s orphan check
// (decision 6) would otherwise have to explain: `kept` is supposed to mean a live, re-enterable
// sandbox, so a `kept` record with no matching sandbox is itself a bug this command must not leave
// behind. Kind-agnostic: `krayt shell --keep` and `krayt code --keep` leave the identical state,
// and the only difference is the word in the message.
func stopKeptSession(cmd *cobra.Command, runDir, id string, rec orchestrator.RunRecord) error {
	sb, err := sandbox.NewClient()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if err := sb.Stop(ctx, rec.SandboxName); err != nil {
		return fmt.Errorf("stop sandbox %q: %w", rec.SandboxName, err)
	}
	if err := sb.Remove(ctx, rec.SandboxName); err != nil {
		return fmt.Errorf("remove sandbox %q: %w", rec.SandboxName, err)
	}
	rec.State = orchestrator.StateDone
	if err := orchestrator.WriteRecord(runDir, rec); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "stopped kept %s session %s (sandbox %s)\n",
		rec.EffectiveKind(), id, rec.SandboxName)
	return err
}

func newRmCmd() *cobra.Command {
	var repo string
	var force bool
	cmd := &cobra.Command{
		Use:   "rm <run-id>",
		Short: "Remove a finished run's artifacts",
		Args:  cobra.ExactArgs(1),
		ValidArgsFunction: completeRunIDs(func(rec orchestrator.RunRecord, cmd *cobra.Command) bool {
			if rec.Terminal() {
				return true
			}
			force, _ := cmd.Flags().GetBool("force")
			return force
		}),
		RunE: func(cmd *cobra.Command, args []string) error {
			sd, err := stateDir(repo)
			if err != nil {
				return err
			}
			runDir := orchestrator.RunDir(sd, args[0])
			rec, err := orchestrator.ReadRecord(runDir)
			if err != nil {
				return fmt.Errorf("no such run %q: %w", args[0], err)
			}
			if !rec.Terminal() && !force {
				return fmt.Errorf("run %q is %s; stop it first or use --force", args[0], rec.State)
			}
			if err := os.RemoveAll(runDir); err != nil {
				return err
			}
			// Removing the run dir removes the per-run ssh config the aggregate Includes.
			if err := orchestrator.PruneAggregateSSHConfig(sd); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", args[0])
			return err
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "repo whose .krayt state to read")
	cmd.Flags().BoolVar(&force, "force", false, "remove even if the run is not finished")
	return cmd
}

func newPatchCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:               "patch <run-id>",
		Short:             "Print the path to a run's changes.patch (re-derives it first for a live `krayt shell`/`krayt code` session)",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRunIDs(nil),
		RunE: func(cmd *cobra.Command, args []string) error {
			sd, err := stateDir(repo)
			if err != nil {
				return err
			}
			runDir := orchestrator.RunDir(sd, args[0])
			rec, err := orchestrator.ReadRecord(runDir)
			if err != nil {
				return fmt.Errorf("no such run %q: %w", args[0], err)
			}
			// A live human-driven session — `krayt shell` (decision 9,
			// add-interactive-shell-session.md) or `krayt code`
			// (add-vscode-remote-ssh-session.md decision 9): re-run krayt-helper finish +
			// copy-out on demand against the running sandbox rather than stat-ing a file that may
			// be stale or may not exist yet. Repeatable and idempotent — each call re-derives the
			// patch from the current workspace. `running` covers a session someone is actively in
			// right now; `kept` covers one between attaches/connections. Every other state (an
			// ordinary run, or a session that already exited without --keep) falls through to the
			// plain stat below, byte-for-byte unchanged.
			if orchestrator.IsSessionKind(rec.EffectiveKind()) &&
				(rec.State == orchestrator.StateRunning || rec.State == orchestrator.StateKept) {
				return patchLiveShell(cmd, runDir, args[0])
			}
			p := filepath.Join(runDir, "changes.patch")
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("no patch for run %q: %w", args[0], err)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), p)
			return err
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "repo whose .krayt state to read")
	return cmd
}

// patchLiveShell drives orchestrator.PatchLiveShell against a live shell session's sandbox and
// prints the fresh changes.patch path — the msb driver instance is created fresh here rather than
// threaded through from elsewhere, matching how every other management command in this file
// resolves its own sandbox.Client on demand rather than holding one across the process.
func patchLiveShell(cmd *cobra.Command, runDir, id string) error {
	sb, err := sandbox.NewClient()
	if err != nil {
		return err
	}
	p, err := orchestrator.PatchLiveShell(cmd.Context(), orchestrator.Deps{Sandbox: sb}, runDir)
	if err != nil {
		return fmt.Errorf("patch run %q: %w", id, err)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), p)
	return err
}
