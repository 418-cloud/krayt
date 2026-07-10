package orchestrator_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/418-cloud/krayt/internal/guest"
	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/protocol/pb"
	"github.com/418-cloud/krayt/internal/provider/fake"
	"github.com/418-cloud/krayt/internal/secrets"
	"github.com/418-cloud/krayt/internal/task"
)

const artifactSecret = "sk-ant-supersecret-0123456789"

// secretArtifactRunner is an agent that (carelessly) copies its credential into a tracked
// source file — so it lands in changes.patch — and into its /output/report.md notes.
type secretArtifactRunner struct{ secret string }

func (r *secretArtifactRunner) Version() string { return "fake" }
func (r *secretArtifactRunner) Run(_ context.Context, cfg guest.RunConfig, _ guest.LogFunc) (int, error) {
	if err := os.WriteFile(filepath.Join(cfg.WorkspaceDir, "config.txt"), []byte("api_key="+r.secret+"\n"), 0o644); err != nil {
		return 1, err
	}
	if cfg.OutputDir != "" {
		notes := "Authenticate with the key " + r.secret + " before running.\n"
		if err := os.WriteFile(filepath.Join(cfg.OutputDir, "report.md"), []byte(notes), 0o644); err != nil {
			return 1, err
		}
	}
	return 0, nil
}

