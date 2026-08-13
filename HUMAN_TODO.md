# HUMAN_TODO

Single handoff log for steps the coding agent cannot complete itself (credentials, real
hardware, a Linux builder, live secrets). Template per `KRAYT_SPEC.md` §14.

---

## Status

**Open:** `move-egress-proxy-to-host.md` (Phase 8, §14) needs a guest image rebuild (it changes
`cmd/krayt-vsock-forward` + `internal/guest/proxy`), a `PinnedRef` bump to the new digest once CI
publishes it, and the Phase-3 egress hardware suite re-run against the new host-side design —
see the `[hardware]` entry below. **Blocking**: nothing else in the repo currently depends on it,
but no run with a real host-side egress proxy can boot until the image is rebuilt and re-pinned,
so the feature is offline-complete only until this lands.

**Open:** the `krayt-agent-claude-code`, `krayt-agent-gemini-cli`, and (new)
`krayt-agent-opencode` published images each need a real CI run + a real live onboarding run —
see the `[tooling]` entries below. The gemini-cli image also needs its `node:24-bookworm-slim`
base digest pinned (currently tag-only — this sandbox's egress proxy has no route to a Docker
registry) and a `hadolint` pass (binary unreachable here too); the opencode image needs the same
`hadolint` pass, for the same reason. All non-blocking (nothing downstream depends on them yet;
the codex agent-image task builds on the *code* landing here, not on these verifications).

The rootfs-compression handoff (ratio/timing + a real post-decompress boot) is
now fully confirmed — see the `[tooling/CI]` entry below it.

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

## [hardware] `move-egress-proxy-to-host.md` — image rebuild, `PinnedRef` bump, and Phase-3 egress suite re-verification

- **Needed:** (1) push these changes so the RC image-build workflow publishes a new guest image
  digest (the guest side changed: `cmd/krayt-vsock-forward` replaces `cmd/krayt-proxy`,
  `internal/guest/proxy` is simplified); (2) read that digest from the workflow run and bump
  `internal/vmimage/pinned.go`'s `PinnedRef`; (3) re-run the hardware egress suite against the
  new host-side proxy design on **both** backends (Apple Silicon/vfkit and Linux/KVM/firecracker).
- **Why the agent can't:** `PinnedRef` is a single hardcoded digest that does not exist until CI
  builds and publishes the new image (§11, §14) — it cannot be guessed or fabricated. The actual
  boot + egress-enforcement tests need real virtualization (vz on Apple Silicon, KVM on Linux),
  neither of which this sandbox has.
