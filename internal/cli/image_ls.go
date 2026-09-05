package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/sandbox"
)

func newImageLsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List images in msb's own store",
		Long:  "Lists what `msb images --format json` reports — krayt keeps no image cache of its own any more (retire-vm-image-pipeline.md).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sb, err := sandbox.NewClient()
			if err != nil {
				return err
			}
			imgs, err := sb.Images(cmd.Context())
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			if _, err := fmt.Fprintln(w, "REF\tSIZE"); err != nil {
				return err
			}
			var total int64
			for _, img := range imgs {
				total += img.SizeB
				ref := img.Ref
				if ref == "" {
					ref = "-"
				}
				if _, err := fmt.Fprintf(w, "%s\t%s\n", ref, humanSize(img.SizeB)); err != nil {
					return err
				}
			}
			if err := w.Flush(); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%d image%s, %s total\n", len(imgs), plural(len(imgs)), humanSize(total))
			return err
		},
	}
	return cmd
}
