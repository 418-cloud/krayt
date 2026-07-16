package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/opencontainers/go-digest"
)

// Run lifecycle states persisted in meta.json (§6.2). `waiting` is set while an agent
// question is outstanding (§6.13).
const (
	StateStarting = "starting"
	StateRunning  = "running"
	StateWaiting  = "waiting"
	StateDone     = "done"
	StateFailed   = "failed"
	StateTimedOut = "timed_out"
)

// RunRecord is the on-disk record of a run at `.krayt/runs/<id>/meta.json` — the source of
// truth every management command reads, so runs are observable without any in-process handle
// or daemon (§6.2, §8.4). It is the full §8.4 schema (task summary, network, resources, patch
// stats, questions) plus the operational fields the daemon-less model needs (state, pid,
// control socket) that the review schema omits.
type RunRecord struct {
	ID           string          `json:"id"`
	ImageRef     string          `json:"image_ref"`
	RepoPath     string          `json:"repo_path,omitempty"`
	TaskSummary  string          `json:"task_summary,omitempty"`
	Network      NetworkMeta     `json:"network"`
	Resources    ResourceMeta    `json:"resources"`
	QuestionMode string          `json:"questions_mode,omitempty"`
	State        string          `json:"state"`
	StartedAt    string          `json:"started_at,omitempty"`
	EndedAt      string          `json:"ended_at,omitempty"`
	DurationSecs int             `json:"duration_secs,omitempty"`
	ExitCode     int             `json:"exit_code"`
	TimedOut     bool            `json:"timed_out"`
	Patch        *PatchMeta      `json:"patch,omitempty"`      // nil until a changes.patch is collected
	Provenance   *ProvenanceMeta `json:"provenance,omitempty"` // nil until the code bundle is built + streamed (§6.7)
	Questions    []QuestionMeta  `json:"questions,omitempty"`
	Safety       []string        `json:"safety,omitempty"` // patch-lint findings (§14 Phase 5)
	Error        string          `json:"error,omitempty"`
	PID          int             `json:"pid,omitempty"`         // supervising process (for `krayt stop`)
	CtrlSocket   string          `json:"ctrl_socket,omitempty"` // guest control socket (for `krayt answer`, §6.13)
}

// NetworkMeta is the run's egress policy as recorded in meta.json (§8.4).
type NetworkMeta struct {
	Mode  string   `json:"mode"`
	Allow []string `json:"allow,omitempty"`
}

// ResourceMeta is the run's resource budget as recorded in meta.json (§8.4).
type ResourceMeta struct {
	CPUs        int    `json:"cpus,omitempty"`
	MemoryMiB   uint64 `json:"memory_mib,omitempty"`
	DiskGiB     uint64 `json:"disk_gib,omitempty"`
	TimeoutSecs int    `json:"timeout_secs,omitempty"`
}

// PatchMeta is the changes.patch diffstat as recorded in meta.json (§8.4).
type PatchMeta struct {
	Path         string `json:"path"`
	FilesChanged int    `json:"files_changed"`
	Insertions   int    `json:"insertions"`
	Deletions    int    `json:"deletions"`
}

// ProvenanceMeta records what source a run was based on (§6.7, §8.4). HeadSHA is the real,
// checkoutable `git rev-parse HEAD` at bundle time; BundleSHA is the commit actually imported as
// the guest's krayt-baseline and diffed against for changes.patch — equal to HeadSHA only in the
// full-history/no-dirty case, synthetic otherwise. BundleDepth/IncludeDirty are the request flags
// that determine whether that equality is expected, so a reader can tell a fidelity gap from a bug.
// BundleDigest is a digest of the actual bundle bytes streamed to the guest.
type ProvenanceMeta struct {
	HeadSHA      string `json:"head_sha,omitempty"`
	BundleSHA    string `json:"bundle_sha"`
	BundleDepth  int    `json:"bundle_depth"`
	IncludeDirty bool   `json:"include_dirty,omitempty"`
	BundleDigest string `json:"bundle_digest"`
}

// QuestionMeta is one agent→human Q&A pair summarized for meta.json / report.md (§6.13, §8.4).
// The prompt/answer are sanitized (agent-originated text) before landing here.
type QuestionMeta struct {
	ID         string `json:"id"`
	Prompt     string `json:"prompt"`
	Answer     string `json:"answer,omitempty"`
	AnsweredBy string `json:"answered_by,omitempty"` // human | timeout
	WaitedSecs int    `json:"waited_secs,omitempty"`
}

