package cli

import (
	"context"
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

// newDoctorCmd builds the `doctor` command (§13). --repo is optional and, unlike every other
// management command's --repo, defaults to empty rather than ".": doctor is commonly run with no
// repo in mind at all (a bare host-prereq check), and an empty default keeps the orphan-sandbox
// check's "skipped — pass --repo to check" honest rather than silently scanning the current
// directory's .krayt/ nobody asked about.
func newDoctorCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check host prerequisites for running krayt",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd.Context(), cmd.OutOrStdout(), repo)
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "repo whose .krayt/ to cross-reference for the orphaned-sandbox check (add-interactive-shell-session.md decision 6); omit to skip that check")
	return cmd
}

func runDoctor(ctx context.Context, w io.Writer, repo string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	checks := commonChecks()
	// Fifth check, needing repo state that the four msb checks above don't — see
	// orphanSandboxCheck's own doc comment for why it degrades rather than fails.
	checks = append(checks, orphanSandboxCheck(ctx, repo))
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
