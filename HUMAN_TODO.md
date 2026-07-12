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

## [Phase 7] Egress / hardening / secrets integration tests on Linux need amd64 probe images

**Non-blocking.** `TestEgressEnforcement`, `TestContainerHardening`, `TestRootImageFailsClosed`,
`TestGuestGitConfigInjectionInert` and `TestSecretConfinementInArtifacts` now *build and run* on
Linux (they share the same `newTestProvider()` seam), but they **skip** without their probe
images, and the published ones (`hack/*-probe`) are **linux/arm64**, built for the Mac.

The `dev-image.yml` workflow already builds multi-arch (amd64 + arm64); the probe images in
`hack/` are not wired into it. Publishing them for amd64 would let the full Phase 3 security
suite re-verify on Linux, which is worth doing before anyone relies on the Linux backend for
untrusted code — the nftables `skuid "proxyd"` lock is guest-side and so should be
backend-independent, but "should be" is not "was tested".

I verified the Phase 2 end-to-end path only. That is exactly what Phase 7's "Done when" asks
for, and I am flagging the rest rather than implying broader coverage than I ran.
