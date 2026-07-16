# HUMAN_TODO

Single handoff log for steps the coding agent cannot complete itself (credentials, real
hardware, a Linux builder, live secrets). Template per `KRAYT_SPEC.md` §14.

---

## Status

**Open:** three verifications for the `gh` CLI + `GH_TOKEN` + `fix-pr-review-comments` change need a
real Docker build, a real fine-grained PAT, and a real krayt run against a real PR — see the three
`[tooling]` / `[GitHub]` entries at the bottom. None can be done or faked from a cloud agent.

Everything else is shipped: all three integration-test-runner handoffs are confirmed — two on real
hardware, and `integration-linux` is now green in CI.

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

## [tooling/CI] vmimage RC/graduate workflows — real GitHub Actions run needed

Added `hack/next-vmimage-tag.sh`, `.github/workflows/vmimage-rc.yml`, and
`.github/workflows/vmimage-graduate.yml` (see `RELEASING.md` for the full flow). The
tag-computation logic itself is verified locally (fabricated tag lists for rc→rc+1,
stable→next-patch-rc.1, and no-prior-tag, plus a real push round-trip against a scratch bare
repo) — but three things need a real run against GitHub Actions to confirm, not just reason
about:

1. **A real PR push actually triggers `vmimage-rc.yml` and publishes a working RC tag.** Open a
   PR touching one of the watched paths (`images/**`, `internal/guest/**`, `cmd/krayt-agent/**`,
   `cmd/krayt-proxy/**`, `cmd/krayt-ask/**`) and confirm the workflow runs, computes the expected
   tag, and pushes it — then that `image.yml`'s existing tag trigger picks it up and publishes.
2. **A real `vmimage-graduate.yml` dispatch actually re-tags the right commit and `image.yml`
   publishes it correctly.** Run it with a real `rc_tag` + `version` and confirm the new clean tag
   points at the RC's exact commit (not `main`'s tip) and that the published digest matches the
   already-tested RC's digest (the reproducibility expectation noted in `RELEASING.md`).
3. **Whether concurrent PRs touching these paths behave as expected under the
   `vmimage-rc-tag` concurrency group** — plausible to reason about (global group, no
   `cancel-in-progress`, so overlapping runs queue rather than race), but not proven without
   actually triggering two overlapping runs.

Not fabricating any of these — logging here per the handoff protocol instead.

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

## [tooling] Build the `krayt-dev` image with the new `gh` CLI layer — ⏳ OPEN

The `gh` CLI install layer was added to `hack/krayt-dev/Dockerfile` (`ARG GH_CLI_VERSION=2.96.0`,
fetched as a `gh_<version>_linux_<TARGETARCH>.tar.gz` release tarball, same exception pattern as
`protoc`). This sandbox has no Docker/buildx, so the build itself is unverified. Confirm it builds
for **both** arches — the asset arch names are Docker's `TARGETARCH` values verbatim
(`amd64`/`arm64`), no translation, but that's only confirmed against the release asset naming, not a
real pull:

```sh
# repo root — the Dockerfile COPYs go.mod/go.sum from here
docker buildx build --platform linux/arm64 -f hack/krayt-dev/Dockerfile -t krayt-dev:local .
# and, to prove the amd64 asset URL/path resolves too (slow under QEMU; CI does both natively):
docker buildx build --platform linux/amd64 -f hack/krayt-dev/Dockerfile -t krayt-dev:local-amd64 .
```

CI (`.github/workflows/dev-image.yml`) already builds both arches on native runners on any push
touching `hack/krayt-dev/**`, so merging exercises this — but confirm `gh --version` runs in the
built image before relying on it. Do **not** fabricate a "builds fine" result.

## [GitHub] Confirm a read-only fine-grained PAT authenticates `gh` and reads PR review comments — ⏳ OPEN

`entrypoint.sh` now runs `gh auth login --with-token < /run/secrets/GH_TOKEN` when `GH_TOKEN` is
present (non-fatal when absent). Verify, with a **real** fine-grained PAT scoped to this repo with
exactly **Metadata + Contents + Pull requests: read** (no write):

- `gh auth login --with-token` succeeds with that token;
- `gh api "repos/{owner}/{repo}/pulls/<n>/comments"` returns the PR's inline **review** comments;
- the token genuinely **cannot** write (a `gh pr comment`/`gh api -X POST` attempt is refused by
  GitHub) — the read-only design depends on this being true at the token level.

Needs a real token + a real PR; not provable statically. Never fabricate a token or a result.

## [GitHub] Real run of `docs/common-tasks/fix-pr-review-comments.md` against a real PR — ⏳ OPEN

Run the new reusable task via `krayt run` with live credentials against a real PR that has Copilot
(or other inline) review comments — from a local checkout of that PR's branch, `--repo .`, with
`--allow api.anthropic.com,api.github.com` and a `--secrets` file carrying the model credential +
`GH_TOKEN`. Confirm it: fetches the **review** comments (not just issue comments), triages each
against the actual code, fixes only real issues, leaves false positives untouched with a specific
reason, writes the summary table + suggested commit message to `report.md`, and attempts **no**
GitHub write. Needs an actual run with live credentials — not something provable statically.
