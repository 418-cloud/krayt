# Task: refuse to start a run the host can't actually afford (`krayt run` preflight)

**Read `CLAUDE.md` and `KRAYT_SPEC.md` (§6.1 resources, §6.2 concurrency, §13 CLI surface) first.
Proceed autonomously — this is a self-contained task run inside a krayt sandbox; there is no
interactive human to approve a plan (use the `ask_human` tool only if genuinely blocked).**

## Background

A real failure on 2026-07-11: two `krayt run`s were started concurrently against the same repo on
a 16GB Mac (2 CPU / 4096 MiB / 20 GiB each, the defaults) — one foreground (`prune-cached-images.md`
task), one `--detach`ed 2m40s later (`shell-completion.md` task). The first run had already
authenticated its agent and was ~11 minutes into `claude -p` when it died with:

```
krayt: orchestrator: stream recv: rpc error: code = Unavailable desc = error reading from server: EOF
```

— a clean gRPC "the guest's connection just went away" error, no application-level failure. The
second run finished successfully with a real patch. At the time, this host had only ~29 GiB free
disk (each run wants a 20 GiB scratch disk — two concurrent runs want 40 GiB) and the two VMs'
4096 MiB memory allocations alone (8 GiB) left little headroom on a 16GB machine once both guests
ran memory-hungry `go build`/`go test -race`/`golangci-lint` simultaneously. The most likely
explanation is the host running out of free RAM or disk mid-run, which macOS/Virtualization.framework
resolves by killing the loser — from the host CLI's perspective that surfaces as this same opaque
EOF, 10+ minutes in, after time was already spent.

`krayt`'s existing `--max-concurrency` (`internal/cli/run.go:108`, default `0` = unbounded) only
caps how many runs **one repo's `.krayt`** may have active at once (`internal/orchestrator/climit.go`)
— it says nothing about whether the **host** actually has the RAM/disk to back that many runs, and
it doesn't see runs started against a *different* repo's `.krayt` at all. This task adds a cheap,
fast preflight check so an under-resourced run fails immediately with a clear, actionable error —
before booting a VM and burning 10+ minutes — instead of dying opaquely partway through.

## Goal

Before `krayt run` boots a VM, compare **live host free RAM and free disk** against what this run
requested (`--memory`, `--disk`) plus a safety margin for the host OS and other processes, and
refuse to start (clear error, no VM created) if it doesn't fit. `--skip-resource-check` bypasses
it for a user who knows better (e.g. plenty of swap, or a deliberately tight run).

Deliberately **not** covered: CPU oversubscription (macOS just time-slices vCPUs — it doesn't crash
under CPU pressure the way it does under memory/disk pressure, so this isn't the failure mode this
task addresses); accounting for other krayt runs' *declared* resource requests (measuring **live**
host free RAM/disk instead is simpler and strictly more correct — it already reflects every other
VM's actual allocation, from any repo, plus any non-krayt memory/disk pressure on the machine, with
no cross-repo bookkeeping needed).

## Current behavior (grounding)

- `internal/cli/run.go` `runRun` (`:115-252`) builds `spec` (`task.RunSpec`, `:167-186`, including
  `spec.Resources` = `task.Resources{CPUs, MemoryMiB, DiskGiB, Timeout}`), then calls `applyAdapter`
  (`:191-193`), then branches on `--detach` (`:195-208`) before `newRunDeps()` (`:211`) boots
  anything. **This is the right insertion point** — after the spec is fully built, before the
  detach fork (so even a detached run fails fast in the *parent*, not silently in the child 10
  minutes later) and before any VM/image work begins.
- `internal/orchestrator/climit.go` `AcquireSlot` is the existing (count-only, per-`.krayt`,
  opt-in via `--max-concurrency`) concurrency guard — unrelated to this task; leave it alone. This
  task's check is host-wide and always-on (opt-out via a flag), not repo-scoped.
- No code in this repo queries host free memory or disk today (`grep -rn "Statfs\|vm_stat"
  internal/` turns up nothing) — this is new.
- `internal/cli/run_darwin.go` / `internal/cli/run_other.go` is the existing pattern for a
  macOS-real / everywhere-else-stub split (`//go:build darwin` / `//go:build !darwin`) — mirror it
  here rather than inventing a different seam.

## Design decisions (already made — do not re-derive)

1. **What's measured, macOS only (v1's only real backend):**
   - **Free disk:** `syscall.Statfs` (stdlib, no new dependency) on the directory backing krayt's
     caches/scratch disks (`os.UserCacheDir()`, e.g. `~/Library/Caches/krayt` — the same root
     `cacheDir`/`acquireUserImage` already use). Free bytes = `stat.Bavail * uint64(stat.Bsize)`
     (darwin's `syscall.Statfs_t` has `Bsize uint32`, `Bavail uint64` — `Bavail`, not `Bfree`: it's
     blocks available to this user, the right number for "can I actually write this much").
   - **Free memory:** shell out to `vm_stat` (a stable macOS system binary; no new dependency, no
     cgo). Parse the page size from its header line (`Mach Virtual Memory Statistics: (page size of
     N bytes)`) and sum the `Pages free`, `Pages inactive`, and `Pages speculative` counts (each
     line is `"<Label>:  <N>."` — strip the trailing `.`) — the standard "readily available without
     swapping" approximation. `available_bytes = (free + inactive + speculative) * page_size`.
