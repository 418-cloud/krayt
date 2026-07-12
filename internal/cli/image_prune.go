package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/imagecache"
	"github.com/418-cloud/krayt/internal/orchestrator"
)

// pruneDecision records what prune chose to do with one cached image and why.
type pruneDecision struct {
	img    cachedImage
	keep   bool
	reason string // human-readable, shown in the summary
}

func newImagePruneCmd() *cobra.Command {
	var repo string
	var olderThan time.Duration
	var all, dryRun bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove cached images outside the retention policy",
		Long: "Reclaims cache disk by removing images that are not protected. Always kept: the " +
			"pinned base VM image. Container images are kept when used within --older-than (default " +
			"24h) or referenced by a non-terminal run under --repo whose image is a digest ref. Every " +
			"non-pinned base VM image is removed. --all bypasses the container protections (never the " +
			"pinned base image); --dry-run reports without deleting.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runImagePrune(cmd, repo, olderThan, all, dryRun)
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "repo whose non-terminal runs protect in-use images")
	cmd.Flags().DurationVar(&olderThan, "older-than", 24*time.Hour, "keep container images used within this window")
	cmd.Flags().BoolVar(&all, "all", false, "ignore age and in-use protections (still keeps the pinned base image)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be removed/kept without deleting")
	return cmd
}

func runImagePrune(cmd *cobra.Command, repo string, olderThan time.Duration, all, dryRun bool) error {
	imgs, err := listCachedImages()
	if err != nil {
		return err
	}
	inUse, err := inUseDigests(repo)
	if err != nil {
		return err
	}

	now := time.Now()
	decisions := make([]pruneDecision, 0, len(imgs))
	for _, ci := range imgs {
		keep, reason := pruneDecide(ci, all, now, olderThan, inUse)
		decisions = append(decisions, pruneDecision{img: ci, keep: keep, reason: reason})
	}

	return reportPrune(cmd, decisions, dryRun)
}

// pruneDecide applies the retention policy (task §prune) to one cached image.
func pruneDecide(ci cachedImage, all bool, now time.Time, olderThan time.Duration, inUse map[string]string) (keep bool, reason string) {
	if ci.kind == kindVMImage {
		// Keep only the pinned base image; every other vmimage entry is unreachable by `krayt
		// run` and removed unconditionally — even under --all the pinned one stays.
		if ci.pinned {
			return true, "pinned base image"
		}
		return false, ""
	}
	// container kind
	if all {
		return false, ""
	}
	if runID, ok := inUse[ci.entry.Digest]; ok {
		return true, "in use by " + runID
	}
	if age := now.Sub(ci.entry.LastUsed); age <= olderThan {
		return true, "used " + humanDuration(age) + " ago"
	}
	return false, ""
}

// inUseDigests maps the resolved digest of every non-terminal run under repo to its run ID,
// but only for runs whose image_ref is itself a digest reference (…@sha256:<hex>) — a
// tag-based ref can't be resolved to a cache digest offline (a documented gap; age protects
// those). A missing/empty .krayt is not an error.
func inUseDigests(repo string) (map[string]string, error) {
	sd, err := stateDir(repo)
	if err != nil {
		return nil, err
	}
	recs, err := orchestrator.List(sd)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, r := range recs {
		if r.Terminal() {
			continue
		}
		if d, ok := digestFromRef(r.ImageRef); ok {
			out[d] = r.ID
		}
	}
	return out, nil
}

// digestFromRef extracts the "sha256:<hex>" digest from a digest reference (…@sha256:<hex>),
// or reports false for a tag-based ref. Matched by direct comparison, no registry resolution.
func digestFromRef(ref string) (string, bool) {
	d := ref
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		d = ref[i+1:]
	}
	if digest.Digest(d).Validate() != nil {
		return "", false
	}
	return d, true
}

// reportPrune prints the decisions and, unless dryRun, removes the entries marked for removal.
func reportPrune(cmd *cobra.Command, decisions []pruneDecision, dryRun bool) error {
	w := cmd.OutOrStdout()
	var removed, kept []pruneDecision
	var reclaim int64
	for _, d := range decisions {
		if d.keep {
			kept = append(kept, d)
		} else {
			removed = append(removed, d)
			reclaim += d.img.entry.SizeB
		}
	}

	verb := "removed"
	if dryRun {
		verb = "would remove"
	}
	for _, d := range removed {
		if !dryRun {
			if err := imagecache.Remove(d.img.entry); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "%s %s %s (%s)\n", verb, d.img.kind, shortDigest(d.img.entry.Digest), humanSize(d.img.entry.SizeB)); err != nil {
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
		if _, err := fmt.Fprintf(w, "kept %s %s (%s)\n", d.img.kind, shortDigest(d.img.entry.Digest), d.reason); err != nil {
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
