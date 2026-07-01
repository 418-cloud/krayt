package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/orchestrator"
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
			_, _ = fmt.Fprintln(w, "RUN\tSTATE\tEXIT\tIMAGE\tSTARTED")
			for _, r := range recs {
				exit := "-"
				if r.Terminal() {
					exit = fmt.Sprint(r.ExitCode)
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.State, exit, r.ImageRef, r.StartedAt)
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
		Use:   "logs <run-id>",
		Short: "Print a run's persisted logs",
		Args:  cobra.ExactArgs(1),
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
		Use:   "attach <run-id>",
		Short: "Live-stream a running agent's logs (until it finishes or Ctrl-C)",
		Args:  cobra.ExactArgs(1),
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
		Short: "Stop a running run (signals its supervisor to tear the VM down)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sd, err := stateDir(repo)
			if err != nil {
				return err
			}
			rec, err := orchestrator.ReadRecord(orchestrator.RunDir(sd, args[0]))
			if err != nil {
				return fmt.Errorf("no such run %q: %w", args[0], err)
			}
			if rec.Terminal() {
				return fmt.Errorf("run %q already finished (%s)", args[0], rec.State)
			}
			if rec.PID <= 0 {
				return fmt.Errorf("run %q has no recorded supervisor pid", args[0])
			}
			// SIGTERM the supervising `krayt run`; its signal handler cancels the run
			// context, which guarantees VM teardown (§6.2, §7).
			if err := syscall.Kill(rec.PID, syscall.SIGTERM); err != nil {
				return fmt.Errorf("signal run %q (pid %d): %w", args[0], rec.PID, err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "stopping %s (pid %d)\n", args[0], rec.PID)
			return err
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "repo whose .krayt state to read")
	return cmd
}

func newRmCmd() *cobra.Command {
	var repo string
	var force bool
	cmd := &cobra.Command{
		Use:   "rm <run-id>",
		Short: "Remove a finished run's artifacts",
		Args:  cobra.ExactArgs(1),
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
		Use:   "patch <run-id>",
		Short: "Print the path to a run's changes.patch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sd, err := stateDir(repo)
			if err != nil {
				return err
			}
			p := filepath.Join(orchestrator.RunDir(sd, args[0]), "changes.patch")
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
