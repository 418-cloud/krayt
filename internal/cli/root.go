// Package cli is the cobra command surface for krayt (§13). Phase 0 wires only the
// root command and `doctor`; the run/ls/attach/etc. commands arrive in later phases.
package cli

import (
	"github.com/spf13/cobra"
)

// Version is the krayt CLI version, reported by `krayt --version`.
const Version = "0.0.0-dev"

// NewRootCmd builds the root command tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "krayt",
		Short:         "Ephemeral VM sandbox for AI coding agents",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newImageCmd())
	root.AddCommand(newRunCmd())
	root.AddCommand(newApplyCmd())
	root.AddCommand(newLsCmd())
	root.AddCommand(newLogsCmd())
	root.AddCommand(newAttachCmd())
	root.AddCommand(newStopCmd())
	root.AddCommand(newRmCmd())
	root.AddCommand(newPatchCmd())
	root.AddCommand(newAnswerCmd())
	return root
}