2. **Policy:** refuse when `free_mem_mib < want.MemoryMiB + memMarginMiB` **or**
   `free_disk_gib < want.DiskGiB + diskMarginGiB`, where `memMarginMiB = 2048` and
   `diskMarginGiB = 5` (named constants — headroom for the host OS and other processes/runs after
   this run's own allocation, not user-configurable in this task; a fixed, documented default is
   simpler than two more flags, and can be revisited later if it proves wrong in practice).
3. **Escape hatch:** `--skip-resource-check` (bool flag on `run`) bypasses the check entirely. No
   interactive prompt/confirmation — krayt runs are frequently headless/detached/scripted, so a
   prompt wouldn't work uniformly; fail closed with a clear message instead, and let the user
   decide to add the flag.
4. **Non-macOS:** always passes (no-op) — this check is about protecting the real, resource-hungry
   macOS VM backend; `newRunDeps`'s existing "no provider yet" error is what actually gates
   non-macOS today (§14 Phase 7), so this check must not become a *second*, unrelated reason a
   future Linux run fails.
5. **Error message includes the actual numbers** (free vs. needed vs. margin) and mentions
   `--skip-resource-check`, so the failure is immediately actionable without reading source.

## Implement

New file `internal/cli/resources.go` (no build tag — pure logic, OS-agnostic, unit-testable):

```go
package cli

import "fmt"

const (
	memMarginMiB  = 2048 // headroom for host OS + other processes after this run's own allocation
	diskMarginGiB = 5
)

// checkHostResources compares already-measured free host RAM/disk against what a run requests
// plus a fixed safety margin. Pure function — the OS-specific measurement lives elsewhere so this
// stays unit-testable without a real host.
func checkHostResources(freeMemMiB, freeDiskGiB, wantMemMiB, wantDiskGiB uint64) error {
	if freeMemMiB < wantMemMiB+memMarginMiB {
		return fmt.Errorf(
			"insufficient free memory to start this run: %d MiB free, need %d MiB (--memory) + %d MiB (safety margin); "+
				"free up memory, lower --memory, or pass --skip-resource-check to override",
			freeMemMiB, wantMemMiB, memMarginMiB)
	}
	if freeDiskGiB < wantDiskGiB+diskMarginGiB {
		return fmt.Errorf(
			"insufficient free disk to start this run: %d GiB free, need %d GiB (--disk) + %d GiB (safety margin); "+
				"free up disk (see `krayt image prune` if available), lower --disk, or pass --skip-resource-check to override",
			freeDiskGiB, wantDiskGiB, diskMarginGiB)
	}
	return nil
}
```

New file `internal/cli/resources_darwin.go` (`//go:build darwin`):

```go
//go:build darwin

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// hostFreeResources measures live free RAM (MiB) and free disk (GiB) on the volume backing
// krayt's caches (os.UserCacheDir()), for the run-start preflight (checkHostResources).
func hostFreeResources() (freeMemMiB, freeDiskGiB uint64, err error) {
	freeMemMiB, err = freeMemoryMiB()
	if err != nil {
		return 0, 0, err
	}
	freeDiskGiB, err = freeDiskGiBAt(os.UserCacheDir)
	if err != nil {
		return 0, 0, err
	}
	return freeMemMiB, freeDiskGiB, nil
}

var vmStatPageLine = regexp.MustCompile(`page size of (\d+) bytes`)
var vmStatCountLine = regexp.MustCompile(`^(Pages free|Pages inactive|Pages speculative):\s+(\d+)\.`)

// freeMemoryMiB shells out to vm_stat (stable macOS system binary, no cgo/new dependency) and
// approximates "readily available without swapping" as free + inactive + speculative pages.
func freeMemoryMiB() (uint64, error) {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, fmt.Errorf("cli: vm_stat: %w", err)
	}
	lines := strings.Split(string(out), "\n")
	var pageSize, pages uint64
	for _, l := range lines {
		if m := vmStatPageLine.FindStringSubmatch(l); m != nil {
			pageSize, _ = strconv.ParseUint(m[1], 10, 64)
		}
		if m := vmStatCountLine.FindStringSubmatch(l); m != nil {
			n, _ := strconv.ParseUint(m[2], 10, 64)
			pages += n
		}
	}
	if pageSize == 0 {
		return 0, fmt.Errorf("cli: vm_stat: could not parse page size")
	}
	return pages * pageSize / (1024 * 1024), nil
}

// freeDiskGiBAt reports free disk (GiB available to this user) on the volume containing dirFn's
// directory. dirFn is os.UserCacheDir, injected so a test can point at t.TempDir() instead.
func freeDiskGiBAt(dirFn func() (string, error)) (uint64, error) {
	dir, err := dirFn()
	if err != nil {
		return 0, fmt.Errorf("cli: resolve cache dir: %w", err)
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, fmt.Errorf("cli: statfs %s: %w", dir, err)
	}
	return stat.Bavail * uint64(stat.Bsize) / (1024 * 1024 * 1024), nil
}
```

