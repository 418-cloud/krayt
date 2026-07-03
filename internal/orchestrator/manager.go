package orchestrator

import (
	"context"
	"fmt"
	"sync"

	"github.com/418-cloud/krayt/internal/task"
)

// Manager owns the set of concurrent runs in one process (§6.2): it bounds concurrency and
// tracks active runs. All durable run state lives on disk under stateDir/runs/<id>/, so the
// management commands (ls/attach/stop/answer) work across invocations without going through
// the Manager — that is the daemon-less, process-agnostic model of §6.2. This in-process view
// backs the automated concurrency proof and the same-process foreground run.
type Manager struct {
	deps     Deps
	stateDir string
	sem      chan struct{} // max-concurrency; nil = unbounded

	mu     sync.Mutex
	active map[string]*activeRun
}

// activeRun is the in-process handle for a run this Manager owns: its cancel (for Stop) and,
// once the guest is connected, an answerer for resolving questions (§6.13).
type activeRun struct {
	cancel context.CancelFunc
	answer AnswerFunc
}

// NewManager returns a Manager rooted at stateDir (e.g. <repo>/.krayt). maxConcurrency <= 0
// means unbounded.
func NewManager(deps Deps, stateDir string, maxConcurrency int) *Manager {
	var sem chan struct{}
	if maxConcurrency > 0 {
		sem = make(chan struct{}, maxConcurrency)
	}
	m := &Manager{deps: deps, stateDir: stateDir, active: map[string]*activeRun{}, sem: sem}
	// Publish each run's answerer as its client connects, so Manager.Answer can resolve a
	// waiting run in-process (§6.13).
	m.deps.OnClient = m.registerAnswerer
	return m
}

// registerAnswerer records (or, with nil, clears) a run's answerer.
func (m *Manager) registerAnswerer(runID string, answer AnswerFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ar := m.active[runID]; ar != nil {
		ar.answer = answer
	}
}

// Answer resolves a waiting run's outstanding question in-process (§6.13). It returns an error
// if the run is not owned here (started by another process) or has not connected yet — the CLI
// `krayt answer` handles the cross-process case by dialing the recorded guest socket directly.
func (m *Manager) Answer(runID, questionID, response string, noAnswer bool) error {
	m.mu.Lock()
	ar := m.active[runID]
	var answer AnswerFunc
	if ar != nil {
		answer = ar.answer
	}
	m.mu.Unlock()
	if answer == nil {
		return fmt.Errorf("run %q is not waiting for an answer here", runID)
	}
	return answer(questionID, response, noAnswer)
}

// StateDir returns the manager's state directory.
func (m *Manager) StateDir() string { return m.stateDir }

// Run drives one run to completion under stateDir/runs/<id>/ (§7, §8.4), bounded by
// max-concurrency. It blocks until the run finishes; the deferred teardown guarantees the VM
// is destroyed. Callers run it in a goroutine per run for concurrency.
func (m *Manager) Run(ctx context.Context, spec task.RunSpec) (*Result, error) {
	if m.sem != nil {
		select {
		case m.sem <- struct{}{}:
			defer func() { <-m.sem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.mu.Lock()
	m.active[spec.ID] = &activeRun{cancel: cancel}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.active, spec.ID)
		m.mu.Unlock()
	}()

	return Run(runCtx, m.deps, spec, RunDir(m.stateDir, spec.ID))
}

// Stop cancels an active run this Manager owns, tearing its VM down (via the run's context →
// deferred Destroy). It returns false if the run is not owned here — e.g. one started by
// another process, which `krayt stop` reaches by signalling the recorded PID instead (§6.2).
func (m *Manager) Stop(id string) bool {
	m.mu.Lock()
	ar, ok := m.active[id]
	m.mu.Unlock()
	if ok {
		ar.cancel()
	}
	return ok
}
