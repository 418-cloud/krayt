package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
)

// reportName is the human-readable run summary the host writes on completion (§8.4).
const reportName = "report.md"

// writeReport renders the fixed-section human summary to runDir/report.md (§8.4). The host
// always owns the Run/Changes sections (derived from the machine record); the agent's own
// notes — anything it wrote to /output/report.md, collected into the run dir — are surfaced
// verbatim (sanitized) under Notes. Called from the run finalizer, so every run, success or
// failure, leaves a report.
func writeReport(runDir string, rec RunRecord, agentNotes string, metaDigest digest.Digest) error {
	var b strings.Builder
	// Sanitize every externally-sourced field before it lands in a catted artifact. TaskSummary
	// is already sanitized (summarizeTask); the network allow list comes straight from an
	// untrusted <repo>/krayt.yaml with no validation, so it can carry terminal escapes. ImageRef
	// is validated by image acquisition before any report is written, but sanitize it too so the
	// invariant holds without a per-field reachability argument (§6.13).
	fmt.Fprintf(&b, "# Run %s\n", rec.ID)
	fmt.Fprintf(&b, "- Image: %s   Task: %s\n", Sanitize(rec.ImageRef), rec.TaskSummary)
	fmt.Fprintf(&b, "- Result: %s   Exit: %d   Duration: %s\n", resultWord(rec), rec.ExitCode, hms(rec.DurationSecs))
	fmt.Fprintf(&b, "- Network: %s\n", Sanitize(networkLine(rec.Network)))
	if rec.Error != "" {
		fmt.Fprintf(&b, "- Error: %s\n", Sanitize(rec.Error))
	}

	b.WriteString("\n## Changes\n")
	if rec.Patch != nil {
		noun := "files"
		if rec.Patch.FilesChanged == 1 {
			noun = "file"
		}
		fmt.Fprintf(&b, "%d %s, +%d/-%d. See changes.patch.\n",
			rec.Patch.FilesChanged, noun, rec.Patch.Insertions, rec.Patch.Deletions)
	} else {
		b.WriteString("No changes.patch was produced.\n")
	}

	if rec.Provenance != nil {
		p := rec.Provenance
		dirty := "no"
		if p.IncludeDirty {
			dirty = "yes"
		}
		// SHAs and digests are host-computed (git output / content hashes), not agent text, so they
		// carry no terminal escapes — but the labels are fixed and load-bearing. The metadata-digest
		// wording is deliberately a drift/consistency check, NOT a signature or tamper-evidence:
		// meta.json and report.md are written back-to-back from the same in-memory record by the same
		// trusted host process, so a consistent edit of both can't be detected — this only lets a
		// report.md held apart from meta.json confirm the two still match (§8.4).
		b.WriteString("\n## Provenance\n")
		fmt.Fprintf(&b, "- Commit: %s  (bundle: %s, depth: %d, dirty: %s)\n", p.HeadSHA, p.BundleSHA, p.BundleDepth, dirty)
		fmt.Fprintf(&b, "- Bundle digest: %s\n", p.BundleDigest)
		fmt.Fprintf(&b, "- Metadata digest (consistency check, not a signature): %s\n", metaDigest)
	}

	if len(rec.Safety) > 0 {
		b.WriteString("\n## Safety\n")
		b.WriteString("The patch touches paths that can execute outside the workspace — review carefully:\n")
		for _, s := range rec.Safety {
			fmt.Fprintf(&b, "- %s\n", Sanitize(s)) // derived from agent-named diff paths (§6.13 invariant)
		}
	}

	if len(rec.Questions) > 0 {
		b.WriteString("\n## Questions\n")
		for _, q := range rec.Questions {
			answer := q.Answer
			if q.AnsweredBy == "timeout" {
				answer = "(no answer — timed out)"
			} else if answer == "" {
				answer = "(unanswered)"
			}
			fmt.Fprintf(&b, "- %s → %s\n", q.Prompt, answer)
		}
	}

	b.WriteString("\n## Notes\n")
	if n := strings.TrimSpace(Sanitize(agentNotes)); n != "" {
		b.WriteString(n)
		b.WriteString("\n")
	} else {
		b.WriteString("(none)\n")
	}

	return os.WriteFile(filepath.Join(runDir, reportName), []byte(b.String()), 0o644)
}

