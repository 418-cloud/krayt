# Task: don't mark a run "running" until its code snapshot is actually captured

**Read `CLAUDE.md` and `KRAYT_SPEC.md` (§6.2 orchestrator, §6.7 code transfer & patch generation)
first. Proceed autonomously — this is a self-contained task run inside a krayt sandbox; there is no
interactive human to approve a plan (use the `ask_human` tool only if genuinely blocked).**

## Background

Found while investigating an unrelated run failure on 2026-07-11 (see
`docs/ai-tasks/preflight-host-resources.md`'s Background for that incident — this is a separate,
latent issue noticed along the way, not what caused that failure). The workflow "wait until `krayt
ls` shows a run as `running`, *then* it's safe to modify the host repo (checkout a branch, commit,
rebase)" is a natural assumption — but it isn't actually guaranteed by the current code.

`orchestrator.Run` (`internal/orchestrator/orchestrator.go`) writes `rec.State = StateRunning` to
`meta.json` (`:176-177`) right after the VM boots and the control channel is ready, but **before**
`pushCode` (`:190-195`) — which is the step that actually reads the host repo
(`patch.CreateBundle(ctx, spec.RepoPath, bundle, spec.BundleDepth, spec.IncludeDirty)`, `:306`,
inside `pushCode`, `:294-330`) to build the self-contained snapshot bundle that becomes the agent's
`/workspace` (§6.7). Between those two points, `pushImage` (`:184-189`) also runs — real, sometimes
slow, network-bound work (pulling/streaming the user's container image blobs). So there is a real
window, externally visible as `state: "running"` in `meta.json`/`krayt ls`, during which the host
repo has **not yet** been snapshotted — a `git checkout -b`, commit, or rebase in that window can
still change what the run captures, silently.

## Goal

Move the `state: running` transition to occur only **after** the code bundle has been captured
(`pushCode` has returned successfully), so "running" is externally visible if and only if it's
actually safe to mutate the host repo without affecting this run. No new state name, no new field
— purely a reordering of when the existing transition is written.

## Current behavior (grounding)

`internal/orchestrator/orchestrator.go`, inside `Run`:

- `:84-92` — the record starts life as `StateStarting` (written immediately).
- `:93-109` — a deferred finalizer unconditionally sets the **terminal** state (`StateFailed` /
  `StateTimedOut` / `StateDone`) from `err`/`res` when `Run` returns, regardless of whatever
  intermediate state was last written. This means a failure during `pushImage`/`pushCode` already
  goes straight from whatever the last-written intermediate state was to `StateFailed` — moving
  the running-transition later does not change or need any error-handling logic, only *when* the
  intermediate "running" write happens on the way to a **successful** run.
- `:111-135` — step 1: create + start the VM.
- `:137-150` — step 2: dial the control channel, wait for boot-readiness.
- `:152-175` — record `rec.CtrlSocket`; register the in-process answerer.
- `:176-177` — **the write to move**: `rec.State = StateRunning; _ = writeRecord(runDir, rec)`.
- `:179-189` — step 3a: `pushImage(ctx, client, deps.Image)` — no host-repo access; pulls/streams
  the user's container image.
- `:190-195` — step 3b: `pushCode(ctx, client, spec)` — **this is what reads `spec.RepoPath`**
  (`:294-330`, `patch.CreateBundle` at `:306`). Once this returns successfully, the run's code
  snapshot is fixed; nothing that happens afterward in the host repo can affect it.
- `:196-220` — step 3c/3d/3e: push task prompt, secrets, network policy — none of these touch the
  host repo either.
- `:222-227` — step 4: `streamRun` actually starts the container and runs the agent.

So the minimal, correct fix is: move the two lines at `:176-177` to run immediately after the
`pushCode` error-check block (after `:195`, before `:196`'s `PushTask` call) instead of before
`pushImage` (before `:184`). Nothing else changes — `pushImage` continues to run first exactly as
today; only the point at which `state: running` becomes externally visible shifts to after
`pushCode` instead of before `pushImage`.

## Implement

In `internal/orchestrator/orchestrator.go`:

1. Delete the two lines at `:176-177` (`rec.State = StateRunning` / `_ = writeRecord(runDir, rec)`)
   from their current position (immediately before the `pushImage` call).
2. Insert the same two lines immediately after the `pushCode` call's error-handling block
   (i.e., right after the `if err := pushCode(...); err != nil { ... }` block at `:190-195`,
   before the `client.Agent.PushTask(...)` call at `:196`).
3. Leave every other line, comment, and step-numbering untouched — this is a pure reordering, not
   a rewrite. (If the numbered-comment style ("// 3. Push inputs...") ends up reading oddly once
   the running-write sits in the middle of that block, a one-line comment adjustment is fine — but
   don't restructure the surrounding comments beyond what's needed for accuracy.)

## Tests

Add a regression test to `internal/orchestrator/orchestrator_test.go` (package `orchestrator_test`,
mirroring `TestEndToEndRun`'s fixture style — `newRepo`, `minimalImage`, `fake.Provider`,
`guest.NewService`). The goal: deterministically observe `meta.json`'s state *while* `pushCode` is
in flight, without sleeps/polling/timing races.

`*guest.Service` (returned by `guest.NewService`, `internal/guest/service.go:115`) implements
`pb.GuestAgentServer` as a concrete struct with a `PushCode` method (`service.go:158`) — wrap it to
observe when the guest side of the code-push stream begins:

```go
// interceptingService wraps guest.Service to signal onPushCode synchronously right as the
// PushCode stream starts arriving — the same point in the real flow where spec.RepoPath has
// definitely not yet been fully captured (pushCode is still in progress on the host side too).
type interceptingService struct {
	*guest.Service
	onPushCode func()
}

