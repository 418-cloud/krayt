# krayt — working agreement

This repo implements `krayt`, specified in full in **`KRAYT_SPEC.md`**. Read the spec before
working. This file is the standing agreement for how to build it.

## Golden rules
- **The spec is the source of truth.** When this file and the spec disagree, the spec wins —
  flag the conflict instead of guessing.
- **Work ONE phase at a time** (`KRAYT_SPEC.md` §14). A phase is done only when its
  **"Done when"** criterion passes — prefer an automated test. Stop at phase boundaries for review.
- **Plan before coding.** At the start of each phase, give a short plan (files/packages,
  §9.1 deps, how you'll meet the "Done when") and wait for my OK before writing code. **In a
  sandboxed run there is nobody to give that OK** — state the plan and proceed (see below).

## Implementation protocol (spec §14)
- Maintain **`HUMAN_TODO.md`** at the repo root — the single handoff log.
- For steps you cannot do yourself — `[HUMAN]`-tagged or otherwise needing the Mac,
  credentials, CI, real hardware, or live API keys:
  1. Do everything around the step that you can (write the config, scripts, CI YAML, commands, tests).
  2. Append a structured entry to `HUMAN_TODO.md` (template in §14).
  3. Then: if non-blocking, log and continue; if blocking, **stop and ask me**, referencing the entry.
- **When an entry is verified done, DELETE it** — never leave a `✅ DONE` entry behind. `HUMAN_TODO.md`
  is a queue of outstanding work, not an archive: what was verified belongs in the durable places
  (§14's phase checkboxes with the run id, `docs/ai-tasks/README.md`'s status table, the code comment
  carrying the provenance), and `git log` keeps the rest. Record the outcome there **first**, then
  remove the entry — deleting before the evidence has a permanent home loses it. See §14.
- **Never fabricate a result** for a human-only step — no fake signatures, invented image
  digests, or "boot succeeded" without a real boot. An honestly-blocked step is correct.

## Running a task from `docs/ai-tasks/` or `docs/common-tasks/`

Being handed one of these files means **do the work**, in full. It is not a proposal to review, a
spec to summarize, or a plan to write — the deliverable is the change itself: edited files, passing
checks, and the report. A run that ends with an analysis of the task, a restated plan, or a list of
what someone else should do next has failed, however good the analysis is.

- **The "Decisions already made" section is settled.** Implement it. Don't reopen a decision,
  re-derive it, or ask which option to take — the choices were made deliberately, before the task
  was written. Where it says "verify", verify the fact; that is not an invitation to change the
  approach. If you find hard evidence a decision is *wrong* (not merely different from what you'd
  pick), say so plainly, then implement the task's intent as best you can — don't stall on it.
- **Finish every part you can.** Being blocked on one deliverable is not permission to skip the
  others. Do all the rest, then say exactly what you left and why.
- **Only the §14 categories are genuine blockers** — real hardware, live credentials, a registry or
  CI run, a Mac. Everything else is yours to do. "I don't have Docker" blocks the image *build*, not
  writing the Dockerfile.
- **Need a human answer?** If the `ask_human` MCP tool is available (the run enabled questions), use
  it — that's what it's for, and it's cheaper than a wrong guess. If it isn't available, the run is
  autonomous: choose the most reasonable option, state the assumption and its reasoning in
  `/output/report.md`, log it in `HUMAN_TODO.md`, and keep going. Never idle waiting for input that
  cannot arrive.
- **Report honestly at the end**: what landed, what's handed off, what's unverified. An honestly
  incomplete run is fine; a run that claims completion it can't back is a defect, same as a
  fabricated result.

## Dependencies & codegen
- Use the **pinned dependencies in §9.1** exactly. Do not guess libraries or versions.
- macOS VM backend is **vfkit** (`crc-org/vfkit`) for v1; direct `Code-Hex/vz` is the
  documented fallback. Keep both behind the `Provider` interface — no provider specifics leak out.
- Protocol code is generated from `internal/protocol/krayt.proto` via `make proto` and
  **committed**. Don't hand-edit generated files; regenerate.

## Build hygiene
- Keep the OS-agnostic core build-tag-clean: `internal/provider/vfkit` and `.../vz` are
  `//go:build darwin`; `internal/guest/*` is `//go:build linux` (cross-compiled to
  `linux/arm64`); everything else compiles on both.
- The `Provider` interface is the only OS-specific seam. Test the core against the
  `fakeProvider`; don't require a real VM for unit tests.

## What needs real hardware (can't be done in a cloud agent)
The vfkit provider, the image boot test, and end-to-end runs need a real Apple-Silicon Mac.
Build/boot-test of the Nix VM image needs a Linux builder (CI). Route these through the
handoff protocol above rather than attempting or faking them.

## Tone
Be concise. Prefer small, reviewable diffs scoped to the current phase.