// TestSecretRedactedInReportAndFlaggedInPatch proves the §6.8/§10 confinement extension: an
// agent-written report.md is redacted in the guest, while changes.patch (which must stay
// byte-exact for `git apply`) is left intact but flagged in the report's Safety section — with
// the secret VALUE never reaching any host artifact and only the KEY name surfaced.
func TestSecretRedactedInReportAndFlaggedInPatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})
	img := minimalImage(ctx, t)

	secretsFile := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(secretsFile, []byte("ANTHROPIC_API_KEY="+artifactSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	guestRoot := t.TempDir()
	p := &fake.Provider{Register: func(s *grpc.Server) {
		pb.RegisterGuestAgentServer(s, guest.NewService(guest.WithRunner(&secretArtifactRunner{secret: artifactSecret}), guest.WithRoot(guestRoot)))
	}}

	runDir := filepath.Join(t.TempDir(), "run")
	spec := task.RunSpec{
		ID: "run_secret_artifacts", ImageRef: "img@sha256:abc", RepoPath: src, BundleDepth: 1,
		TaskPrompt: []byte("task"), SecretsPath: secretsFile,
	}
	res, err := orchestrator.Run(ctx, orchestrator.Deps{Provider: p, Image: img}, spec, runDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// report.md (the host-folded agent notes) is redacted — value gone, marker present.
	rep := readFile(t, filepath.Join(runDir, "report.md"))
	if strings.Contains(rep, artifactSecret) {
		t.Errorf("secret value leaked into report.md:\n%s", rep)
	}
	if !strings.Contains(rep, secrets.RedactionMarker) {
		t.Errorf("report.md Notes should carry a redaction marker; got:\n%s", rep)
	}

	// changes.patch is deliberately NOT redacted (mutating hunks breaks `git apply`) — it stays
	// byte-exact, so the secret value is present verbatim for the human's manual review.
	patchBytes, err := os.ReadFile(filepath.Join(runDir, "changes.patch"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(patchBytes, []byte(artifactSecret)) {
		t.Errorf("changes.patch should be byte-exact (secret present, not redacted); got:\n%s", patchBytes)
	}

	// The guest's secret-scan.json marker names the KEY only — never the value.
	scan := readFile(t, filepath.Join(runDir, "secret-scan.json"))
	if !strings.Contains(scan, "ANTHROPIC_API_KEY") {
		t.Errorf("secret-scan.json should list the matched key; got: %s", scan)
	}
	if strings.Contains(scan, artifactSecret) {
		t.Errorf("secret-scan.json must never contain the value; got: %s", scan)
	}

	// The host turns that marker into a Safety warning (Result, meta.json, and report.md).
	safetyHit := func(lines []string) bool {
		for _, s := range lines {
			if strings.Contains(s, "ANTHROPIC_API_KEY") && strings.Contains(s, "changes.patch") {
				return true
			}
		}
		return false
	}
	if !safetyHit(res.Safety) {
		t.Errorf("Result.Safety should flag the secret in the patch; got %v", res.Safety)
	}
	if !strings.Contains(rep, "## Safety") || !strings.Contains(rep, "ANTHROPIC_API_KEY") {
		t.Errorf("report.md Safety should flag the secret key; got:\n%s", rep)
	}

	// meta.json carries the Safety warning but never the raw value.
	mb := readFile(t, filepath.Join(runDir, "meta.json"))
	if strings.Contains(mb, artifactSecret) {
		t.Errorf("secret value leaked into meta.json")
	}
	var m orchestrator.RunRecord
	if err := json.Unmarshal([]byte(mb), &m); err != nil {
		t.Fatalf("parse meta.json: %v", err)
	}
	if !safetyHit(m.Safety) {
		t.Errorf("meta.json safety should flag the secret in the patch; got %v", m.Safety)
	}
}

// secretAskingRunner asks the human a question whose prompt and choices embed the credential —
// the leak path through the ask_human bridge.
type secretAskingRunner struct{ secret string }

func (r *secretAskingRunner) Version() string { return "fake" }
func (r *secretAskingRunner) Run(ctx context.Context, cfg guest.RunConfig, _ guest.LogFunc) (int, error) {
	if cfg.Ask == nil {
		return 1, fmt.Errorf("no ask bridge wired")
	}
	answer, _, err := cfg.Ask(ctx, "Use the key "+r.secret+"?", []string{"use " + r.secret, "skip"})
	if err != nil {
		return 1, err
	}
	if err := os.WriteFile(filepath.Join(cfg.WorkspaceDir, "greeting.txt"), []byte(answer+"\n"), 0o644); err != nil {
		return 1, err
	}
	return 0, nil
}

// TestSecretRedactedInQuestion proves the ask_human prompt/choices are redacted in the guest at
// the bridge boundary (§6.8/§6.13), so the value never reaches the persisted questions/<id>.json
// or the meta.json Q&A summary.
func TestSecretRedactedInQuestion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	img := minimalImage(ctx, t)
	secretsFile := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(secretsFile, []byte("ANTHROPIC_API_KEY="+artifactSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr := orchestrator.NewManager(orchestrator.Deps{Provider: askProvider(&secretAskingRunner{secret: artifactSecret}), Image: img}, t.TempDir(), 0)
	stateDir := mgr.StateDir()
	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})

	const id = "run_secret_q"
	runDone := make(chan error, 1)
	go func() {
		_, err := mgr.Run(ctx, task.RunSpec{
			ID: id, ImageRef: "latest", RepoPath: src, BundleDepth: 1, TaskPrompt: []byte("t"),
			SecretsPath: secretsFile,
			Questions:   task.QuestionsPolicy{Mode: task.QuestionWait, Timeout: 30 * time.Second},
		})
		runDone <- err
	}()

	waitState(t, stateDir, id, orchestrator.StateWaiting)
	runDir := orchestrator.RunDir(stateDir, id)
	qs, err := orchestrator.ReadQuestions(runDir)
	if err != nil || len(qs) != 1 {
		t.Fatalf("persisted questions = %+v (err %v)", qs, err)
	}
	// The prompt + choices crossed the bridge, so they must be redacted on disk.
	if strings.Contains(qs[0].Prompt, artifactSecret) {
		t.Errorf("secret leaked into persisted question prompt: %q", qs[0].Prompt)
	}
	if !strings.Contains(qs[0].Prompt, secrets.RedactionMarker) {
		t.Errorf("question prompt should carry a redaction marker; got %q", qs[0].Prompt)
	}
	for _, c := range qs[0].Choices {
		if strings.Contains(c, artifactSecret) {
			t.Errorf("secret leaked into a persisted choice: %q", c)
		}
	}

	if err := mgr.Answer(id, qs[0].ID, "ok", false); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if err := <-runDone; err != nil {
		t.Fatalf("run: %v", err)
	}

	// The meta.json Q&A summary is likewise clean.
	if mb := readFile(t, filepath.Join(runDir, "meta.json")); strings.Contains(mb, artifactSecret) {
		t.Errorf("secret value leaked into meta.json question summary")
	}
}
