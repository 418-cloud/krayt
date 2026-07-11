package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/orchestrator"
)

// newQuestionsCmd lists a run's agent→human questions and answers, so the human never reads
// questions/*.json by hand (§6.13). Prompts/choices/answers are agent-originated, so they're
// sanitized and clearly labelled on display; each pending entry prints the exact `krayt answer`
// line to run. `--pending-only` and `--sort` shape the view; the default is the full history,
// chronological (oldest-first) — a stable order that's always familiar.
func newQuestionsCmd() *cobra.Command {
	var repo, sortMode string
	var pendingOnly bool
	cmd := &cobra.Command{
		Use:               "questions <run-id>",
		Aliases:           []string{"q"},
		Short:             "List a run's agent questions and answers (§6.13)",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRunIDs(nil),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateSort(sortMode); err != nil {
				return err
			}
			sd, err := stateDir(repo)
			if err != nil {
				return err
			}
			id := args[0]
			runDir := orchestrator.RunDir(sd, id)
			if _, err := orchestrator.ReadRecord(runDir); err != nil {
				return fmt.Errorf("no such run %q: %w", id, err)
			}
			all, err := orchestrator.ReadQuestions(runDir)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(all) == 0 {
				_, err := fmt.Fprintf(out, "run %s has no questions\n", id)
				return err
			}
			qs := selectQuestions(all, pendingOnly, sortMode)
			if len(qs) == 0 { // everything was filtered out by --pending-only
				_, err := fmt.Fprintf(out, "run %s has no pending questions\n", id)
				return err
			}

			var b strings.Builder
			pending := 0
			for _, q := range qs {
				status, stamp := "pending", q.AskedAt
				switch {
				case isPending(q):
					pending++
				case q.NoAnswer:
					status, stamp = "no answer (timeout)", q.AnswerAt
				default:
					status, stamp = "answered by human", q.AnswerAt
				}
				fmt.Fprintf(&b, "%s  [%s]  %s\n\n", q.ID, status, stamp)
				fmt.Fprintf(&b, "%s\n", indentLines(orchestrator.Sanitize(q.Prompt), "  "))
				if len(q.Choices) > 0 {
					cs := make([]string, len(q.Choices))
					for i, c := range q.Choices {
						cs[i] = orchestrator.Sanitize(c)
					}
					fmt.Fprintf(&b, "  choices: %s\n", strings.Join(cs, " | "))
				}
				switch {
				case isPending(q):
					fmt.Fprintf(&b, "  answer:  krayt answer %s %s <response>\n", id, q.ID)
				case q.NoAnswer:
					b.WriteString("  → (no answer)\n")
				default:
					fmt.Fprintf(&b, "  → %s\n", orchestrator.Sanitize(q.Response))
				}
				b.WriteString("\n")
			}
			if pending > 0 {
				fmt.Fprintf(&b, "%d pending — answer with: krayt answer %s <response>\n", pending, id)
			}
			_, err = fmt.Fprint(out, b.String())
			return err
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "repo whose .krayt state to read")
	cmd.Flags().BoolVar(&pendingOnly, "pending-only", false, "show only unanswered questions")
	cmd.Flags().StringVar(&sortMode, "sort", "asked", "order: asked (oldest-first) | pending-first | pending-last")
	_ = cmd.RegisterFlagCompletionFunc("sort", cobra.FixedCompletions(sortModes, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}

// isPending reports whether a question is still awaiting an answer.
func isPending(q orchestrator.QuestionRecord) bool { return q.AnswerAt == "" }

// sortModes is the authoritative set of valid --sort values, shared by validation and shell
// completion so the two can't drift.
var sortModes = []string{"asked", "pending-first", "pending-last"}

// validateSort keeps the set of valid --sort values authoritative here.
func validateSort(mode string) error {
	for _, m := range sortModes {
		if mode == m {
			return nil
		}
	}
	return fmt.Errorf("invalid --sort %q (want asked, pending-first, or pending-last)", mode)
}

// selectQuestions applies --pending-only and --sort. ReadQuestions returns oldest-first, and the
// pending-* sorts are stable, so chronological order is preserved within each group.
func selectQuestions(all []orchestrator.QuestionRecord, pendingOnly bool, sortMode string) []orchestrator.QuestionRecord {
	qs := make([]orchestrator.QuestionRecord, 0, len(all))
	for _, q := range all {
		if pendingOnly && !isPending(q) {
			continue
		}
		qs = append(qs, q)
	}
	switch sortMode {
	case "pending-first":
		sort.SliceStable(qs, func(i, j int) bool { return isPending(qs[i]) && !isPending(qs[j]) })
	case "pending-last":
		sort.SliceStable(qs, func(i, j int) bool { return !isPending(qs[i]) && isPending(qs[j]) })
	}
	return qs
}

// pendingQuestions counts a run's unanswered questions, for the `krayt ls` waiting-row hint.
// Best-effort: an unreadable questions dir counts as zero.
func pendingQuestions(stateDir, id string) int {
	qs, err := orchestrator.ReadQuestions(orchestrator.RunDir(stateDir, id))
	if err != nil {
		return 0
	}
	n := 0
	for _, q := range qs {
		if isPending(q) {
			n++
		}
	}
	return n
}

// indentLines prefixes every line of s with pad, so a multi-line agent prompt stays aligned.
func indentLines(s, pad string) string {
	if s == "" {
		return pad
	}
	return pad + strings.ReplaceAll(s, "\n", "\n"+pad)
}
