# krayt — working agreement

This repo implements `krayt`, specified in full in **`KRAYT_SPEC.md`**. Read the spec before
working. This file is the standing agreement for how to build it.

## Golden rules
- **The spec is the source of truth.** When this file and the spec disagree, the spec wins —
  flag the conflict instead of guessing.
- **Work ONE phase at a time** (`KRAYT_SPEC.md` §14). A phase is done only when its
  **"Done when"** criterion passes — prefer an automated test. Stop at phase boundaries for review.
- **Plan before coding.** At the start of each phase, give a short plan (files/packages,
  §9.1 deps, how you'll meet the "Done when") and wait for my OK before writing code.

## Implementation protocol (spec §14)
- Maintain **`HUMAN_TODO.md`** at the repo root — the single handoff log.
- For steps you cannot do yourself — `[HUMAN]`-tagged or otherwise needing the Mac,
  credentials, CI, real hardware, or live API keys:
  1. Do everything around the step that you can (write the config, scripts, CI YAML, commands, tests).
  2. Append a structured entry to `HUMAN_TODO.md` (template in §14).
  3. Then: if non-blocking, log and continue; if blocking, **stop and ask me**, referencing the entry.
- **Never fabricate a result** for a human-only step — no fake signatures, invented image
  digests, or "boot succeeded" without a real boot. An honestly-blocked step is correct.

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
