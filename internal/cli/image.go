package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/sandbox"
)

func newImageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Manage images in msb's own store",
		Long: "A thin front-end over msb's image store (retire-vm-image-pipeline.md) — krayt " +
			"keeps no image cache of its own any more. `krayt run` never needs `image pull` first: " +
			"`msb create` resolves and pulls the agent image itself.",
	}
	cmd.AddCommand(newImagePullCmd())
	cmd.AddCommand(newImageLsCmd())
	cmd.AddCommand(newImageRmCmd())
	cmd.AddCommand(newImagePruneCmd())
	return cmd
}

func newImagePullCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pull <ref>",
		Short: "Pull an image into msb's store ahead of a run",
		Long: "Pulls <ref> via `msb pull` so it's already cached before `krayt run` needs it. Not " +
			"required — `msb create` pulls on demand — this just moves the wait earlier.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sb, err := sandbox.NewClient()
			if err != nil {
				return err
			}
			if err := sb.Pull(cmd.Context(), args[0]); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "pulled %s\n", args[0])
			return err
		},
	}
	return cmd
}
