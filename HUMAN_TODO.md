# HUMAN_TODO

Single handoff log for steps the coding agent cannot complete itself (credentials, real
hardware, a Linux builder, live secrets). Template per `KRAYT_SPEC.md` §14.

---

## Status by phase

### Phase 0 — Foundations
**No outstanding human steps.** Phase 0 is self-contained and verified:
`go build ./...`, `go vet ./...`, and `go test ./...` pass on macOS, the core + guest
cross-compile to `linux/arm64`, and the `Hello` RPC round-trips over the fake provider
(`internal/provider/fake`). CI (`.github/workflows/ci.yml`) re-runs the macOS + Linux
test matrix on push.

Resolved during Phase 0:
- **Protocol codegen via the pinned Nix toolchain** — maintainer ran `make proto`; the
  committed `internal/protocol/pb` now matches the canonical Nix path (`protoc v7.34.1`,
  `protoc-gen-go v1.36.11`, `protoc-gen-go-grpc v1.6.2`). Only the `protoc` version
  comment differed from the earlier sandbox-generated copy; the generated code is
  otherwise identical.
- **`flake.lock`** — generated (pins `nixpkgs` + `flake-utils`) and ready to commit
  alongside `flake.nix`.

### Phase 1 — Boot a VM on macOS
The Go code (vfkit provider, host control client, base-image pull, guest-agent binary)
is implemented and unit-tested cross-OS. The remaining items need a Linux builder,
registry credentials, and real Apple-Silicon hardware — the last one is the phase's
"Done when" and is **blocking**, so work pauses there.

## [Phase 1] Install vfkit on the Mac
- Needed: `brew install vfkit` on the Apple-Silicon Mac used for runs.
- Why the agent can't: package install on your machine; trivial and scriptable.
- Exact steps/commands: `brew install vfkit && krayt doctor`
- Verify success by: `krayt doctor` shows `[ok] vfkit installed + runnable`.
- Blocking: no — only needed for the boot test below.

