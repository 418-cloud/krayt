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

### Phase 2 — End-to-end single run (happy path) — DONE ✅
The Phase 2 "Done when" is met both ways: by the automated in-process proof
(`internal/orchestrator` `TestEndToEndRun` over the fakeProvider) **and** on real
Apple-Silicon hardware via the actual CLI path. On a real Mac,
`krayt run --image docker.io/tjololo/test-krayt:rc0 --task task.md --repo /tmp/test`
booted a micro-VM, imported the image into containerd on the per-run scratch disk, ran it,
and produced `run_980ab3c8/changes.patch` (creates `greeting.txt` = `edited`); `git apply
--check` passed and `krayt apply` landed it cleanly. **Phase 2 complete.**

Getting the real run green took three image iterations, all now resolved:
- **guest-agent vendorHash regenerated** — needed because Phase 2 added
  `github.com/containerd/containerd/v2/client` (§6.10) to the guest-agent's imports; the
  maintainer built on aarch64-linux and pasted the real hash into `images/flake.nix`.
- **`git bundle verify` ran outside a repo** (`need a repository to verify a bundle`) — fixed
  in `internal/patch` `verifyBundle` (runs from a throwaway bare repo); regression test
  `TestIngestOutsideGitRepo`.
- **`no space left on device` on image import** — the closure-sized rootfs had no room.
  Fixed with a per-run sparse **scratch disk** (`/dev/vdb`, default 20 GiB, wires `DiskGiB`)
  created by the vfkit provider and formatted + mounted at `/var/lib/containerd` by the new
  `krayt-scratch` systemd unit before containerd; the guest-agent `TMPDIR` points there too
  so the image tar + clone stay off RAM.

The base image also gained `gitMinimal` in the closure (§6.7) — the one addition §11.6's
closure list omits (flagged for the spec). The `integration,darwin` test
`TestEndToEndRealVM` remains available to re-verify the path in CI/automation.
