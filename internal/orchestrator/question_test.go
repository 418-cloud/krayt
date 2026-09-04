package orchestrator_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/task"
)

func waitState(t *testing.T, stateDir, id, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		rec, err := orchestrator.ReadRecord(orchestrator.RunDir(stateDir, id))
		if err == nil && rec.State == want {
			return
		}
		if time.Now().After(deadline) {
			// Include the record's own error: a bare last="failed" says nothing about why, which
			// is the difference between a one-line diagnosis and re-instrumenting the test.
			t.Fatalf("run %s never reached state %q (last=%q, err=%q, read err=%v)", id, want, rec.State, rec.Error, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The question tests deliberately use plain t.TempDir(), which on macOS roots at $TMPDIR — ~49
// bytes of /var/folders/<…>/T/ before the test's own name is added. That is exactly the shape
// that used to overflow the 104-byte sockaddr_un limit once the orchestrator appended
// runs/<id>/ask/ask.sock, and it is now the shape that exercises runSocketDir's fallback to a
// short hardened root. These tests previously mkdtemp'd under /tmp to dodge the overflow; that
// workaround was hiding the defect rather than testing around it — a real `krayt run` from a
// scratch repo under $TMPDIR hit the same wall and every --on-question=wait run died on
// "bind: invalid argument". Keeping the long path here is the point.

// TestQuestionWaitAnswer is the ask_human round-trip proof (§6.13): a scripted agent asks a
// question over the real vsock-route unix socket, driving the run to `waiting`; Manager.Answer
// (what `krayt answer` calls) resolves it so the run continues to completion with the answer
// reflected in the patch.
func TestQuestionWaitAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sb := newFakeSandbox(t, t.TempDir(), fakeMsbScript{Agent: fakeAgentScript{
		Ask:      &fakeAskScript{Prompt: "proceed?", Choices: []string{"yes", "no"}, AnswerFile: "greeting.txt"},
		ExitCode: 0,
	}})
	const id = "run_q"
	mgr := orchestrator.NewManager(orchestrator.Deps{Sandbox: sb}, t.TempDir(), 0)
	stateDir := mgr.StateDir()
	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})

	runDone := make(chan error, 1)
	go func() {
		_, err := mgr.Run(ctx, task.RunSpec{
			ID: id, ImageRef: "img", RepoPath: src, BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
			Questions: task.QuestionsPolicy{Mode: task.QuestionWait, Timeout: 30 * time.Second},
		})
		runDone <- err
	}()

	waitState(t, stateDir, id, orchestrator.StateWaiting)
	runDir := orchestrator.RunDir(stateDir, id)
	qs, err := orchestrator.ReadQuestions(runDir)
	if err != nil || len(qs) != 1 || qs[0].Prompt != "proceed?" {
		t.Fatalf("persisted questions = %+v (err %v)", qs, err)
	}

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
	patchBytes, err := os.ReadFile(filepath.Join(runDir, "changes.patch"))
	if err != nil || !strings.Contains(string(patchBytes), "yes") {
		t.Errorf("patch should reflect the answer 'yes'; got err=%v patch=\n%s", err, patchBytes)
	}

	qs, err = orchestrator.ReadQuestions(runDir)
	if err != nil || len(qs) != 1 {
		t.Fatalf("re-read questions: %+v (err %v)", qs, err)
	}
	if qs[0].Response != "yes" || qs[0].NoAnswer || qs[0].AnswerAt == "" {
		t.Errorf("answer not recorded in history: %+v", qs[0])
	}
}

