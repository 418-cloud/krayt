# HUMAN_TODO

Single handoff log for steps the coding agent cannot complete itself (credentials, real
hardware, a Linux builder, live secrets). Template per `KRAYT_SPEC.md` §14.

---

## Status

Phases 0–6 are complete and verified end-to-end on Apple-Silicon hardware — krayt runs a real
coding agent in an isolated micro-VM over an untrusted repo and hands back a reviewable patch,
with egress control, secrets redaction, concurrency, park-and-walk-away, and an agent↔human
question channel. All security-review findings (Critical, High, Medium, and Low) are fixed and
verified on hardware — see `docs/ai-tasks/README.md` for the fix-by-fix status table.

The detailed phase-by-phase and finding-by-finding history that used to live in this file has been
pruned to keep it short and current — it was all resolved, and the record of *how* lives in `git
log`/PR history and `docs/ai-tasks/README.md`, not here. This file only tracks what's still open.

**Nothing currently outstanding.**
