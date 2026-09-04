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

// commonChecks are the (now OS-agnostic — msb is krayt's only sandbox backend,
// run-tasks-on-microsandbox.md) prerequisite checks for `krayt doctor`: exactly the four msb
// checks KRAYT_SPEC.md:1025-1034 defines. baseImageCheck reports on the pre-msb micro-VM image
// (kernel/initrd/rootfs), which no run under msb ever touches, so it does not belong here —
// surfacing it would warn a healthy msb host about an image it will never use.
func commonChecks() []checkResult {
	return msbChecks()
}

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
	checks := commonChecks()
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