New file `internal/cli/resources_other.go` (`//go:build !darwin`):

```go
//go:build !darwin

package cli

// hostFreeResources is a no-op off macOS today — there is no real VM backend to protect yet
// (§14 Phase 7), and this check must not become a second, unrelated reason a future Linux run
// fails. Returns very large values so checkHostResources always passes.
func hostFreeResources() (freeMemMiB, freeDiskGiB uint64, err error) {
	return 1 << 32, 1 << 32, nil
}
```

Wire into `internal/cli/run.go`:
- Add `skipResourceCheck bool` to `runFlags` (near `detach`, `:62`) and register it in
  `bindRunFlags` (`:88-113`): `fl.BoolVar(&f.skipResourceCheck, "skip-resource-check", false,
  "skip the host free-RAM/disk preflight check before booting the VM")`.
- In `runRun`, right after `applyAdapter` (`:191-193`) and before the `--detach` branch (`:195`):
  ```go
  if !f.skipResourceCheck {
  	freeMemMiB, freeDiskGiB, err := hostFreeResources()
  	if err != nil {
  		return fmt.Errorf("resource preflight: %w", err)
  	}
  	if err := checkHostResources(freeMemMiB, freeDiskGiB, spec.Resources.MemoryMiB, spec.Resources.DiskGiB); err != nil {
  		return err
  	}
  }
  ```

## Tests

- `internal/cli/resources_test.go` (no build tag): table-test `checkHostResources` — plenty of
  both → nil; short on memory only → memory error mentioning the actual numbers; short on disk
  only → disk error; short on both → returns the memory error first (document that ordering, don't
  leave it unspecified); exactly at the margin boundary (`free == want+margin`) → passes (not an
  off-by-one failure).
- `internal/cli/resources_darwin_test.go` (`//go:build darwin`): this machine has real `vm_stat`
  and a real filesystem, so these are *smoke* tests, not fixed-value tests — `freeMemoryMiB()`
  returns `> 0, nil`; `freeDiskGiBAt(func() (string, error) { return t.TempDir(), nil })` returns
  `> 0, nil` for a real temp dir; a `dirFn` that errors propagates the error.
- `internal/cli` (extend `run_test.go`'s existing flag/config-precedence test style): with
  `--skip-resource-check` unset and a *huge* `--memory`/`--disk` (e.g. `--memory 999999999`), the
  preflight (once wired) returns a clear error before any provider/image code runs — this can be
  asserted without a real VM by checking the error message and that `newRunDeps`/`acquireUserImage`
  were never reached (e.g. via whatever seam `run_test.go` already uses to stub those out; if none
  exists, asserting the specific returned error is sufficient — don't invent a new mocking seam
  just for this). With `--skip-resource-check` set, the same huge request does **not** get rejected
  by this check (it may still fail later for other reasons — that's fine, just confirm this
  specific check was bypassed).

## Docs (required)

- `KRAYT_SPEC.md` §6.1: document the preflight (what's checked, the fixed margins, macOS-only,
  `--skip-resource-check`).
- `KRAYT_SPEC.md` §13 CLI surface: add `--skip-resource-check` to the `krayt run` flag list.
- `README.md`: a one-line mention near "Running an agent" — concurrent runs are checked against
  live host free RAM/disk before boot.
- `docs/ai-tasks/README.md`: add this task to the top table with a status.

## Verify (offline)

```sh
go build ./...
GOOS=linux GOARCH=arm64 go build ./...
go test -race ./...
golangci-lint run
```

No new dependency — `syscall` (stdlib) and shelling out to `vm_stat` (already on every macOS host).
Runs fully offline; the darwin-only tests only need a real filesystem and the real `vm_stat`
binary, both already present in this sandbox.

## Done when

- `krayt run` on macOS refuses to boot a VM when live free RAM or disk (minus the fixed margin)
  can't cover `--memory`/`--disk`, with an error stating the actual numbers and the
  `--skip-resource-check` override.
- `--skip-resource-check` bypasses the check entirely.
- The check is a no-op on non-macOS builds (doesn't become a new reason Linux runs fail).
- `checkHostResources` is fully unit-tested offline; the darwin measurement functions have smoke
  tests; `go build`/`go test -race`/`golangci-lint run` pass for both host and `linux/arm64` guest
  target.

## Constraints

- Host-side CLI only — no protobuf, no guest, no VM-image rebuild.
- No new dependency: stdlib `syscall`/`os/exec`/`regexp` plus the already-present `vm_stat` binary.
- Do not touch `internal/orchestrator/climit.go`'s `--max-concurrency` — this is a separate,
  additive check, not a replacement.
- Keep the margins as fixed named constants in this task; don't add `--min-free-memory`-style
  flags — that's scope creep past what's needed here.
- Small, focused diff.
