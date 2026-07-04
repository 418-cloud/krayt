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

	// The answer must be persisted back into the Q&A history (§6.13), not just the question.
	qs, err = orchestrator.ReadQuestions(runDir)
	if err != nil || len(qs) != 1 {
		t.Fatalf("re-read questions: %+v (err %v)", qs, err)
	}
	if qs[0].Response != "yes" || qs[0].NoAnswer || qs[0].AnswerAt == "" {
		t.Errorf("answer not recorded in history: %+v", qs[0])
	}
}

// chattyAskingRunner asks a question and keeps emitting log lines *while blocked* on the
// answer — reproducing the ask-probe's "question sent — now waiting" line arriving right
// behind the question. Those logs must not be mistaken for the agent resuming.
type chattyAskingRunner struct{ prompt string }

func (r *chattyAskingRunner) Version() string { return "fake" }
func (r *chattyAskingRunner) Run(ctx context.Context, cfg guest.RunConfig, log guest.LogFunc) (int, error) {
	stop := make(chan struct{})
	go func() {
		// small lead so the question reaches the host first, then chatter while blocked
		select {
		case <-time.After(50 * time.Millisecond):
		case <-stop:
			return
		}
		for i := 0; i < 8; i++ {
			log(pb.LogLine_STDOUT, []byte("agent: still waiting for input...\n"), time.Now().UnixMilli())
			select {
			case <-time.After(50 * time.Millisecond):
			case <-stop:
				return
			}
		}
	}()
	answer, _, err := cfg.Ask(ctx, r.prompt, nil)
	close(stop)
	if err != nil {
		return 1, err
	}
	if err := os.WriteFile(filepath.Join(cfg.WorkspaceDir, "greeting.txt"), []byte(answer+"\n"), 0o644); err != nil {
		return 1, err
	}
	return 0, nil
}

// TestQuestionStaysWaitingWhileAgentLogs is the regression for the ask-probe finding: a run
// blocked in ask_human must stay `waiting` even as the agent keeps logging — a log line is not
// a resume signal (§6.13).
func TestQuestionStaysWaitingWhileAgentLogs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	img := minimalImage(ctx, t)
	mgr := orchestrator.NewManager(orchestrator.Deps{Provider: askProvider(&chattyAskingRunner{prompt: "proceed?"}), Image: img}, t.TempDir(), 0)
	stateDir := mgr.StateDir()
	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})

	const id = "run_chatty"
	runDone := make(chan error, 1)
	go func() {
		_, err := mgr.Run(ctx, task.RunSpec{
			ID: id, ImageRef: "latest", RepoPath: src, BundleDepth: 1, TaskPrompt: []byte("t"),
			Questions: task.QuestionsPolicy{Mode: task.QuestionWait, Timeout: 30 * time.Second},
		})
		runDone <- err
	}()

	waitState(t, stateDir, id, orchestrator.StateWaiting)
	runDir := orchestrator.RunDir(stateDir, id)
	// While the agent chatters (~8 lines over ~450ms), the state must stay `waiting`.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if rec, err := orchestrator.ReadRecord(runDir); err == nil && rec.State != orchestrator.StateWaiting {
			t.Fatalf("run flipped to %q while still blocked on input (a log was misread as resume)", rec.State)
		}
		time.Sleep(20 * time.Millisecond)
	}

	qs, _ := orchestrator.ReadQuestions(runDir)
	if len(qs) == 0 {
		t.Fatal("no question was recorded")
	}
	if err := mgr.Answer(id, qs[0].ID, "yes", false); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if err := <-runDone; err != nil {
		t.Fatalf("run: %v", err)
	}
	if rec, _ := orchestrator.ReadRecord(runDir); rec.State != orchestrator.StateDone {
		t.Errorf("final state = %q, want done", rec.State)
	}
}

// resumingRunner asks a question, then keeps "working" after the answer arrives until the test
// releases it — giving an observable `running` window between the answer and completion, so the
// precise waiting→running transition (§6.13) can be asserted.
type resumingRunner struct {
	prompt  string
	resumed chan struct{} // closed once the agent receives its answer
	release chan struct{} // test closes this to let the agent finish
}

func (r *resumingRunner) Version() string { return "fake" }
func (r *resumingRunner) Run(ctx context.Context, cfg guest.RunConfig, _ guest.LogFunc) (int, error) {
	answer, _, err := cfg.Ask(ctx, r.prompt, []string{"yes", "no"})
	if err != nil {
		return 1, err
	}
	close(r.resumed)
	select {
	case <-r.release:
	case <-ctx.Done():
		return 1, ctx.Err()
	}
	if err := os.WriteFile(filepath.Join(cfg.WorkspaceDir, "greeting.txt"), []byte(answer+"\n"), 0o644); err != nil {
		return 1, err
	}
	return 0, nil
}

// TestQuestionResolvedResumes is the Phase 6 proof: answering a waiting question flips the run
// waiting→running *immediately* (via the guest "question resolved" RunEvent), while the agent is
// still working — not held at `waiting` until the run ends (§6.13).
func TestQuestionResolvedResumes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	img := minimalImage(ctx, t)
	runner := &resumingRunner{prompt: "proceed?", resumed: make(chan struct{}), release: make(chan struct{})}
	mgr := orchestrator.NewManager(orchestrator.Deps{Provider: askProvider(runner), Image: img}, t.TempDir(), 0)
	stateDir := mgr.StateDir()
	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})

	const id = "run_resolve"
	runDone := make(chan error, 1)
	go func() {
		_, err := mgr.Run(ctx, task.RunSpec{
			ID: id, ImageRef: "latest", RepoPath: src, BundleDepth: 1, TaskPrompt: []byte("t"),
			Questions: task.QuestionsPolicy{Mode: task.QuestionWait, Timeout: 30 * time.Second},
		})
		runDone <- err
	}()

	waitState(t, stateDir, id, orchestrator.StateWaiting)
	runDir := orchestrator.RunDir(stateDir, id)
	qs, err := orchestrator.ReadQuestions(runDir)
	if err != nil || len(qs) != 1 {
		t.Fatalf("questions = %+v (err %v)", qs, err)
	}

	if err := mgr.Answer(id, qs[0].ID, "yes", false); err != nil {
		t.Fatalf("Answer: %v", err)
	}

	// The reverse edge: the run returns to `running` on the answer, while the agent is still
	// working (not yet released) — i.e. before it terminates.
	waitState(t, stateDir, id, orchestrator.StateRunning)

	close(runner.release)
	if err := <-runDone; err != nil {
		t.Fatalf("run: %v", err)
	}
	if rec, _ := orchestrator.ReadRecord(runDir); rec.State != orchestrator.StateDone {
		t.Errorf("final state = %q, want done", rec.State)
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
