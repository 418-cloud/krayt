# HUMAN_TODO

Single handoff log for steps the coding agent cannot complete itself (credentials, real
hardware, a Linux builder, live secrets). Template per `KRAYT_SPEC.md` §14.

---

## Status

**Nothing open right now.**

Phases 0–7 are complete and released as
[`v0.5.0`](https://github.com/418-cloud/krayt/releases/tag/v0.5.0) — krayt runs a real coding
agent in an isolated micro-VM over an untrusted repo and hands back a reviewable patch, with
egress control, secrets redaction, concurrency, park-and-walk-away, and an agent↔human question
channel, on **both** macOS/vfkit and Linux/firecracker behind the same `Provider` interface. All
security-review findings (Critical, High, Medium, and Low) are fixed and verified on hardware —
see `docs/ai-tasks/README.md` for the fix-by-fix status table. The multi-arch base VM image and
all seven probe images are published and public on GHCR, and a real Claude Code agent run has
completed on both backends against the same pinned image digest.

The detailed phase-by-phase and finding-by-finding history that used to live in this file has been
pruned now that it's shipped — the record of *how* lives in `git log`/PR history,
`docs/ai-tasks/README.md`, and `KRAYT_SPEC.md`'s own `[x]` phase checklists, not here. This file
only tracks what's still open, and right now that's nothing.