## [Phase 1] Fill guest-agent vendorHash in images/flake.nix — DONE
- Resolved: `vendorHash` is set to `sha256-JNdn1OQB/IhnG+NAmgmwn/2PztEwE4zL7C4nIGOMXs8=`
  (the `got:` value from the CI build's hash mismatch). The `go-modules` derivation now
  builds. To regenerate after changing Go deps: set it back to `lib.fakeHash`, build, and
  paste the new `got:` hash. Build runs on aarch64-linux — see `docs/macos-linux-builder.md`
  for a local builder, or let CI compute it.

## [Phase 1] Build + publish the VM image via CI — DONE
- Resolved: the `vm-image` workflow builds and publishes to GHCR
  (`ghcr.io/418-cloud/krayt-vmimage`). The boot-tested image is `v0.0.0-rc5`,
  digest `sha256:97da098e67af271bab29721cdbbaf9f03e6d604d3271983c689792c21e474dad`
  (rc1–rc4 were earlier iterations while debugging the boot — see the boot-test entry).
  Commit `images/flake.lock` if not already.
- Note: confirm the GHCR package is set **public** (or that the boot-test host can
  authenticate) so `krayt image pull` can fetch it.

## [Phase 1] Pin the published image digest in internal/vmimage/pinned.go — DONE
- Resolved: pinned by digest to the boot-tested image (v0.0.0-rc5) —
  `PinnedRef = ghcr.io/418-cloud/krayt-vmimage@sha256:97da098e…74dad` and
  `PinnedDigest = sha256:97da098e…74dad`. `krayt doctor` reports it pinned (cached after
  `krayt image pull`).

## [Phase 1] Boot test on real Apple-Silicon hardware (the "Done when") — DONE ✅
- Resolved: on a real Apple-Silicon Mac with vfkit, `TestBootHello` passed — the VM
  (image v0.0.0-rc5, digest `sha256:97da098e…74dad`) booted and a `Hello` RPC
  round-tripped host↔guest over the vfkit vsock socket in ~11s
  (`guest-agent ready: version=0.0.0-dev`). **Phase 1 "Done when" met.**
- Getting here took several image iterations (all in `images/flake.nix`): short socket
  paths (macOS 104-byte limit), rootfs skeleton + `/nix/var/nix/profiles/system`, scripted
  initrd instead of systemd-initrd, and a `/init` symlink for the scripted stage-2 target.

### Phase 2 — End-to-end single run (happy path)
The host-side + OS-agnostic work is implemented and proven by an automated test: the Phase 2
"Done when" is met in-process via the fakeProvider (`internal/orchestrator`
`TestEndToEndRun`) — the real bundle→clone→baseline→diff→collect round-trip runs, a stand-in
agent edits one file, and the resulting `changes.patch` applies cleanly with `krayt apply`.
The container runtime (containerd) and the real boot are the only pieces that need
hardware; those are the handoffs below. The first three **block** the real-VM confirmation
(the last entry); the automated proof does not depend on them.

## [Phase 2] Regenerate guest-agent vendorHash — BLOCKING (image build)
- Needed: a fresh `vendorHash` in `images/flake.nix` for the `krayt-agent` `buildGoModule`.
- Why the agent can't: Phase 2 added `github.com/containerd/containerd/v2/client` (§6.10) to
  the guest-agent's imports, changing its vendored module set. Computing the Nix vendor hash
  needs Nix on aarch64-linux; it cannot be derived on macOS / in a cloud agent. `vendorHash`
  is currently `lib.fakeHash` so the build fails fast with the correct hash.
- Exact steps/commands: build on the arm64 Linux runner (or `nix build .#guest-agent`); copy
  the `got: sha256-…` value from the hash-mismatch error into `vendorHash`.
- Verify success by: `nix build .#guest-agent` succeeds on aarch64-linux.
- Blocking: yes — the VM image cannot build until this is set.

## [Phase 2] Rebuild + republish the base VM image, then re-pin the digest — BLOCKING (real run)
- Needed: a new base image build that includes (a) the containerd-wired guest-agent,
  (b) `git` in the closure (`gitMinimal` on the `krayt-agent` service path, §6.7), and
  (c) the new `krayt-scratch` service that formats + mounts the per-run scratch disk
  (`/dev/vdb`) at `/var/lib/containerd` before containerd, with the guest-agent `TMPDIR`
  pointed there (§6.10). Publish to GHCR and update `internal/vmimage/pinned.go`.
- Why the agent can't: Linux builder/CI + registry credentials + real-hardware boot.
- Note: `vendorHash` does NOT need regenerating for the scratch-disk / `patch.go` changes —
  no Go dependency changed (only the earlier containerd addition required it, now done).
- Exact steps/commands: run the `vm-image` workflow → capture the published digest → set
  `PinnedRef`/`PinnedDigest` → `krayt image pull`.
- Verify success by: `krayt doctor` shows the image pinned + cached; `TestBootHello` still
  round-trips `Hello`; a `krayt run` against a trivial image imports it without
  `no space left on device` and yields a non-empty `changes.patch`.
- Blocking: yes — the real-VM e2e needs a guest that can import containers, run git, and has
  disk space for the image.

## [Phase 2] Provide a trivial user OCI image that edits one file — BLOCKING (real run)
- Needed: a small `linux/arm64` OCI image whose entrypoint edits a file under `/workspace`
  and exits 0 (the "trivial image that edits one file" of the Phase 2 "Done when").
- Why the agent can't: building/publishing an image needs a registry + builder; krayt does
  not build user images (Non-Goal §2).
- Exact steps/commands: e.g. a Dockerfile whose CMD is
  `sh -c 'echo edited >> /workspace/greeting.txt'`; push to a registry the host can pull.
- Verify success by: `krayt run --image <ref> --task task.md --repo <repo>` yields a
  non-empty `changes.patch`.
- Blocking: yes — for the real-VM e2e only (the automated proof uses a fake runner instead).

## [Phase 2] Real-VM end-to-end "Done when" on Apple-Silicon hardware
- Needed: run `internal/orchestrator` `TestEndToEndRealVM` (build tag `integration,darwin`)
  on a Mac with vfkit, the republished base image, and the trivial user image above.
- Why the agent can't: needs virtualization hardware + a real containerd in the VM.
- Exact steps/commands:
  `KRAYT_KERNEL=…/vmlinuz KRAYT_INITRD=…/initrd KRAYT_ROOTFS=…/rootfs.img
  KRAYT_IMAGE=<trivial-image-ref>
  go test -tags 'integration darwin' -run TestEndToEndRealVM -v ./internal/orchestrator/`
- Verify success by: the test passes (boot → run → `changes.patch` applies to the host repo).
- Blocking: no for the phase's automated proof (already green); yes for the on-hardware
  confirmation. Depends on the three entries above.
