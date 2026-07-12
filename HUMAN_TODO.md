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

## [Phase 7] Publish the x86_64 base VM image + make the pinned digest per-arch

**Blocking:** `krayt run` / `krayt image pull` on Linux. **Not** blocking Phase 7's "Done when"
(the integration tests take the image via `KRAYT_KERNEL`/`KRAYT_INITRD`/`KRAYT_ROOTFS`, so they
run against a locally-built image and pass today).

**What's wrong:** `internal/vmimage/pinned.go` pins a *single* digest, and it is the **aarch64**
artifact (`ghcr.io/418-cloud/krayt-vmimage@sha256:a0c489cd…`). There is no notion of architecture
in it. On an x86_64 Linux host, `krayt image pull` therefore fetches the arm64 image and `krayt
run` hands Firecracker an arm64 kernel, which fails in a thoroughly confusing way. `krayt doctor`
currently reports only "base VM image not cached".

**Why I didn't just fix it:** it means changing `internal/vmimage` (+ the `image pull/ls/prune`
commands that consume it), which is the OS-agnostic core I was asked to leave alone. It needs a
decision, then a publish.

**What I already did:**
- `images/flake.nix` builds **both** systems (`aarch64-linux`, `x86_64-linux`) from one config.
- `.github/workflows/image.yml` is now an arch matrix: it builds on native arm64 **and** x86_64
  runners and pushes arch-suffixed tags (`…:<tag>-arm64`, `…:<tag>-amd64`), printing each digest
  as a `::notice`.

**What you need to do:**
1. Run the `vm-image` workflow (tag or `workflow_dispatch` with `publish: true`) — it needs GHCR
   push credentials, which I don't have.
2. Decide the pinning shape (suggestion: `PinnedRef`/`PinnedDigest` become per-`GOARCH`, with a
   `Pinned()` accessor that fails closed with a clear message on an arch with no published image,
   rather than silently pulling the wrong one).
3. Paste both digests from the workflow's `::notice` output into `internal/vmimage/pinned.go`.

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