// TestQuestionFailModeSentinel confirms the default `fail` mode never blocks: no --vsock route
// is emitted at all, so the agent's ask dial fails and it must fall back on its own — there is
// no separate host-side "sentinel this question" branch to maintain under msb.
func TestQuestionFailModeSentinel(t *testing.T) {
	sb := newFakeSandbox(t, t.TempDir(), fakeMsbScript{Agent: fakeAgentScript{
		Ask:      &fakeAskScript{Prompt: "proceed?", AnswerFile: "greeting.txt"},
		ExitCode: 0,
	}})
	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})

	res, err := orchestrator.Run(context.Background(), orchestrator.Deps{Sandbox: sb}, task.RunSpec{
		ID: "run_fail", ImageRef: "img", RepoPath: src, BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
		Questions: task.QuestionsPolicy{Mode: task.QuestionFail},
	}, filepath.Join(t.TempDir(), "run"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	patchBytes, rerr := os.ReadFile(res.PatchPath)
	if rerr != nil || !strings.Contains(string(patchBytes), "NO_HUMAN_ANSWER") {
		t.Errorf("fail mode should leave the agent with no human answer; got err=%v patch=\n%s", rerr, patchBytes)
	}
}

// TestQuestionTimeoutSentinel proves a question left unanswered past its wait limit gets the
// no-answer sentinel recorded into its history automatically (§6.13's default `sentinel` policy).
func TestQuestionTimeoutSentinel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sb := newFakeSandbox(t, t.TempDir(), fakeMsbScript{Agent: fakeAgentScript{
		Ask:      &fakeAskScript{Prompt: "proceed?", AnswerFile: "greeting.txt"},
		ExitCode: 0,
	}})
	const id = "run_qtimeout"
	mgr := orchestrator.NewManager(orchestrator.Deps{Sandbox: sb}, t.TempDir(), 0)
	stateDir := mgr.StateDir()
	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})

	runDone := make(chan error, 1)
	go func() {
		_, err := mgr.Run(ctx, task.RunSpec{
			ID: id, ImageRef: "img", RepoPath: src, BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
			Questions: task.QuestionsPolicy{Mode: task.QuestionWait, Timeout: 200 * time.Millisecond, OnTimeout: task.OnTimeoutSentinel},
		})
		runDone <- err
	}()

	waitState(t, stateDir, id, orchestrator.StateWaiting)
	if err := <-runDone; err != nil {
		t.Fatalf("run: %v", err)
	}
	runDir := orchestrator.RunDir(stateDir, id)
	qs, err := orchestrator.ReadQuestions(runDir)
	if err != nil || len(qs) != 1 {
		t.Fatalf("questions = %+v (err %v)", qs, err)
	}
	if !qs[0].NoAnswer {
		t.Errorf("timed-out question should be recorded as no-answer; got %+v", qs[0])
	}
}

// TestQuestionResolvedResumesWhileAgentStillWorking is the state-transition proof: answering a
// waiting question flips the run waiting→running immediately (via Bridge.OnResolved), while the
// agent is still working — not held at `waiting` until the run ends (§6.13). There is no log
// line for the host to misread as a resume signal under this design at all: resumption comes
// only from the bridge callback, never from stream content.
func TestQuestionResolvedResumesWhileAgentStillWorking(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sb := newFakeSandbox(t, t.TempDir(), fakeMsbScript{Agent: fakeAgentScript{
		Ask:      &fakeAskScript{Prompt: "proceed?", Choices: []string{"yes", "no"}, AnswerFile: "greeting.txt", PostAskSleepMS: 300},
		ExitCode: 0,
	}})
	const id = "run_resolve"
	mgr := orchestrator.NewManager(orchestrator.Deps{Sandbox: sb}, t.TempDir(), 0)
	stateDir := mgr.StateDir()
	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})

	runDone := make(chan error, 1)
	go func() {
		_, err := mgr.Run(ctx, task.RunSpec{
			ID: id, ImageRef: "img", RepoPath: src, BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
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
	// sleeping (PostAskSleepMS) — i.e. well before it terminates.
	waitState(t, stateDir, id, orchestrator.StateRunning)

	if err := <-runDone; err != nil {
		t.Fatalf("run: %v", err)
	}
	if rec, _ := orchestrator.ReadRecord(runDir); rec.State != orchestrator.StateDone {
		t.Errorf("final state = %q, want done", rec.State)
	}
}

// TestQuestionTimeoutAbort proves the `abort` on-timeout policy fails the whole run rather than
// letting the agent proceed on a sentinel (§6.13).
func TestQuestionTimeoutAbort(t *testing.T) {
	sb := newFakeSandbox(t, t.TempDir(), fakeMsbScript{Agent: fakeAgentScript{
		Ask:      &fakeAskScript{Prompt: "proceed?", AnswerFile: "greeting.txt"},
		ExitCode: 0,
	}})
	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})

	_, err := orchestrator.Run(context.Background(), orchestrator.Deps{Sandbox: sb}, task.RunSpec{
		ID: "run_qabort", ImageRef: "img", RepoPath: src, BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
		Questions: task.QuestionsPolicy{Mode: task.QuestionWait, Timeout: 200 * time.Millisecond, OnTimeout: task.OnTimeoutAbort},
	}, filepath.Join(t.TempDir(), "run"))
	if err == nil {
		t.Fatal("expected the run to fail under the abort on-timeout policy")
	}
}