// Terminal reports whether the run has finished.
func (r RunRecord) Terminal() bool {
	return r.State == StateDone || r.State == StateFailed || r.State == StateTimedOut
}

// runsDir is `<stateDir>/runs`.
func runsDir(stateDir string) string { return filepath.Join(stateDir, "runs") }

// RunDir is the directory for a run under stateDir (e.g. <repo>/.krayt/runs/<id>).
func RunDir(stateDir, id string) string { return filepath.Join(runsDir(stateDir), id) }

// metaPath is the meta.json path for a run dir.
func metaPath(runDir string) string { return filepath.Join(runDir, "meta.json") }

// writeRecord atomically writes a run record to runDir/meta.json (write-temp-then-rename so
// a concurrent `ls`/`answer` never sees a half-written file). It returns a digest of the exact
// bytes written, so the report writer can surface a meta.json consistency check (§8.4) without a
// second read-and-rehash that could drift from what's actually on disk.
func writeRecord(runDir string, rec RunRecord) (digest.Digest, error) {
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return "", fmt.Errorf("orchestrator: create run dir: %w", err)
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("orchestrator: marshal record: %w", err)
	}
	tmp := metaPath(runDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return "", fmt.Errorf("orchestrator: write record: %w", err)
	}
	if err := os.Rename(tmp, metaPath(runDir)); err != nil {
		return "", fmt.Errorf("orchestrator: commit record: %w", err)
	}
	return digest.FromBytes(b), nil
}

// ReadRecord reads a run's meta.json.
func ReadRecord(runDir string) (RunRecord, error) {
	var rec RunRecord
	b, err := os.ReadFile(metaPath(runDir))
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return rec, fmt.Errorf("orchestrator: parse %s: %w", metaPath(runDir), err)
	}
	return rec, nil
}

// List returns every run's record under stateDir, newest first, skipping unreadable dirs.
func List(stateDir string) ([]RunRecord, error) {
	entries, err := os.ReadDir(runsDir(stateDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("orchestrator: list runs: %w", err)
	}
	var recs []RunRecord
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rec, err := ReadRecord(RunDir(stateDir, e.Name()))
		if err != nil {
			continue // a run dir mid-creation or hand-removed; skip rather than fail `ls`
		}
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].StartedAt > recs[j].StartedAt })
	return recs, nil
}

// LogPath is the persisted container-log path for a run dir.
func LogPath(runDir string) string { return filepath.Join(runDir, "logs", "agent.log") }

// ConsoleLogPath is the persisted guest serial-console log path for a run dir — the
// guest-agent's own stdout/stderr (and anything it execs, e.g. proxyd), as opposed to
// LogPath's container-only stdout/stderr. Populated best-effort by Run before it tears the VM
// down (provider.VM.LogPaths); may not exist if the provider had nothing to offer (e.g. the
// fake provider, or a VM that never got far enough to boot).
func ConsoleLogPath(runDir string) string { return filepath.Join(runDir, "logs", "console.log") }

// FollowLog tails runDir/logs/agent.log, writing new bytes to w as they appear, until the
// run reaches a terminal state (log then drained) or ctx is canceled. Because it reads the
// on-disk log, `krayt attach` works for a run supervised by any process (§6.2). poll bounds
// how often it re-checks for new data.
func FollowLog(ctx context.Context, runDir string, w io.Writer, poll time.Duration) error {
	f, err := waitOpen(ctx, LogPath(runDir), poll)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			continue
		}
		if err != nil && err != io.EOF {
			return err
		}
		// Caught up to EOF: stop once the run is terminal (after one grace read to catch a
		// final line written just before the terminal-state write), else wait for more.
		if rec, rerr := ReadRecord(runDir); rerr == nil && rec.Terminal() {
			if n2, _ := f.Read(buf); n2 > 0 {
				_, _ = w.Write(buf[:n2])
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// waitOpen opens path, retrying until it exists or ctx is canceled (the log file appears a
// moment after the run starts).
func waitOpen(ctx context.Context, path string, poll time.Duration) (*os.File, error) {
	for {
		f, err := os.Open(path)
		if err == nil {
			return f, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// nowStamp is the timestamp format used in records.
func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }
