package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/vmimage"
)

// newVersionCmd prints the krayt CLI version and the base VM image it pins (§11.4), so a user can
// see exactly which image `krayt run` will pull for this build.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the krayt version and the pinned VM image",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(),
				"krayt %s\nvm-image: %s\n  digest: %s\n",
				Version, vmimage.PinnedRef, vmimage.PinnedDigest)
			return err
		},
	}
}
