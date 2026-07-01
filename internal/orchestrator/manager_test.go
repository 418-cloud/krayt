package orchestrator_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/418-cloud/krayt/internal/guest"
	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/protocol/pb"
	"github.com/418-cloud/krayt/internal/provider/fake"
	"github.com/418-cloud/krayt/internal/task"
)

// TestConcurrentRuns is the core Phase 4 proof: N runs execute concurrently through one
// Manager and each produces an isolated changes.patch, log, and terminal state under its own
// .krayt/runs/<id>/ (§6.2, Done-when).
func TestConcurrentRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	img := minimalImage(ctx, t)
	runner := &editingRunner{edits: map[string]string{"greeting.txt": "edited by agent\n"}}
	p := &fake.Provider{Register: func(s *grpc.Server) {
		root, _ := os.MkdirTemp("", "krayt-guest-")
		pb.RegisterGuestAgentServer(s, guest.NewService(guest.WithRunner(runner), guest.WithRoot(root)))
	}}
	stateDir := t.TempDir()
	mgr := orchestrator.NewManager(orchestrator.Deps{Provider: p, Image: img}, stateDir, 0)

	const n = 6
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
				ID: fmt.Sprintf("run_%02d", i), ImageRef: "latest", RepoPath: repos[i],
				BundleDepth: 1, TaskPrompt: []byte("t"),
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

// TestAttachLive proves attach shows live output: FollowLog receives a log line while the run
// is still executing (not only after it finishes), reading the on-disk log like `krayt attach`.
func TestAttachLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	img := minimalImage(ctx, t)
	runner := &slowLogRunner{lines: 5, delay: 120 * time.Millisecond}
	p := &fake.Provider{Register: func(s *grpc.Server) {
		root, _ := os.MkdirTemp("", "krayt-guest-")
		pb.RegisterGuestAgentServer(s, guest.NewService(guest.WithRunner(runner), guest.WithRoot(root)))
	}}
	stateDir := t.TempDir()
	mgr := orchestrator.NewManager(orchestrator.Deps{Provider: p, Image: img}, stateDir, 0)
	src := newRepo(t, map[string]string{"a.txt": "1\n"})

	runDone := make(chan error, 1)
	go func() {
		_, err := mgr.Run(ctx, task.RunSpec{ID: "run_attach", ImageRef: "latest", RepoPath: src, BundleDepth: 1, TaskPrompt: []byte("t")})
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
	for i := 1; i <= 5; i++ {
		if !strings.Contains(buf.String(), fmt.Sprintf("line %d", i)) {
			t.Errorf("attach output missing line %d; got:\n%s", i, buf.String())
		}
	}
}

// TestMaxConcurrency confirms the Manager serializes runs beyond the limit.
func TestMaxConcurrency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	img := minimalImage(ctx, t)
	var mu sync.Mutex
	var inFlight, maxSeen int
	runner := &gateRunner{onRun: func() {
		mu.Lock()
		inFlight++
		if inFlight > maxSeen {
			maxSeen = inFlight
		}
		mu.Unlock()
		time.Sleep(150 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
	}}
	p := &fake.Provider{Register: func(s *grpc.Server) {
		root, _ := os.MkdirTemp("", "krayt-guest-")
		pb.RegisterGuestAgentServer(s, guest.NewService(guest.WithRunner(runner), guest.WithRoot(root)))
	}}
	mgr := orchestrator.NewManager(orchestrator.Deps{Provider: p, Image: img}, t.TempDir(), 1)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			src := newRepo(t, map[string]string{"a.txt": "1\n"})
			_, _ = mgr.Run(ctx, task.RunSpec{ID: fmt.Sprintf("run_%d", i), ImageRef: "latest", RepoPath: src, BundleDepth: 1, TaskPrompt: []byte("t")})
		}(i)
	}
	wg.Wait()
	if maxSeen > 1 {
		t.Errorf("max-concurrency 1 violated: observed %d runners in flight", maxSeen)
	}
}

// --- helper runners + a thread-safe buffer ---

type slowLogRunner struct {
	lines int
	delay time.Duration
}

func (r *slowLogRunner) Version() string { return "fake" }
func (r *slowLogRunner) Run(ctx context.Context, _ guest.RunConfig, log guest.LogFunc) (int, error) {
	for i := 1; i <= r.lines; i++ {
		log(pb.LogLine_STDOUT, []byte(fmt.Sprintf("line %d\n", i)), time.Now().UnixMilli())
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(r.delay):
		}
	}
	return 0, nil
}

type gateRunner struct{ onRun func() }

func (r *gateRunner) Version() string { return "fake" }
func (r *gateRunner) Run(_ context.Context, _ guest.RunConfig, _ guest.LogFunc) (int, error) {
	r.onRun()
	return 0, nil
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
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
