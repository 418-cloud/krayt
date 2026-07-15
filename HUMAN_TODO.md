# HUMAN_TODO

Single handoff log for steps the coding agent cannot complete itself (credentials, real
hardware, a Linux builder, live secrets). Template per `KRAYT_SPEC.md` §14.

---

## Status

**Open: CI verification of the `integration-linux` job** (see the handoff below). The other two
integration-test-runner handoffs are confirmed on real hardware. Everything else is shipped.

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

## [tooling/CI] First real run of the `integration-linux` CI job

- Needed: confirm the new `.github/workflows/ci.yml` → `integration-linux` job actually runs the
  suite on a GitHub-hosted Ubuntu runner — i.e. that the runner exposes a usable `/dev/kvm`, that
  the GHCR pulls of `krayt-vmimage` + `krayt-probe` succeed, and that the tests pass (or, if they
  fail, why).
- Why the agent can't: no `/dev/kvm` in the sandbox (`krayt doctor` FAILs the KVM check here), and
  no way to run a hosted-runner job. Whether the standard hosted runner has KVM is a known unknown
  (it works for some KVM-dependent actions, e.g. Android emulators, but confirm rather than assume).
- Exact steps/commands: trigger it via `workflow_dispatch` (Actions → ci → Run workflow) or by a PR
  touching one of its path filters (`internal/provider/**`, `internal/orchestrator/**`,
  `internal/guest/**`, `cmd/krayt-agent/**`, `hack/*-probe/**`, `hack/netprobe/**`,
  `hack/run-integration-tests.sh`, `.github/workflows/ci.yml`). The job installs firecracker
  v1.16.1, `modprobe tun`, runs `hack/linux-net-setup.sh`, logs into GHCR, then runs the script.
- Verify success by: the job is green with real `--- PASS` lines in the "Run the integration suite"
  step. If `/dev/kvm` genuinely isn't available on the hosted runner, say so plainly — the job will
  go red at the `krayt doctor` preflight (it does not silently no-op) and this handoff becomes
  "self-hosted KVM runner needed" rather than "done".
- Note for the maintainer (design nuance, not a blocker): `krayt doctor`'s CAP_NET_ADMIN check
  inspects the running binary, and `make krayt` writes a fresh, un-capped `./bin/krayt` each time,
  so the script `sudo setcap cap_net_admin+ep ./bin/krayt` right after building (a per-invocation
  grant on a build artifact — not `linux-net-setup.sh`'s persistent NAT/systemd setup, which it
  still does not touch). Separately, `doctor`'s host-NAT check is optional (`[warn]`, §6.6 — a
  no-egress task runs fine without it), so a Linux host that never ran `hack/linux-net-setup.sh`
  passes the preflight and only `TestEgressEnforcement` fails at run time; the CI job runs
  `linux-net-setup.sh` so that gap doesn't bite it. Flagging in case you'd prefer the preflight to
  hard-fail on missing NAT.
- Blocking: no.
