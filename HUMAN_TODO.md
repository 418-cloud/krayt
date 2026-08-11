# HUMAN_TODO

Single handoff log for steps the coding agent cannot complete itself (credentials, real
hardware, a Linux builder, live secrets). Template per `KRAYT_SPEC.md` §14.

---

## Status

**Open:** nothing. The last open item — the `krayt upgrade` real-network download+swap smoke
test — is now confirmed on real darwin hardware with unrestricted internet. See the `[tooling]`
entry at the bottom.

Everything else is shipped: all three integration-test-runner handoffs are confirmed — two on real
hardware, and `integration-linux` is now green in CI. The `gh` CLI + `GH_TOKEN` +
`fix-pr-review-comments` change is also fully confirmed now — real image build (both arches), a
real read-only fine-grained PAT authenticating and reading review comments (and genuinely refused
on a write attempt), and a real end-to-end run against a real PR. See the three `[tooling]` /
`[GitHub]` entries below. The vmimage RC/graduate workflows are confirmed too — real PR-triggered
RC publish, a real graduate dispatch with matching digest, and concurrent-PR queuing under the
`vmimage-rc-tag` concurrency group. See the `[tooling/CI]` entry below.

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

## [tooling/CI] vmimage RC/graduate workflows — ✅ DONE

Added `hack/next-vmimage-tag.sh`, `.github/workflows/vmimage-rc.yml`, and
`.github/workflows/vmimage-graduate.yml` (see `RELEASING.md` for the full flow). The
tag-computation logic was already verified locally (fabricated tag lists for rc→rc+1,
stable→next-patch-rc.1, and no-prior-tag, plus a real push round-trip against a scratch bare
repo); the three things that needed a real GitHub Actions run are now confirmed for real too:

1. **A real PR push triggers `vmimage-rc.yml` and publishes a working RC tag.** Confirmed: a PR
   touching a watched path (`images/**`, `internal/guest/**`, `cmd/krayt-agent/**`,
   `cmd/krayt-proxy/**`, `cmd/krayt-ask/**`) ran the workflow, computed the expected tag, and
   pushed it — and `image.yml`'s existing tag trigger picked it up and published.
2. **A real `vmimage-graduate.yml` dispatch re-tags the right commit and `image.yml` publishes it
   correctly.** Confirmed: run with a real `rc_tag` + `version`, the new clean tag pointed at the
   RC's exact commit (not `main`'s tip), and the published digest matched the already-tested RC's
   digest — the reproducibility expectation from `RELEASING.md` held.
3. **Concurrent PRs touching these paths behave as expected under the `vmimage-rc-tag`
   concurrency group.** Confirmed: two overlapping runs queued rather than raced (global group, no
   `cancel-in-progress`), as designed.

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

## [tooling] Build the `krayt-dev` image with the new `gh` CLI layer — ✅ DONE

The `gh` CLI install layer was added to `hack/krayt-dev/Dockerfile` (`ARG GH_CLI_VERSION=2.96.0`,
fetched as a `gh_<version>_linux_<TARGETARCH>.tar.gz` release tarball, same exception pattern as
`protoc`). Confirmed for real: CI (`.github/workflows/dev-image.yml`) built both `linux/amd64` and
`linux/arm64` on native runners, and `gh --version` runs correctly in the built image — exercised
directly by the real `fix-pr-review-comments` run below, which depends on `gh` working inside it.

## [GitHub] Confirm a read-only fine-grained PAT authenticates `gh` and reads PR review comments — ✅ DONE

`entrypoint.sh` runs `gh auth login --with-token < /run/secrets/GH_TOKEN` when `GH_TOKEN` is present
(non-fatal when absent). Verified with a real fine-grained PAT scoped to this repo with exactly
**Metadata + Contents + Pull requests: read** (no write):

- `gh auth login --with-token` succeeded with that token.
- `gh api "repos/{owner}/{repo}/pulls/<n>/comments"` returned the PR's real inline **review**
  comments.
- A write attempt (`gh api -X POST` / `gh pr comment`) was genuinely **refused by GitHub** —
  confirms the read-only design holds at the token level, not just by the task's own restraint.

## [GitHub] Real run of `docs/common-tasks/fix-pr-review-comments.md` against a real PR — ✅ DONE

Run via `krayt run` with live credentials against a real PR with real inline review comments.
Confirmed it: fetched the **review** comments (not just issue comments), triaged each against the
actual code, fixed genuine issues, left false positives untouched with a stated reason, wrote the
summary table + suggested commit message to `report.md`, and attempted **no** GitHub write.

## [tooling] `krayt upgrade` real-network download+swap smoke test — ✅ DONE

`krayt upgrade` (`internal/selfupdate` + `internal/cli/upgrade.go`) was fully unit-tested offline
against `httptest` fixtures from day one; what remained was the real end-to-end path this file
originally called out as unreachable from the cloud sandbox (no unallowlisted internet, and
linux/arm64 has no published release asset regardless). Run for real on darwin hardware
(`/Users/tjololo/.local/bin/krayt`, real `api.github.com` + release CDN, real installed binary),
covering everything the earlier sandbox pass couldn't:

- Interactive upgrade (no `--yes`): real TTY prompt `Upgrade? [y/N]`, answered `y` — download,
  checksum verification, extraction, and atomic swap all happened for real (0.7.0 → 0.7.1).
- Post-swap confirmation subprocess: immediately after the backup message, `krayt upgrade` itself
  printed the new binary's `version` output (`krayt 0.7.1` + vm-image digest) — confirms
  `exec.CommandContext(ctx, path, "version")` runs the freshly-swapped binary, not the old one.
- Backup + restore path: `krayt.bak` was created on every swap with the documented restore
  command printed (`cp .../krayt.bak .../krayt`).
- Downgrade path (`--version v0.6.0`-style, run here as `--version v0.7.0` against a 0.7.1
  install): correctly labeled `(downgrade)` in the prompt, and completed the same
  download/verify/swap sequence in reverse.
- `--check`: correctly reported `up to date` when current == target, and `upgrade available` when
  current < target, in both directions.
- `--yes`: skipped the interactive prompt and upgraded non-interactively as documented.
- Round-tripped 0.7.1 → 0.7.0 → 0.7.1 across four separate invocations, with `krayt version`
  after each swap matching the version just installed (including the correct pinned vm-image
  digest changing between 0.7.0 and 0.7.1) — real, observable confirmation, not something
  provable from the network-restricted sandbox this was originally logged from.

No gaps remain: this closes out every item the original entry listed as unverifiable (tarball
download, checksum verification, extraction, atomic swap, the post-swap confirmation subprocess,
and the `--yes`-free interactive prompt).
