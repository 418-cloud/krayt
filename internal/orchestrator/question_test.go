package orchestrator_test

import (
	"context"
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
	"github.com/418-cloud/krayt/internal/task"
)

// askingRunner simulates an agent that pauses to ask the human a question via the in-VM
// bridge (RunConfig.Ask), then writes a file reflecting the answer it received.
type askingRunner struct {
	prompt string
}

func (r *askingRunner) Version() string { return "fake" }
func (r *askingRunner) Run(ctx context.Context, cfg guest.RunConfig, log guest.LogFunc) (int, error) {
	log(pb.LogLine_STDOUT, []byte("agent: asking a question\n"), time.Now().UnixMilli())
	if cfg.Ask == nil {
		return 1, fmt.Errorf("no ask bridge wired")
	}
	answer, noAnswer, err := cfg.Ask(ctx, r.prompt, []string{"yes", "no"})
	if err != nil {
		return 1, err
	}
	decision := answer
	if noAnswer {
		decision = "default (no human)"
	}
	log(pb.LogLine_STDOUT, []byte("agent: got decision "+decision+"\n"), time.Now().UnixMilli())
	if err := os.WriteFile(filepath.Join(cfg.WorkspaceDir, "greeting.txt"), []byte(decision+"\n"), 0o644); err != nil {
		return 1, err
	}
	return 0, nil
}

func askProvider(runner guest.Runner) *fake.Provider {
	return &fake.Provider{Register: func(s *grpc.Server) {
		root, _ := os.MkdirTemp("", "krayt-guest-")
		pb.RegisterGuestAgentServer(s, guest.NewService(guest.WithRunner(runner), guest.WithRoot(root)))
	}}
}

func waitState(t *testing.T, stateDir, id, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		rec, err := orchestrator.ReadRecord(orchestrator.RunDir(stateDir, id))
		if err == nil && rec.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never reached state %q (last=%q)", id, want, rec.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestQuestionWaitAnswer is the second half of the Phase 4 Done-when: a stubbed agent question
// drives the run to `waiting`, and Manager.Answer (what `krayt answer` calls) resolves it so
// the run continues to completion with the answer reflected in the patch.
func TestQuestionWaitAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	img := minimalImage(ctx, t)
	mgr := orchestrator.NewManager(orchestrator.Deps{Provider: askProvider(&askingRunner{prompt: "proceed?"}), Image: img}, t.TempDir(), 0)
	stateDir := mgr.StateDir()
	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})

	const id = "run_q"
	runDone := make(chan error, 1)
	go func() {
		_, err := mgr.Run(ctx, task.RunSpec{
			ID: id, ImageRef: "latest", RepoPath: src, BundleDepth: 1, TaskPrompt: []byte("t"),
			Questions: task.QuestionsPolicy{Mode: task.QuestionWait, Timeout: 30 * time.Second},
		})
		runDone <- err
	}()

	// The run must reach `waiting` and persist the question.
	waitState(t, stateDir, id, orchestrator.StateWaiting)
	runDir := orchestrator.RunDir(stateDir, id)
	qs, err := orchestrator.ReadQuestions(runDir)
	if err != nil || len(qs) != 1 || qs[0].Prompt != "proceed?" {
		t.Fatalf("persisted questions = %+v (err %v)", qs, err)
	}

	// Answer it (what `krayt answer <id> <qid> yes` does in-process).
	if err := mgr.Answer(id, qs[0].ID, "yes", false); err != nil {
		t.Fatalf("Answer: %v", err)
	}

	if err := <-runDone; err != nil {
		t.Fatalf("run: %v", err)
	}
	rec, _ := orchestrator.ReadRecord(runDir)
	if rec.State != orchestrator.StateDone {
		t.Errorf("final state = %q, want done", rec.State)
	}
	patch, err := os.ReadFile(filepath.Join(runDir, "changes.patch"))
	if err != nil || !strings.Contains(string(patch), "yes") {
		t.Errorf("patch should reflect the answer 'yes'; got err=%v patch=\n%s", err, patch)
	}
}

// TestQuestionFailModeSentinel confirms the default `fail` mode never blocks: a question is
// sentinel-answered immediately so the agent proceeds autonomously (§6.13).
func TestQuestionFailModeSentinel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	img := minimalImage(ctx, t)
	mgr := orchestrator.NewManager(orchestrator.Deps{Provider: askProvider(&askingRunner{prompt: "proceed?"}), Image: img}, t.TempDir(), 0)
	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})

	// No goroutine answering: fail mode must resolve the question on its own.
	res, err := mgr.Run(ctx, task.RunSpec{
		ID: "run_fail", ImageRef: "latest", RepoPath: src, BundleDepth: 1, TaskPrompt: []byte("t"),
		Questions: task.QuestionsPolicy{Mode: task.QuestionFail},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	patch, rerr := os.ReadFile(res.PatchPath)
	if rerr != nil || !strings.Contains(string(patch), "default (no human)") {
		t.Errorf("fail mode should give the agent a no-answer sentinel; got err=%v patch=\n%s", rerr, patch)
	}
}
