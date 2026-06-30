package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/patch"
)

func newApplyCmd() *cobra.Command {
	var threeWay bool
	var repo string
	cmd := &cobra.Command{
		Use:   "apply <run-id>",
		Short: "git apply a run's changes.patch onto the host repo (after review)",
		Long: "Applies .krayt/runs/<run-id>/changes.patch onto the host repo with `git apply`. " +
			"Review the diff first — nothing auto-applies (§6.7).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repoAbs, err := filepath.Abs(repo)
			if err != nil {
				return err
			}
			patchPath := filepath.Join(repoAbs, ".krayt", "runs", args[0], "changes.patch")
			if _, err := os.Stat(patchPath); err != nil {
				return fmt.Errorf("no patch for run %q: %w", args[0], err)
			}
			if err := patch.Apply(cmd.Context(), repoAbs, patchPath, threeWay); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "applied %s\n", patchPath)
			return err
		},
	}
	cmd.Flags().BoolVar(&threeWay, "3way", false, "use `git apply --3way` for a merge-style apply")
	cmd.Flags().StringVar(&repo, "repo", ".", "host repo to apply onto")
	return cmd
}
