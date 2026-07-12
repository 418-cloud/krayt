package cli

import (
	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/orchestrator"
)

// completeRunIDs returns a cobra ValidArgsFunction that completes <run-id> from the command's
// --repo, newest-first, annotated with "<state>, <image-ref>". keep filters which runs are
// suggested; pass nil to suggest every run.
func completeRunIDs(keep func(rec orchestrator.RunRecord, cmd *cobra.Command) bool) func(
	cmd *cobra.Command, args []string, toComplete string,
) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) >= 1 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		repo, _ := cmd.Flags().GetString("repo")
		sd, err := stateDir(repo)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		recs, err := orchestrator.List(sd)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var out []string
		for _, rec := range recs {
			if keep != nil && !keep(rec, cmd) {
				continue
			}
			out = append(out, cobra.CompletionWithDesc(rec.ID, rec.State+", "+truncate(rec.ImageRef, 40)))
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// completeQuestionIDs completes args[1] of `answer <run-id> [<question-id>] <response>` with
// the run's pending question IDs, annotated with a sanitized, truncated prompt snippet.
func completeQuestionIDs(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 1 { // only right after <run-id>
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	repo, _ := cmd.Flags().GetString("repo")
	sd, err := stateDir(repo)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	qs, err := orchestrator.ReadQuestions(orchestrator.RunDir(sd, args[0]))
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, q := range qs {
		if !isPending(q) {
			continue
		}
		out = append(out, cobra.CompletionWithDesc(q.ID, truncate(orchestrator.Sanitize(q.Prompt), 40)))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// completeCachedImageDigests completes args[0] of `image rm <digest>` with the full encoded
// digest of every cached image (both cache roots), annotated with "<kind>, <size>" and
// "(pinned)" for the pinned base image. The full digest is offered so a selection is always
// unambiguous and directly removable.
func completeCachedImageDigests(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) >= 1 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	imgs, err := listCachedImages()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, ci := range imgs {
		if ci.entry.Digest == "" {
			continue
		}
		desc := ci.kind + ", " + humanSize(ci.entry.SizeB)
		if ci.pinned {
			desc += " (pinned)"
		}
		out = append(out, cobra.CompletionWithDesc(digestEncoded(ci.entry.Digest), desc))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// truncate shortens s for a completion description so one long value can't break the shell's
// completion-list formatting.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
}
