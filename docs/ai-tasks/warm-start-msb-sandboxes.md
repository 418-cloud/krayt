# Task: warm-start msb sandboxes — flat rootfs and image materialization, without weakening ephemerality

**Read `CLAUDE.md`, `docs/adr-microsandbox-sandbox-layer.md`, and `KRAYT_SPEC.md` §2 (Non-Goals),
§6.2, §10 first.** Give a short plan and proceed. Depends on `run-tasks-on-microsandbox.md` and
`retire-vm-image-pipeline.md`. This is **pure performance work with a hard guardrail**; read the
guardrail first.

## The guardrail — read this before anything else

**The ephemeral, per-run VM is krayt's blast-radius control.** It is the reason in-container
hardening is defence in depth rather than the boundary, and the reason a compromised agent cannot
reach the next run. Every optimization below reuses a *build artifact*; **none of them reuse a
sandbox**. Concretely:

- One sandbox per run, created fresh and `rm`'d at the end. No pooling, no `msb start` on a stopped
  sandbox from a previous run, no keeping a "warm" sandbox between runs.
- No snapshot that captures a previous run's guest state. A snapshot of a *pristine, pre-task* image
  is a build artifact; a snapshot taken after an agent has run is a persistence channel between
  runs and is out of scope, permanently.

If an optimization requires relaxing either point, it does not belong in this task — it belongs in a
proposal that argues against §2's ephemerality claim on its own terms.

## Background

Under B1 the boot path is msb's. Two of its features cut cold-start cost without touching the
guardrail:

- **Flat OCI rootfs.** `msb pull <ref> --materialize flat` merges the OCI layers into one reusable
  ext4 base; `msb run --root-disk flat:8G,clone=auto` clones that base into the sandbox as
  `rootfs.raw`, grows the private clone, and attaches it through virtio-blk, so the guest mounts
  ext4 directly with no OverlayFS. `clone=auto` prefers a native copy-on-write clone and falls back
  to a sparse copy — `FICLONE` on Linux, `clonefile` on macOS, block cloning on supported Windows
  volumes. **krayt already depends on exactly this primitive**: the vfkit provider CoW-cloned the
  raw rootfs with `clonefile` (§14 Phase 1), so the performance argument is one krayt has already
  made and verified once.
- **Pre-pull.** The pull is optional — creating the first flat sandbox lazily materializes a missing
  artifact — but doing it once, ahead of the first run, moves a multi-hundred-megabyte download off
  the critical path of a run the user is waiting on.

## Decisions already made (do not re-litigate)

1. **Measure before and after, and put the numbers in the task's status row.** This is the only task
   in the arc with no correctness deliverable, so an unmeasured "optimization" has delivered
   nothing. Report cold-start and warm-start wall-clock for the same run, on the same machine, with
   and without the change. `add-rtk-to-agent-images.md`'s status row is the house standard for
   reporting a measured-but-modest result honestly; match it.
2. **Flat rootfs is opt-in per run, defaulting off until measured.** `--root-disk flat:…` is not the
   built-in default in msb either, and it has real trade-offs: flat v1 uses ext4 and **rejects
   rootfs patches and snapshots** rather than silently changing their semantics. Add it as a
   `sandbox.root_disk` key (or a `--root-disk` flag) with the layered default preserved, flip the
   default only once decision 1's numbers justify it, and say in the config comment that flat and
   snapshots are mutually exclusive.
3. **`krayt image pull` gains `--materialize`**, passed through to `msb pull`. That is the pre-pull
   story and it costs one flag, since `retire-vm-image-pipeline.md` already made `krayt image pull`
   a front-end over `msb pull`.
4. **`clone=auto`, never `clone=reflink`.** `reflink` *requires* native clone support and fails
   clearly when the destination filesystem cannot provide it — correct for a benchmark, wrong for a
   user whose cache lives on a filesystem without it. `auto` degrades to a sparse copy instead of
   failing a run. If the fallback is slow enough to matter, surface which mode was used (msb's
   `doctor` reports whether `clone=auto` resolves to a native reflink) rather than forcing the
   strict mode.
5. **No sandbox reuse, no cross-run snapshots** — the guardrail above. Write it into §6.2 next to
   the run-lifecycle description, so the next person to look at msb's snapshot API finds the
   reasoning rather than the API.

## What to build

- `sandbox.root_disk` in `krayt.yaml` (and the matching `krayt run` flag), threaded into
  `CreateSpec`, defaulting to msb's layered root.
- `krayt image pull --materialize <layered|flat|all>`.
- A `krayt doctor` row surfacing msb's own clone-probe result, so a user on a filesystem without
  native cloning learns it before wondering why warm starts are not warm.
- A short benchmark script under `hack/` that runs the same task twice and reports both timings, so
  decision 1's numbers are reproducible by a reviewer rather than pasted from one session.

## Done when

- `go build ./...`, `go test -race ./...`, `golangci-lint run` green.
- Offline tests assert the rendered argv for each `root_disk` value, and that the default emits no
  `--root-disk` flag at all (so krayt inherits msb's default rather than pinning a choice).
- A test asserts no code path calls `msb start`, `msb snapshot`, or reuses a sandbox name across
  runs — the guardrail as an executable assertion, not a comment.
- **Hardware (`HUMAN_TODO.md`)**: decision 1's measurements, on an Apple-Silicon Mac, for a real
  agent-image run. Report them even if the win is small; a measured non-improvement is a result and
  should end with the flag defaulting off.

## Out of scope

- Sandbox pooling, warm pools, keeping a sandbox alive between runs, or any snapshot of post-task
  guest state — see the guardrail.
- Tuning msb's memory/CPU knobs (`--thp`, `--max-cpus`, memory pooling). Different subject, and one
  where msb's defaults are more likely right than krayt's guesses.