func (s *interceptingService) PushCode(stream pb.GuestAgent_PushCodeServer) error {
	if s.onPushCode != nil {
		s.onPushCode()
	}
	return s.Service.PushCode(stream)
}
```

Test body:

```go
func TestStateNotRunningUntilCodeCaptured(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := newRepo(t, map[string]string{"a.txt": "1\n"})
	img := minimalImage(ctx, t)
	runner := &editingRunner{edits: map[string]string{"a.txt": "2\n"}}
	guestRoot := t.TempDir()

	runDir := filepath.Join(t.TempDir(), "run")
	var stateAtPushCode string
	svc := &interceptingService{
		Service: guest.NewService(guest.WithRunner(runner), guest.WithRoot(guestRoot)),
		onPushCode: func() {
			rec, err := orchestrator.ReadRecord(runDir)
			if err != nil {
				t.Errorf("ReadRecord during PushCode: %v", err)
				return
			}
			stateAtPushCode = rec.State
		},
	}
	p := &fake.Provider{Register: func(s *grpc.Server) { pb.RegisterGuestAgentServer(s, svc) }}

	spec := task.RunSpec{
		ID: "run_state_order", ImageRef: "latest", RepoPath: src, BundleDepth: 1,
		TaskPrompt: []byte("task"), Resources: task.Resources{CPUs: 2, MemoryMiB: 2048},
	}
	if _, err := orchestrator.Run(ctx, orchestrator.Deps{Provider: p, Image: img}, spec, runDir); err != nil {
		t.Fatalf("orchestrator.Run: %v", err)
	}

	if stateAtPushCode != orchestrator.StateStarting {
		t.Errorf("state during PushCode = %q, want %q (code must be captured before \"running\" is externally visible)",
			stateAtPushCode, orchestrator.StateStarting)
	}
	final, err := orchestrator.ReadRecord(runDir)
	if err != nil {
		t.Fatalf("ReadRecord final: %v", err)
	}
	if final.State != orchestrator.StateDone {
		t.Errorf("final state = %q, want %q", final.State, orchestrator.StateDone)
	}
}
```

(`orchestrator.StateStarting`/`StateDone` are already exported, `state.go:17-23`; if
`ReadRecord`/`RunRecord`/`Run`/`Deps` aren't all exported for `orchestrator_test`'s external test
package to reach, check — `TestEndToEndRun` already uses all of these, so they should already be
available.) Run this test against the **unmodified** code first to confirm it fails (proving it
actually catches the bug — `stateAtPushCode` would be `StateRunning`, not `StateStarting`), then
apply the fix and confirm it passes.

## Docs (required)

- `KRAYT_SPEC.md` §6.2: note precisely what `running` means (the code snapshot is already
  captured — safe to mutate the host repo from this point on) and what it means before that point
  (`starting` — VM is booting / image and code are being transferred; do not assume the repo has
  been read yet).
- `docs/ai-tasks/README.md`: add this task to the top table with a status.

## Verify (offline)

```sh
go build ./...
GOOS=linux GOARCH=arm64 go build ./...
go test -race ./...
golangci-lint run
```

No new dependency — this only touches `internal/orchestrator` and its existing test fixtures.
Runs fully offline (the existing `fake.Provider`/`guest.NewService` harness needs no real VM).

## Done when

- `state: running` in `meta.json` is only ever observed after `pushCode` has returned successfully
  — proven by `TestStateNotRunningUntilCodeCaptured` (confirmed to fail on the pre-fix code, pass
  on the post-fix code).
- All existing orchestrator tests (`TestEndToEndRun`, `TestContainerPolicyReachesRunner`,
  `TestSecretsRedactedInLogs`, `TestRunTimeout`, `TestRunTimeoutDuringSetup`, etc.) still pass
  unmodified — this is a pure reordering, not a behavior change to any of the paths they cover.
- `KRAYT_SPEC.md` §6.2 documents the tightened meaning of `running`.

## Constraints

- Pure reordering of two existing lines — do not introduce a new state, a new `meta.json` field, or
  change the deferred finalizer's terminal-state logic.
- Do not change the relative order of `pushImage` vs `pushCode` themselves — only when the
  running-state write happens relative to them.
- Small, focused diff.
