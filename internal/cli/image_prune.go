package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/sandbox"
)

// pruneDecision records what prune chose to do with one of msb's images and why.
type pruneDecision struct {
	img    sandbox.ImageInfo
	keep   bool
	reason string // human-readable, shown in the summary
}

// imageUse summarizes what krayt's own run records know about one image ref: whether a
// non-terminal run is currently using it, and the most recent time any run (terminal or not)
// used it — the two facts krayt's age/in-use retention needs (retire-vm-image-pipeline.md
// decision 3), now that msb's own store carries no age policy or krayt-visible last-used time.
type imageUse struct {
	runID    string // non-empty: a non-terminal run's ID currently using this ref
	lastUsed time.Time
}

func newImagePruneCmd() *cobra.Command {
	var repo string
	var olderThan time.Duration
	var all, dryRun bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove images outside the retention policy",
		Long: "Reclaims msb's store by removing images that are not protected. An image is " +
			"protected when a non-terminal run under --repo references it, or when some run " +
			"(terminal or not) used it within --older-than (default 24h) — krayt's own retention " +
			"policy, layered on top of msb's own `image prune` sweep of dangling artifacts (" +
			"retire-vm-image-pipeline.md decision 3). --all bypasses both protections. --dry-run " +
			"reports what would happen without removing or pruning anything, though it still lists " +
			"msb's images to compute the report.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runImagePrune(cmd, repo, olderThan, all, dryRun)
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "repo whose runs protect in-use/recently-used images")
	cmd.Flags().DurationVar(&olderThan, "older-than", 24*time.Hour, "keep images used within this window")
	cmd.Flags().BoolVar(&all, "all", false, "ignore age and in-use protections")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be removed/kept without deleting")
	return cmd
}

func runImagePrune(cmd *cobra.Command, repo string, olderThan time.Duration, all, dryRun bool) error {
	sb, err := sandbox.NewClient()
	if err != nil {
		return err
	}
	imgs, err := sb.Images(cmd.Context())
	if err != nil {
		return err
	}
	uses, err := imageUses(repo)
	if err != nil {
		return err
	}

	now := time.Now()
	decisions := make([]pruneDecision, 0, len(imgs))
	for _, img := range imgs {
		keep, reason := pruneDecide(img, all, now, olderThan, uses)
		decisions = append(decisions, pruneDecision{img: img, keep: keep, reason: reason})
	}

	return reportPrune(cmd, sb, decisions, all, dryRun)
}

// pruneDecide applies the retention policy (decision 3) to one of msb's images.
func pruneDecide(img sandbox.ImageInfo, all bool, now time.Time, olderThan time.Duration, uses map[string]imageUse) (keep bool, reason string) {
	if all {
		return false, ""
	}
	u, ok := uses[img.Ref]
	if !ok {
		return false, ""
	}
	if u.runID != "" {
		return true, "in use by " + u.runID
	}
	if age := now.Sub(u.lastUsed); age <= olderThan {
		return true, "used " + humanDuration(age) + " ago"
	}
	return false, ""
}

// imageUses maps every image_ref recorded by any run under repo (`.krayt/runs/*/meta.json`,
// which already records it — decision 3) to what krayt knows about its use: a non-terminal run's
// ID, if any currently reference it, and the most recent used-at time (EndedAt, falling back to
// StartedAt for a still-running run) across every run that referenced it. A missing/empty .krayt
// is not an error.
func imageUses(repo string) (map[string]imageUse, error) {
	sd, err := stateDir(repo)
	if err != nil {
		return nil, err
	}
	recs, err := orchestrator.List(sd)
	if err != nil {
		return nil, err
	}
	out := map[string]imageUse{}
	for _, r := range recs {
		if r.ImageRef == "" {
			continue
		}
		u := out[r.ImageRef]
		if !r.Terminal() && u.runID == "" {
			u.runID = r.ID
		}
		if t := recordUsedAt(r); t.After(u.lastUsed) {
			u.lastUsed = t
		}
		out[r.ImageRef] = u
	}
	return out, nil
}

// recordUsedAt is the best single timestamp for when a run last used its image: EndedAt once the
// run is terminal, else StartedAt for a run still in flight. An unparsable/absent timestamp
// yields the zero time, which never satisfies an age window.
func recordUsedAt(r orchestrator.RunRecord) time.Time {
	ts := r.EndedAt
	if ts == "" {
		ts = r.StartedAt
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Time{}
	}
	return t
}

// reportPrune prints the decisions and, unless dryRun, removes the entries marked for removal via
// `msb rmi`, then sweeps whatever msb's own store still considers dangling via `msb image prune`.
// dryRun calls neither.
func reportPrune(cmd *cobra.Command, sb *sandbox.Client, decisions []pruneDecision, all, dryRun bool) error {
	w := cmd.OutOrStdout()
	var removed, kept []pruneDecision
	var reclaim int64
	for _, d := range decisions {
		if d.keep {
			kept = append(kept, d)
		} else {
			removed = append(removed, d)
			reclaim += d.img.SizeB
		}
	}

	verb := "removed"
	if dryRun {
		verb = "would remove"
	}
	for _, d := range removed {
		if !dryRun {
			if err := sb.Rmi(cmd.Context(), d.img.Ref, all); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "%s %s (%s)\n", verb, d.img.Ref, humanSize(d.img.SizeB)); err != nil {
			return err
		}
	}
	if !dryRun {
		if err := sb.ImagePrune(cmd.Context()); err != nil {
			return err
		}
	}

	summaryVerb := "removed"
	if dryRun {
		summaryVerb = "would remove"
	}
	if _, err := fmt.Fprintf(w, "%s %d image%s, %s reclaimed; kept %d\n",
		summaryVerb, len(removed), plural(len(removed)), humanSize(reclaim), len(kept)); err != nil {
		return err
	}
	for _, d := range kept {
		if _, err := fmt.Fprintf(w, "kept %s (%s)\n", d.img.Ref, d.reason); err != nil {
			return err
		}
	}
	return nil
}

// humanDuration renders an age compactly for the kept-summary ("3h", "2d", "45m").
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
