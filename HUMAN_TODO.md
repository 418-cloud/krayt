# HUMAN_TODO

Single handoff log for steps the coding agent cannot complete itself (credentials, real
hardware, a Linux builder, live secrets). Template per `KRAYT_SPEC.md` §14.

---

## Status

**Nothing open.** All three integration-test-runner handoffs are confirmed — two on real
hardware, and `integration-linux` is now green in CI. Everything else is shipped.

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
only tracks what's still open.

---

## [tooling] Build + first-run the new `edit-probe` image — ✅ DONE

Published multi-arch to `ghcr.io/418-cloud/krayt-probe:edit-probe` via `probe-images.yml`. The
first real run on hardware caught a genuine bug: the original entrypoint wrote an unrelated new
file (`EDITED_BY_KRAYT.txt`) instead of touching the repo's own content, so `TestConcurrentRealVMs`
could never see its per-run marker survive into `changes.patch` — it would have failed on every
run, regardless of whether VM isolation actually held. Fixed to append to the existing
`greeting.txt` instead, so the untouched marker line rides along as ordinary diff context.
Confirmed on an Apple-Silicon Mac after the fix: `TestEndToEndRealVM` and `TestConcurrentRealVMs`
both `--- PASS`.

## [tooling] Run `hack/run-integration-tests.sh` on an Apple-Silicon Mac (macOS/vfkit path) — ✅ DONE

Run end-to-end on real Apple-Silicon hardware: `TestBootHello`, `TestEndToEndRealVM`,
`TestEgressEnforcement`, `TestContainerHardening`, `TestRootImageFailsClosed`,
`TestGuestGitConfigInjectionInert`, `TestSecretConfinementInArtifacts`, and `TestConcurrentRealVMs`
all `--- PASS`; the script exited 0 with `==> Integration suite passed.` — confirms the script
correctly encodes the darwin/vfkit manual steps it replaces.

## [tooling/CI] First real run of the `integration-linux` CI job — ✅ DONE

Confirmed green on a GitHub-hosted Ubuntu runner: `/dev/kvm` is present (just not permissioned for
the runner user by default — worked around with a udev rule in `ci.yml` rather than group
membership, since a CI job never gets the fresh login session that normally requires), and the
full suite passes, `TestEgressEnforcement` included.

That last one surfaced a real bug along the way, not a CI-only quirk: any Linux host running both
Docker and krayt's firecracker backend silently drops all guest egress. `dockerd` sets the
netfilter `FORWARD` hook's policy to `DROP` at startup — a separate base chain from krayt's own
`krayt_fwd`, hooked at the same priority; nftables evaluates every base chain at a given hook
independently, and a `DROP` in any one of them is terminal regardless of what the others decide.
Fixed in `hack/linux-net-setup.sh` (an explicit accept in Docker's own `DOCKER-USER` chain, the
customization point Docker documents for exactly this) and surfaced in `krayt doctor`'s NAT check
so a host in this state doesn't look falsely green. Documented in the README's Linux prerequisites.
