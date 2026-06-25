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

## [Phase 1] Fill guest-agent vendorHash in images/flake.nix
- Needed: replace `vendorHash = lib.fakeHash;` with the real Go-module dependency hash.
  This is NOT the nixpkgs narHash from the flake.lock message — it is the hash buildGoModule
  reports for the vendored deps.
- Why the agent can't: the hash is produced by a Nix build on aarch64-linux, which needs a
  Linux builder + the Nix binary cache (unavailable in the sandbox).
- Exact steps/commands: the package is aarch64-linux only, so address it explicitly (the
  short `#guest-agent` resolves to the host system, e.g. aarch64-darwin on a Mac, and 404s):
  ```
  nix build ./images#packages.aarch64-linux.guest-agent
  ```
  Nix prints `error: hash mismatch … got: sha256-…` — paste that `got:` value into
  `images/flake.nix`. This runs on CI's arm64 Linux runner as-is; on a Mac it needs an
  aarch64-linux builder — see `docs/macos-linux-builder.md` for setup (Determinate native
  builder or nix-darwin `linux-builder`), otherwise let CI compute it (the image workflow's
  first run surfaces the same mismatch).
- Verify success by: `nix build ./images#packages.aarch64-linux.guest-agent` succeeds.
- Blocking: yes — the VM image can't build until this is set. Precedes the image build.

## [Phase 1] Build + publish the VM image via CI (no merge to main needed)
- Needed: drive the `vm-image` workflow (`.github/workflows/image.yml`) entirely from the
  feature branch. The `build` job runs on any PR touching the image/guest sources; the
  `publish` job runs when you push a `v*` tag (tags can point at a branch commit, so main
  is never touched). Confirm repo Settings → Actions → Workflow permissions allow
  `packages: write` (the publish job uses `secrets.GITHUB_TOKEN`).
- Why the agent can't: building a NixOS aarch64 image needs a Linux runner + Nix binary
  cache (§11.3); publishing needs registry write. The sandbox has neither.
- Exact steps/commands: see the step-by-step in this file's sibling note below, or just:
  1. push the branch + open a (draft) PR → read the `vendorHash` from the failed build;
  2. paste it into `images/flake.nix`, push → build goes green;
  3. `git tag v0.0.0-rc1 && git push origin v0.0.0-rc1` → `publish` pushes to GHCR.
  Commit `images/flake.lock` too (already generated).
- Verify success by: the publish job's `::notice title=krayt-vmimage::` prints
  `PinnedRef=…` and `PinnedDigest=…`.
- Blocking: yes — the boot test needs a published image.

## [Phase 1] Pin the published image digest in internal/vmimage/pinned.go
- Needed: set `PinnedDigest` (and `PinnedRef` if the registry/owner differs) to the
  digest printed by the image workflow.
- Why the agent can't: the digest doesn't exist until CI builds + pushes the image.
- Exact steps/commands: edit `internal/vmimage/pinned.go`:
  `PinnedDigest digest.Digest = "sha256:<from CI>"`
- Verify success by: `krayt doctor` shows the base image as pinned; `krayt image pull`
  pulls and verifies it without the "no pinned digest" warning.
- Blocking: yes — without the pin, `image pull` runs unverified.

## [Phase 1] Boot test on real Apple-Silicon hardware (the "Done when")
- Needed: on a Mac with vfkit installed and the image pulled, run the gated integration
  test; confirm the VM boots and a `Hello` RPC round-trips host↔guest over the vfkit
  vsock socket (§14 Phase 1).
- Why the agent can't: needs real virtualization hardware (no nested virt / vfkit boot
  in a cloud agent) and a built VM image.
- Exact steps/commands:
  ```
  krayt image pull
  # locate the cached files (krayt doctor / the pull output), then:
  KRAYT_KERNEL=…/vmlinuz KRAYT_INITRD=…/initrd KRAYT_ROOTFS=…/rootfs.img \
    go test -tags 'integration darwin' -run TestBootHello -v ./internal/provider/vfkit/
  ```
- Verify success by: `TestBootHello` passes and logs `guest-agent ready: version=…`.
- Blocking: yes — this IS the Phase 1 completion criterion. Work pauses here.
