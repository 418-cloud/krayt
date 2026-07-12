package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/imagecache"
)

func newImageRmCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rm <digest>",
		Short: "Remove a cached image by digest (or unambiguous prefix)",
		Long: "Removes one cached image identified by its full digest or an unambiguous hex " +
			"prefix (docker-rmi style), searching both cache roots. Refuses the pinned base VM " +
			"image unless --force (removing it just makes the next `krayt run` ask you to " +
			"`krayt image pull` again).",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeCachedImageDigests,
		RunE: func(cmd *cobra.Command, args []string) error {
			ci, err := resolveCachedImage(args[0])
			if err != nil {
				return err
			}
			if ci.pinned && !force {
				return fmt.Errorf("%s is the pinned base VM image; refusing without --force", shortDigest(ci.entry.Digest))
			}
			size := ci.entry.SizeB
			if err := imagecache.Remove(ci.entry); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "removed %s (%s reclaimed)\n", shortDigest(ci.entry.Digest), humanSize(size))
			return err
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove even the pinned base VM image")
	return cmd
}
