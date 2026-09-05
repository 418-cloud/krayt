package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/sandbox"
)

func newImageRmCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rm <ref>",
		Short: "Remove an image from msb's store",
		Long: "Removes one image by reference (`msb rmi`) — krayt's image store is msb's own, " +
			"ref-keyed, not a krayt-owned digest cache (retire-vm-image-pipeline.md decision 4). " +
			"--force maps to msb's own --force, which allows removing an image a sandbox still " +
			"references.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeImageRefs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sb, err := sandbox.NewClient()
			if err != nil {
				return err
			}
			if err := sb.Rmi(cmd.Context(), args[0], force); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", args[0])
			return err
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove even an image a sandbox still references")
	return cmd
}
