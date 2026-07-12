package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// dateFormat is the LAST USED column format (date granularity is enough for a cache view).
const dateFormat = "2006-01-02"

func newImageLsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List cached base-VM and container images",
		Long: "Lists every image in the two host-side digest-keyed caches — the base micro-VM " +
			"image (§11.4) and the user/agent container images (§6.11) — with size and last-used " +
			"time. The pinned base image is marked (pinned).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			imgs, err := listCachedImages()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			if _, err := fmt.Fprintln(w, "KIND\tDIGEST\tREF\tSIZE\tLAST USED"); err != nil {
				return err
			}
			var total int64
			for _, ci := range imgs {
				total += ci.entry.SizeB
				last := ci.entry.LastUsed.Format(dateFormat)
				if ci.pinned {
					last += " (pinned)"
				}
				if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					ci.kind, shortDigest(ci.entry.Digest), refFor(ci), humanSize(ci.entry.SizeB), last); err != nil {
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

// plural returns "s" unless n == 1.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
