package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newVersionCmd prints the krayt CLI version. There is no more base VM image to pin (§11 — msb
// owns the sandbox image, retire-vm-image-pipeline.md); `krayt image ls` shows what's actually
// cached, in msb's own store.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the krayt version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "krayt %s\n", Version)
			return err
		},
	}
}
