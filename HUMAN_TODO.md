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

**Phase 7 (Linux/firecracker) is complete and verified on real hardware** — a Linux host with
`/dev/kvm` (nested virt), not a Mac. The Phase 2 end-to-end test passes unmodified through the
firecracker provider, plus Hello, guest-network, 3-way concurrency, and the whole Phase 3 security
suite. A **real Claude Code agent run** completed on Linux (`run_26018248`), and the same pinned
multi-arch image digest was confirmed still working on Apple Silicon — so krayt is now genuinely
dual-backend, verified from both sides.

Notably this phase needed *no* human hardware handoff for the agent-driven parts: unlike the vfkit
path, a Linux/KVM host can be driven directly, which also means the Linux integration suite can run
in CI.

The detailed phase-by-phase and finding-by-finding history that used to live in this file has been
pruned to keep it short and current — it was all resolved, and the record of *how* lives in `git
log`/PR history and `docs/ai-tasks/README.md`, not here. This file only tracks what's still open.

---

## [Phase 7] Publish the multi-arch base VM image and re-pin — ✅ DONE

Published and pinned:
`ghcr.io/418-cloud/krayt-vmimage@sha256:68bc9efe9b649cc79309ff11925ed8d8e3c5c6dc14b272ae8e07f1c32cb07661`
(v0.3.0-rc1) — a genuine multi-arch OCI index carrying both `linux/amd64` and `linux/arm64`, each
entry with its `platform` set.

`internal/vmimage` resolves that index to the host's architecture at pull time, so the pin stays
**one ref + one digest with no architecture in it** rather than needing per-`GOARCH` constants.

Verified from both sides against the real published artifact: on Linux, `krayt image pull` (no
arguments) fetched the amd64 variant anonymously from ghcr, `krayt doctor` went fully green, and
the Phase 2 end-to-end + 3-way concurrency tests passed against it under firecracker; on Apple
Silicon, a human confirmed krayt still works against the same digest. Unit-covered offline by
`TestPullSelectsHostArchFromIndex` / `TestPullRejectsIndexWithoutHostArch`.

`.github/workflows/image.yml` builds both arches on native runners, pushes each with an OCI image
config declaring its platform, gathers them into one index, and **asserts the index entries carry
`platform`** before printing the digest — because an index without it publishes perfectly cleanly
and then matches nothing on any host.

---

## [Phase 7] Publish the probe images for amd64 (built + verified locally; not published)

**Non-blocking — the security suite has now been *run* on Linux and passes.** The whole Phase 3
suite (`TestEgressEnforcement`, `TestContainerHardening`, `TestRootImageFailsClosed`,
`TestGuestGitConfigInjectionInert`, `TestSecretConfinementInArtifacts`) was verified against the
firecracker provider on real hardware, with the probes built natively for amd64 and served from a
throwaway local TLS registry — so no registry credentials were needed to prove it.

What that run turned up, and what is now fixed in-tree:
- **`hack/netprobe/` did not exist.** `TestEgressEnforcement`'s probe — the one covering the
  *most* important security property — was never committed; it had been built ad hoc on the Mac.
  It is now written, documented, and green. Note its `KRAYT_ALLOW_HOST` contract: the run does not
  forward the network policy into the container, so the allowlisted host is baked into the image
  as an `ENV` and the test's `KRAYT_ALLOW_HOST` must match it.
- **`hack/hardening-probe` and `hack/ask-probe` hardcoded `GOARCH=arm64`**, which silently puts an
  arm64 binary inside an amd64 image (an exec-format failure at run time, not at build). Both now
  use `TARGETARCH`.

**What's left for you:** the probe images are only in a local registry on the test host. To make
this reproducible in CI (and it *can* run in CI now — unlike the vfkit path, a Linux runner with
`/dev/kvm` needs no special hardware), they need publishing multi-arch, the way `dev-image.yml`
already does for `krayt-dev`. Wiring `hack/*-probe` + `hack/netprobe` into a build/publish matrix
needs GHCR push credentials.

---

## [Phase 7] Real-agent (Claude Code) run on Linux — ✅ DONE

Verified by a human on the Linux/firecracker host: `run_26018248`, image
`ghcr.io/418-cloud/krayt-dev:sha-d315d9d` (the amd64 variant, selected automatically), real Claude
Code agent, `network: allowlist`, `questions_mode: wait` — exit 0 in 2m30s with a clean
`changes.patch` (+2/-0) and a full §8.4 `report.md` + `meta.json`.

Worth noting what that run incidentally proves, beyond "the adapter works": the agent had to reach
`api.anthropic.com` **through the egress proxy** to do anything at all. A real agent completing a
task is therefore a stronger end-to-end proof of the §6.6 allowlist than `hack/netprobe` is — the
allowlist had to *permit* the right host, not merely block the wrong ones.

Also confirmed by the human: krayt still works on Apple Silicon against the same pinned digest, so
the one multi-arch index resolves correctly on **both** backends. That is the whole point of the
§11.5 index pin, now demonstrated from both sides rather than argued.