// agentNotes reads any agent-written report.md the guest collected into the run dir, so the
// host can fold it into the Notes section before overwriting the file with the canonical
// report. Absent/unreadable → empty (a normal run where the image wrote no report).
func agentNotes(runDir string) string {
	b, err := os.ReadFile(filepath.Join(runDir, reportName))
	if err != nil {
		return ""
	}
	return string(b)
}

// summarizeTask is the meta.json task_summary: the first 200 characters of the task prompt,
// sanitized and single-lined (§8.4).
func summarizeTask(prompt []byte) string {
	s := Sanitize(strings.Join(strings.Fields(string(prompt)), " "))
	if r := []rune(s); len(r) > 200 {
		return string(r[:200])
	}
	return s
}

// summarizeQuestions turns the persisted Q&A files into meta.json/report.md summaries (§6.13,
// §8.4), sanitizing agent-originated prompt/answer text and deriving who answered and how long
// the run waited.
func summarizeQuestions(runDir string) []QuestionMeta {
	qs, err := ReadQuestions(runDir)
	if err != nil || len(qs) == 0 {
		return nil
	}
	out := make([]QuestionMeta, 0, len(qs))
	for _, q := range qs {
		m := QuestionMeta{ID: q.ID, Prompt: Sanitize(q.Prompt)}
		if q.AnswerAt != "" {
			m.WaitedSecs = durationSecs(q.AskedAt, q.AnswerAt)
			if q.NoAnswer {
				m.AnsweredBy = "timeout"
			} else {
				m.Answer, m.AnsweredBy = Sanitize(q.Response), "human"
			}
		}
		out = append(out, m)
	}
	return out
}

// resultWord maps a finalized record to the report's success|failed|timed out word (§8.4).
func resultWord(rec RunRecord) string {
	switch {
	case rec.TimedOut:
		return "timed out"
	case rec.State == StateDone && rec.ExitCode == 0:
		return "success"
	default:
		return "failed"
	}
}

// hms renders a duration in seconds as e.g. "7m42s" (§8.4).
func hms(secs int) string {
	if secs <= 0 {
		return "0s"
	}
	return (time.Duration(secs) * time.Second).String()
}

// networkLine renders the egress policy for the report, e.g. "allowlist (api.anthropic.com)".
func networkLine(n NetworkMeta) string {
	mode := n.Mode
	if mode == "" {
		mode = "allowlist"
	}
	if len(n.Allow) == 0 {
		return mode
	}
	return fmt.Sprintf("%s (%s)", mode, strings.Join(n.Allow, ", "))
}

// durationSecs returns the whole seconds between two RFC3339 stamps, or 0 if either is missing
// or unparseable (both come from nowStamp, so parse failures are not expected).
func durationSecs(start, end string) int {
	t0, err0 := time.Parse(time.RFC3339, start)
	t1, err1 := time.Parse(time.RFC3339, end)
	if err0 != nil || err1 != nil {
		return 0
	}
	if d := int(t1.Sub(t0).Seconds()); d > 0 {
		return d
	}
	return 0
}

// ansiEscape matches terminal escape sequences (CSI/OSC and a bare ESC) so agent-originated
// text can't inject color/cursor control into the host's terminal when a report is catted or
// a question is displayed (§6.13 "sanitize on display").
var ansiEscape = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-_][0-?]*[ -/]*[@-~]|\x1b`)

// Sanitize strips terminal escape sequences and other C0 control characters (keeping tab and
// newline) from untrusted agent text before it lands in a review artifact or is printed to a
// terminal — used by the report writer and the `krayt questions` command (§6.13, "sanitize on
// display").
func Sanitize(s string) string {
	s = ansiEscape.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
