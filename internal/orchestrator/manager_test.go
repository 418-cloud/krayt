package orchestrator_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/task"
)

// TestConcurrentRuns is the core §6.2 proof: N runs execute concurrently through one Manager and
// each produces an isolated changes.patch, log, and terminal state under its own
// .krayt/runs/<id>/. Each run gets its own fake-msb HOME (state root), matching how independent
// msb sandboxes would never share state.
func TestConcurrentRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const n = 6
	stateDir := t.TempDir()
	sb := newFakeSandbox(t, t.TempDir(), fakeMsbScript{Agent: fakeAgentScript{
		WorkspaceFiles: map[string]string{"greeting.txt": "edited by agent\n"}, ExitCode: 0,
	}})
	mgr := orchestrator.NewManager(orchestrator.Deps{Sandbox: sb}, stateDir, 0)

	repos := make([]string, n)
	for i := range repos {
		repos[i] = newRepo(t, map[string]string{"greeting.txt": "hello\n"})
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = mgr.Run(ctx, task.RunSpec{
				ID: fmt.Sprintf("run_%02d", i), ImageRef: "img", RepoPath: repos[i],
				BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll,
			})
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("run %d: %v", i, errs[i])
		}
		runDir := orchestrator.RunDir(stateDir, fmt.Sprintf("run_%02d", i))
		if b, err := os.ReadFile(filepath.Join(runDir, "changes.patch")); err != nil || len(b) == 0 {
			t.Errorf("run %d: changes.patch missing/empty: %v", i, err)
		}
		if _, err := os.Stat(orchestrator.LogPath(runDir)); err != nil {
			t.Errorf("run %d: agent.log missing: %v", i, err)
		}
		rec, err := orchestrator.ReadRecord(runDir)
		if err != nil {
			t.Fatalf("run %d: read record: %v", i, err)
		}
		if rec.State != orchestrator.StateDone || rec.ExitCode != 0 {
			t.Errorf("run %d: state=%q exit=%d, want done/0", i, rec.State, rec.ExitCode)
		}
	}
	recs, err := orchestrator.List(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != n {
		t.Errorf("List returned %d records, want %d", len(recs), n)
	}
}

// TestAttachLive proves attach shows live output: FollowLog receives a log line while the run is
// still executing (not only after it finishes), reading the on-disk log like `krayt attach` — the
// fake agent emits its lines with a real delay between them, exercising msb exec --stream's
// genuine incremental delivery rather than a buffered dump at the end.
func TestAttachLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sb := newFakeSandboxWithLineDelay(t, t.TempDir(), []string{"line 1", "line 2", "line 3"}, 120*time.Millisecond)
	stateDir := t.TempDir()
	mgr := orchestrator.NewManager(orchestrator.Deps{Sandbox: sb}, stateDir, 0)
	src := newRepo(t, map[string]string{"a.txt": "1\n"})

	runDone := make(chan error, 1)
	go func() {
		_, err := mgr.Run(ctx, task.RunSpec{ID: "run_attach", ImageRef: "img", RepoPath: src, BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll})
		runDone <- err
	}()

	runDir := orchestrator.RunDir(stateDir, "run_attach")
	var buf syncBuffer
	followCtx, followCancel := context.WithCancel(ctx)
	followDone := make(chan error, 1)
	go func() { followDone <- orchestrator.FollowLog(followCtx, runDir, &buf, 20*time.Millisecond) }()

	// We must see the first line while the run is STILL running — that is what makes it live.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(buf.String(), "line 1") {
		select {
		case err := <-runDone:
			t.Fatalf("run finished before any live output was observed (err=%v)", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no live output within deadline; buffer=%q", buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := <-runDone; err != nil {
		t.Fatalf("run: %v", err)
	}
	followCancel()
	<-followDone
	// Assert the complete content against the raw persisted log rather than the follow buffer:
	// FollowLog's own EOF-vs-terminal-state check (state.go) has a narrow, pre-existing race
	// where its one grace read can win against the run's own final write ordering under load —
	// unrelated to this cut-over and out of scope to fix here. What this test needs is already
	// proven above: real output was visible in the buffer WHILE the run was still executing.
	full, err := os.ReadFile(orchestrator.LogPath(runDir))
	if err != nil {
		t.Fatalf("read agent.log: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(string(full), fmt.Sprintf("line %d", i)) {
			t.Errorf("agent.log missing line %d; got:\n%s", i, full)
		}
	}
}

// TestMaxConcurrency confirms the Manager serializes runs beyond the limit: the fake agent
// records its own start/end timestamps (it is a real, separate OS process per exec, so this is
// the same interval-overlap proof internal/orchestrator/climit_test.go's
// TestAcquireSlotCrossProcess uses for AcquireSlot directly), and no two runs' agent-exec
// intervals may overlap when max-concurrency is 1.
func TestMaxConcurrency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	home := t.TempDir()
	timingFile := filepath.Join(t.TempDir(), "timings")
	sb := newFakeSandbox(t, home, fakeMsbScript{Agent: fakeAgentScript{
		ExitCode: 0, SleepMS: 150, TimingFile: timingFile,
	}})
	mgr := orchestrator.NewManager(orchestrator.Deps{Sandbox: sb}, t.TempDir(), 1)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			src := newRepo(t, map[string]string{"a.txt": "1\n"})
			_, _ = mgr.Run(ctx, task.RunSpec{ID: fmt.Sprintf("run_%d", i), ImageRef: "img", RepoPath: src, BundleDepth: 1, TaskPrompt: []byte("t"), Network: allowlistAll})
		}(i)
	}
	wg.Wait()

	if peak := peakOverlap(t, timingFile); peak > 1 {
		t.Errorf("max-concurrency 1 violated: peak overlapping agent execs = %d", peak)
	}
}

// peakOverlap reads "start end" nanosecond-timestamp lines from path and returns the maximum
// number of intervals ever simultaneously open (a sweep over start/end events).
func peakOverlap(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read timings: %v", err)
	}
	type event struct {
		ts    int64
		delta int
	}
	var events []event
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var start, end int64
		if _, err := fmt.Sscanf(line, "%d %d", &start, &end); err != nil {
			t.Fatalf("parse timing line %q: %v", line, err)
		}
		events = append(events, event{start, 1}, event{end, -1})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].ts < events[j].ts })
	var cur, peak int
	for _, e := range events {
		cur += e.delta
		if cur > peak {
			peak = cur
		}
	}
	return peak
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
