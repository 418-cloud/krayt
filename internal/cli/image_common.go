package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/sandbox"
)

// completeImageRefs completes args[0] of `image rm <ref>` from msb's own store (`msb images -q`,
// retire-vm-image-pipeline.md decision 4/Done-when) — krayt keeps no cache of its own to read
// completions from any more.
func completeImageRefs(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) >= 1 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	sb, err := sandbox.NewClient()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	// cmd.Context() is nil unless the command has gone through cobra's Execute() (never true for
	// completion invoked directly, e.g. by tests) — fall back rather than pass nil to exec.CommandContext.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	refs, err := sb.ImageRefs(ctx)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return refs, cobra.ShellCompDirectiveNoFileComp
}

// humanSize renders a byte count as a compact IEC size (e.g. 412MiB, 1.8GiB) — one decimal
// place under 10 units, none above.
func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	val := float64(b) / float64(div)
	if val < 10 {
		return fmt.Sprintf("%.1f%s", val, units[exp])
	}
	return fmt.Sprintf("%.0f%s", val, units[exp])
}

// plural returns "s" unless n == 1.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
