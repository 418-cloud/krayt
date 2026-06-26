package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// checkResult is one host-prerequisite check (§13 `krayt doctor`). An optional check
// that fails is reported as a warning and does not fail the command.
type checkResult struct {
	name     string
	ok       bool
	optional bool
	detail   string
}

// commonChecks are the platform-agnostic prerequisite checks, appended to hostChecks.
func commonChecks() []checkResult {
	return []checkResult{baseImageCheck()}
}

// hostChecks returns the platform-specific prerequisite checks; it is implemented per
// OS in doctor_darwin.go / doctor_linux.go / doctor_other.go.

// newDoctorCmd builds the `doctor` command (§13).
func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check host prerequisites for running krayt",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd.OutOrStdout())
		},
	}
}

func runDoctor(w io.Writer) error {
	checks := append(hostChecks(), commonChecks()...)
	allOK := true
	for _, c := range checks {
		mark := "ok"
		if !c.ok {
			if c.optional {
				mark = "warn"
			} else {
				mark = "FAIL"
				allOK = false
			}
		}
		if c.detail != "" {
			if _, err := fmt.Fprintf(w, "[%s] %s — %s\n", mark, c.name, c.detail); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintf(w, "[%s] %s\n", mark, c.name); err != nil {
			return err
		}
	}
	if !allOK {
		return fmt.Errorf("doctor: one or more prerequisite checks failed")
	}
	return nil
}