- **Exact steps/commands:**
  1. Push this branch/PR — `.github/workflows/vmimage-rc.yml` is path-triggered on
     `images/**`, `internal/guest/**`, `cmd/krayt-agent/**`, `cmd/krayt-vsock-forward/**`,
     `cmd/krayt-ask/**`, all of which this task touches, so it fires automatically (see
     `RELEASING.md`).
  2. Read the published RC tag/digest from the workflow run (or `oras manifest fetch` the tag it
     prints) and set `internal/vmimage/pinned.go`'s `PinnedRef` to it.
  3. `go test -tags integration ./internal/orchestrator/... -run TestEgressEnforcement` and
     `-run TestContainerHardening` on a Mac with vfkit installed (`KRAYT_KERNEL`/`KRAYT_INITRD`/
     `KRAYT_ROOTFS`/`KRAYT_NETPROBE_IMAGE`/`KRAYT_ALLOW_HOST`/`KRAYT_HARDENING_IMAGE` per each
     test's header comment) and on a Linux/KVM box for the firecracker backend — same tests, same
     env vars, `hack/run-integration-tests.sh` auto-detects which backend to use.
  4. `-run TestConcurrentRealVMs` on both backends (needs `KRAYT_IMAGE`, a real agent image) —
     asserts two simultaneous VMs each get their own egress socket + child process with no
     cross-VM reachability, the concurrency property this task's vsock design is supposed to buy
     over a gateway-bound TCP proxy.
  5. **New checks this task adds that the existing probes don't cover yet** — write these as real
     test/probe code before running them, don't skip straight to manual verification:
     - `nft list ruleset` inside the guest, over a debug shell or an extended probe image,
       contains **no `skuid` rule** (the existing `hardening-probe` only checks that
       `setuid(proxyd)` itself fails `EPERM`, which is still worth keeping as a non-root
       regression check, but no longer proves the egress lock's correctness — the ruleset shape
       does. `TestEgressRulesetShape` (`internal/guest/proxy/firewall_internal_test.go`) is the
       offline counterpart; this hardware check confirms the *installed* ruleset on a live guest
       matches it, not just the constant.
     - A container attempt to reach the **host** itself on a private address (e.g. the vfkit/
       firecracker host's own LAN IP) is refused by the SSRF guard — extend `hack/netprobe` with
       a fourth check (a new exit code) that CONNECTs through the proxy to a private-range target
       and asserts it gets a 403, proving the §2 hard-block change actually holds on hardware, not
       just in `TestCheckDialAddr`.
- **Verify success by:** all of the above tests/probes passing (`--- PASS`) on both backends,
  with the new image's digest recorded in `PinnedRef`. Update `KRAYT_SPEC.md` §14's Phase 8 "Done
  when (hardware)" checkbox from `[ ]` to `[x]` and this section's status line once confirmed —
  do not mark it done without a real boot + real test run.
- **Blocking:** no (nothing else in the repo currently depends on this landing), but the run
  supervisor cannot boot a VM with a working egress path against the *old* pinned image (guest
  side is now `krayt-vsock-forward`, not `krayt-proxy`) — so this is the one step standing between
  "offline-complete" and "actually usable" for this task specifically. See the top-of-file Status
  note.

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

## [tooling/CI] Real compression ratio, CI time, and post-decompress boot for `rootfs.img` zstd compression — ✅ DONE

Ran for real via `workflow_dispatch` (`publish: true`, no tag) on commit `85f7446`, published as
`ghcr.io/418-cloud/krayt-vmimage:manual-85f74468c467-{arm64,amd64}`. Both arches pushed the new
`rootfs.img.zst` layer under `application/vnd.krayt.rootfs+zstd`, with `vmlinuz`/`initrd`
unchanged (`Exists`, reused from a prior push) — confirms the media-type/layer-shape half of the
change end-to-end against a real registry.

**Measured ratio and step time**, from the "Push OCI artifact + record digest" step's log
(`ls -la result/rootfs.img $stage/rootfs.img.zst` line + the step's own wall-clock duration):

| arch  | uncompressed  | `.zst`        | ratio  | zstd-reported | step wall time |
|-------|---------------|---------------|--------|----------------|----------------|
| arm64 | 2,317,037,568 B (2.16 GiB) | 464,153,046 B (443 MiB) | 4.99:1 | 20.03% | 2m 27s |
| amd64 | 2,178,670,592 B (2.03 GiB) | 483,812,248 B (461 MiB) | 4.50:1 | 22.21% | 4m 29s |

Ratio is well within the range that makes the `-19 -T0` choice (decision 3/5 in
`docs/ai-tasks/compress-vmimage-rootfs.md`) look justified — no need to revisit the `--long`/level
tradeoff based on this. Step time (compression + the `.zst` blob's registry upload, since the
other layers were already `Exists`) is a few minutes per arch, run in parallel across the matrix —
acceptable for a `publish` job that only runs on a tag push/dispatch, not on every PR.

**Boot confirmation**, on an Apple-Silicon Mac (vfkit), against the real published multi-arch
index (`internal/vmimage/pinned.go` updated to
`ghcr.io/418-cloud/krayt-vmimage@sha256:f831c8f1dff2f8c06a52e688fd62303048351fbb121694b16fadbcfd7ccb2501`,
which gathers the two per-arch manual pushes above):

- `krayt doctor` correctly reported the pinned digest **not cached** beforehand (proves it wasn't
  silently reusing an old plain-`rootfs.img` cache entry).
- `krayt image pull` verified the digest and produced a plain, decompressed
  `rootfs.img`/`vmlinuz`/`initrd` under `~/Library/Caches/krayt/vmimage/<digest>/` — confirms
  `vmimage.Pull`'s zstd-decompress-then-verify path works against a real registry artifact, not
  just the offline fixture.
- `krayt run` (with `--skip-resource-check`, unrelated to this change — just local free-memory
  headroom) booted the pulled image under vfkit for a real task: `echo "Write Hello to a
  greetings.txt file" | krayt run --task -` completed exit 0, `greetings.txt` containing `Hello`
  came back in `changes.patch`, and `report.md`'s provenance section shows the run's own commit
  (`85f74468c467`) matching the compression change itself. A corrupted/truncated decompression
  would have failed to boot or produced garbage here, not a silent success — this is the real
  round-trip the offline tests couldn't reach.

This closes out the item: offline unit tests, real CI ratio/timing, and a real pull+boot are all
now confirmed. No gaps remain.

## [tooling] Publish `krayt-agent-claude-code` — real workflow run + live onboarding run
- Needed:
  1. A real push to `main` (or `workflow_dispatch`) triggering `.github/workflows/agent-images.yml`,
     confirming both the `linux/amd64` and `linux/arm64` builds succeed, the merge job assembles a
     multi-arch manifest, and `ghcr.io/418-cloud/krayt-agent-claude-code` is pushed with `:latest`,
     `:sha-<short>`, and `:2.1.226` (the pinned CLI version) — plus a check that the GHCR package is
     publicly visible (matching the other published images, e.g. `krayt-dev`/`krayt-probe`).
  2. A live onboarding run exactly as the main README's quickstart shows, with a real
     `ANTHROPIC_API_KEY`, confirming a `changes.patch` and `/output/report.md` come back:
     ```sh
     krayt run --image ghcr.io/418-cloud/krayt-agent-claude-code --agent claude-code \
       --task ./task.md --repo . --secrets ./secrets.env --allow api.anthropic.com
     ```
- Why the agent can't: no `docker build`/push access and no live Anthropic credential in this
  environment; also can't confirm GHCR package visibility without a real push.
- Exact steps/commands: push this change (or `gh workflow run agent-images.yml`) and watch the
  run; then, on a Mac with `krayt` built and the base VM image pulled, create a scratch repo +
  `task.md` + `secrets.env` (one `ANTHROPIC_API_KEY`) and run the quickstart command above.
- Verify success by: `agent-images.yml` green with both arches in the manifest
  (`docker buildx imagetools inspect ghcr.io/418-cloud/krayt-agent-claude-code:latest` shows
  `linux/amd64,linux/arm64`); the GHCR package page loads without auth; `krayt ls` shows the run
  reaching `done` with `EXIT 0`; `.krayt/runs/<id>/changes.patch` applies cleanly and
  `report.md` contains Claude's summary.
- Blocking: no — the gemini-cli/opencode agent-image tasks depend on this task's *code* (the
  `agent-images.yml` scaffolding + README table) having landed, not on this verification.

## [tooling] Pin the `node:24-bookworm-slim` digest in `images/agents/gemini-cli/Dockerfile`
- Needed: `images/agents/gemini-cli/Dockerfile` currently reads `FROM node:24-bookworm-slim`
  (tag only, no `@sha256:...`) — every other base image in this repo is digest-pinned, and
  `renovate.json`'s `pinDigests: true` (for the `dockerfile` manager) expects one to maintain.
- Why the agent can't: this environment's egress proxy blocks `registry.npmjs.org`,
  `hub.docker.com`, `registry-1.docker.io`, `ghcr.io`, and GitHub release-asset downloads (all
  return `403`/connection-refused) — only `api.github.com` (via the authenticated `gh` CLI) is
  reachable, which has no route to a Docker registry's manifest digest. Inventing a
  plausible-looking `sha256:...` would silently break the build, which is worse than leaving it
  unpinned — see `CLAUDE.md`'s "never fabricate ... invented image digests."
- Exact steps/commands: either (a) merge Renovate's bootstrap PR — `pinDigests: true` makes
  Renovate open a PR adding the digest to this exact unpinned `FROM` line on its next run, no
  human digest-lookup needed, or (b) resolve it manually first:
  `docker buildx imagetools inspect node:24-bookworm-slim` (or `crane digest
  node:24-bookworm-slim`) and hand-edit the `FROM` line.
- Verify success by: `FROM node:24-bookworm-slim@sha256:<digest>` in the Dockerfile, and
  `agent-images.yml` still builds both arches successfully off it.
- Blocking: no — the tag alone still resolves to a real, current image; this only affects
  reproducibility/supply-chain pinning, not whether the image builds or runs.

## [tooling] Publish `krayt-agent-gemini-cli` — real workflow run + live onboarding run
- Needed:
  1. A real push to `main` (or `workflow_dispatch`) triggering `.github/workflows/agent-images.yml`,
     confirming both the `linux/amd64` and `linux/arm64` builds succeed, the merge job assembles a
     multi-arch manifest, and `ghcr.io/418-cloud/krayt-agent-gemini-cli` is pushed with `:latest`,
     `:sha-<short>`, and `:0.55.1` (the pinned CLI version) — plus a check that the GHCR package is
     publicly visible (matching the other published images).
  2. A live onboarding run exactly as this image's README quickstart shows, with a real
     `GEMINI_API_KEY`, confirming a `changes.patch` and `/output/report.md` come back:
     ```sh
     krayt run --image ghcr.io/418-cloud/krayt-agent-gemini-cli --agent gemini-cli \
       --task ./task.md --repo . --secrets ./secrets.env --allow generativelanguage.googleapis.com
     ```
  3. One `--on-question=wait` run against the same image, prompting the task to ask a genuine
     question, confirming the `ask_human` MCP server (registered via the entrypoint's runtime
     `~/.gemini/settings.json` rewrite) actually surfaces the question to `krayt questions`/
     `krayt answer` and the run resumes — this is the one piece of MCP wiring that can't be
     verified without a real `gemini` process talking to a real socket.
- Why the agent can't: no `docker build`/push access, no live Gemini API key, and no way to
  drive a real `--on-question=wait` round-trip in this environment.
- Exact steps/commands: push this change (or `gh workflow run agent-images.yml`) and watch the
  run; then, on a Mac with `krayt` built and the base VM image pulled, create a scratch repo +
  `task.md` + `secrets.env` (one `GEMINI_API_KEY`) and run the quickstart command above; repeat
  with `--on-question=wait` and a task prompt that instructs the agent to ask a clarifying
  question before proceeding.
- Verify success by: `agent-images.yml` green with both arches in the manifest
  (`docker buildx imagetools inspect ghcr.io/418-cloud/krayt-agent-gemini-cli:latest` shows
  `linux/amd64,linux/arm64`); the GHCR package page loads without auth; `krayt ls` shows the run
  reaching `done` with `EXIT 0`; `.krayt/runs/<id>/changes.patch` applies cleanly and
  `report.md` contains Gemini's response; the `--on-question=wait` run shows `waiting` in
  `krayt ls`, `krayt questions <run-id>` lists the real question, and `krayt answer` resumes it
  to completion.
- Blocking: no — nothing downstream depends on this verification; the opencode agent-image task
  builds on the `agent-images.yml` scaffolding landing, not on this run happening.

## [tooling] `hadolint` the gemini-cli Dockerfile
- Needed: `images/agents/gemini-cli/Dockerfile` was hand-reviewed against common hadolint rules
  (pinned `USER` before `ENTRYPOINT`, JSON-form `ENTRYPOINT`, apt lists cleaned up, no `latest`
  tag) but never actually run through `hadolint` — the binary isn't installed in this
  environment and the sandbox's egress proxy blocks the GitHub release-asset host it ships from
  (`release-assets.githubusercontent.com` → `403`), so it couldn't be fetched either.
- Why the agent can't: no package manager access or reachable download host for the `hadolint`
  binary in this sandbox.
- Exact steps/commands: `hadolint images/agents/gemini-cli/Dockerfile` (or
  `docker run --rm -i hadolint/hadolint < images/agents/gemini-cli/Dockerfile`).
- Verify success by: no unexpected findings (the image intentionally mirrors
  `images/agents/claude-code/Dockerfile`'s already-passing shape).
- Blocking: no — this is a lint pass, not a build/runtime correctness issue.

## [tooling] Publish `krayt-agent-opencode` — real workflow run + live onboarding run
- Needed:
  1. A real push to `main` (or `workflow_dispatch`) triggering `.github/workflows/agent-images.yml`,
     confirming both the `linux/amd64` and `linux/arm64` builds succeed (including the models.dev
     catalog snapshot fetched at build time — confirm it actually lands at
     `/home/agent/.cache/opencode/models.json` in the built image and isn't empty/stale), the
     merge job assembles a multi-arch manifest, and `ghcr.io/418-cloud/krayt-agent-opencode` is
     pushed with `:latest`, `:sha-<short>`, and `:1.18.16` (the pinned CLI version) — plus a check
     that the GHCR package is publicly visible (matching the other published images).
  2. A live onboarding run exactly as this image's README quickstart shows, with a real
     `ANTHROPIC_API_KEY` (one provider is enough per the task), confirming a `changes.patch` and
     `/output/report.md` come back:
     ```sh
     krayt run --image ghcr.io/418-cloud/krayt-agent-opencode --agent opencode \
       --task ./task.md --repo . --secrets ./secrets.env --allow api.anthropic.com
     ```
     Also confirms the egress mitigation actually holds: with only `api.anthropic.com`
     allowlisted, opencode must NOT hit `models.opencode.ai` (the baked-in catalog snapshot +
     `OPENCODE_DISABLE_MODELS_FETCH=true` should make that unnecessary) — a run that stalls or
     fails on a blocked catalog fetch means that mitigation didn't actually work and needs
     revisiting.
  3. One `--on-question=wait` run against the same image, prompting the task to ask a genuine
     question, confirming the `ask_human` MCP server (registered via the entrypoint's runtime
     `OPENCODE_CONFIG` file pointing at a generated `mcp` block) actually surfaces the question to
     `krayt questions`/`krayt answer` and the run resumes — this is the one piece of MCP wiring
     that can't be verified without a real `opencode` process talking to a real socket, and the
     first time this image's specific MCP config shape (`type: "local"`, array `command`) has been
     exercised against a live opencode binary rather than just checked against docs.
- Why the agent can't: no `docker build`/push access, no live provider API key, and no way to
  drive a real `--on-question=wait` round-trip in this environment.
- Exact steps/commands: push this change (or `gh workflow run agent-images.yml`) and watch the
  run; then, on a Mac with `krayt` built and the base VM image pulled, create a scratch repo +
  `task.md` + `secrets.env` (one `ANTHROPIC_API_KEY`) and run the quickstart command above; repeat
  with `--on-question=wait` and a task prompt that instructs the agent to ask a clarifying
  question before proceeding.
- Verify success by: `agent-images.yml` green with both arches in the manifest
  (`docker buildx imagetools inspect ghcr.io/418-cloud/krayt-agent-opencode:latest` shows
  `linux/amd64,linux/arm64`); the GHCR package page loads without auth; `krayt ls` shows the run
  reaching `done` with `EXIT 0`; `.krayt/runs/<id>/changes.patch` applies cleanly and
  `report.md` contains opencode's response; the `--on-question=wait` run shows `waiting` in
  `krayt ls`, `krayt questions <run-id>` lists the real question, and `krayt answer` resumes it
  to completion.
- Blocking: no — nothing downstream depends on this verification; the codex agent-image task
  builds on the `agent-images.yml` scaffolding landing, not on this run happening.

## [tooling] `hadolint` the opencode Dockerfile
- Needed: `images/agents/opencode/Dockerfile` was hand-reviewed against common hadolint rules
  (pinned `USER` before `ENTRYPOINT`, JSON-form `ENTRYPOINT`, apt lists cleaned up, no `latest`
  tag, digest-pinned `FROM`) but never actually run through `hadolint` — same tooling gap as the
  gemini-cli entry above (binary not installed, release-asset host unreachable from this sandbox).
- Why the agent can't: no package manager access or reachable download host for the `hadolint`
  binary in this sandbox.
- Exact steps/commands: `hadolint images/agents/opencode/Dockerfile` (or
  `docker run --rm -i hadolint/hadolint < images/agents/opencode/Dockerfile`).
- Verify success by: no unexpected findings (the image intentionally mirrors
  `images/agents/claude-code/Dockerfile`'s already-passing shape, modulo the `TARGETARCH` case
  statement it shares with `hack/krayt-dev/Dockerfile`'s already-reviewed pattern instead).
- Blocking: no — this is a lint pass, not a build/runtime correctness issue.
