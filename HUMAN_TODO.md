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
firecracker provider, plus Hello, guest-network and 3-way concurrency checks. Notably this phase
needed *no* human hardware handoff: unlike the vfkit path, a Linux/KVM host can be driven by the
coding agent directly.

The detailed phase-by-phase and finding-by-finding history that used to live in this file has been
pruned to keep it short and current — it was all resolved, and the record of *how* lives in `git
log`/PR history and `docs/ai-tasks/README.md`, not here. This file only tracks what's still open.

---

## [Phase 7] Publish the multi-arch base VM image and re-pin

**Blocking:** `krayt run` / `krayt image pull` on Linux. **Not** blocking Phase 7's "Done when"
(the integration tests take the image via `KRAYT_KERNEL`/`KRAYT_INITRD`/`KRAYT_ROOTFS`, so they
run against a locally-built image and pass today).

**What's wrong:** the currently pinned digest (`sha256:a0c489cd…`) is the **aarch64** artifact.
On x86_64 that pull yields an arm64 kernel, and Firecracker fails on it in a thoroughly confusing
way.

**The code side is done and verified.** `internal/vmimage` now resolves a multi-arch OCI index to
the host's architecture at pull time, so pinning stays **one ref + one digest with no architecture
in it** — the index's — rather than needing per-`GOARCH` constants. Verified end to end against a
real registry: index digest pinned → `krayt image pull` → correct amd64 artifact on disk (the
arm64 entry provably not fetched) → boots and answers Hello. Unit-covered by
`TestPullSelectsHostArchFromIndex` / `TestPullRejectsIndexWithoutHostArch`; a pre-index
single-arch artifact still pulls unchanged, so nothing breaks in the meantime.

`.github/workflows/image.yml` builds both arches on native runners, pushes each with an OCI image
config declaring its platform, gathers them into one index, and asserts the index entries actually
carry `platform` before printing the digest to pin.

**What you need to do:** run the `vm-image` workflow (tag, or `workflow_dispatch` with
`publish: true`) — it needs GHCR push credentials, which I don't have — then paste the index
digest from its `::notice` output into `internal/vmimage/pinned.go` (`PinnedRef` + `PinnedDigest`,
both the same index digest). That is the whole change.

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

## [Phase 7] Real-agent (Claude Code) run on Linux — needs a token

**Non-blocking.** `krayt-dev` is already built multi-arch by `dev-image.yml`, so an amd64 image
should exist, but actually running Claude Code inside it needs a live
`CLAUDE_CODE_OAUTH_TOKEN` / `ANTHROPIC_API_KEY` (§6.14). Deliberately left as a human step.

This is the Phase 5/6 dogfood path (real agent → patch + report + meta; `ask_human` MCP round-trip
→ `krayt answer`), which is verified on Apple Silicon but not yet on Linux/firecracker. The
machinery it rides on — container runtime, egress, secrets, question channel — is all green on
Linux, so this is expected to work; it has simply not been run.
