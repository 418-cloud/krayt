# Task: open linux/arm64 and Windows now that the backend-tagged image is gone

**Read `CLAUDE.md`, `docs/adr-microsandbox-sandbox-layer.md` ("What is actually at stake"), and
`KRAYT_SPEC.md` §12, §15 first.** Give a short plan and proceed. Depends on
`retire-vm-image-pipeline.md`. **Two independent deliverables** — Part A is small and unblocks a
platform today; Part B is a genuine port. Land A first; if B cannot be finished, finish A in full
and say exactly what B needs.

## Background

Two of B1's headline wins are platforms krayt could not reach:

- **linux/arm64** was blocked by the base VM image, not by krayt's code. The image index carried an
  arch dimension but no *backend* dimension (`images/flake.nix` built a PE `Image` for vfkit and an
  uncompressed ELF `vmlinux` for Firecracker), and the only published arm64 variant was the vfkit
  one — so a linux/arm64 krayt would resolve to it via `vmimage.selectPlatform` and fail to boot
  under Firecracker. That is why `.github/workflows/release-please.yml:52-70` carries a comment
  excluding it. **With no krayt VM image there is no index, no backend dimension and no conflict.**
- **Windows** krayt had no path to at all: two providers, one Apple-only and one KVM-only. msb
  supports Windows 11 with the Windows Hypervisor Platform, and its `--vsock` flag takes a local
  named pipe there instead of a unix socket.

## Part A — linux/arm64

### Decisions already made

1. **Add `linux/arm64` to the release cross-build matrix** in `release-please.yml`, and delete the
   comment block explaining why it was excluded — that reasoning is now history, not policy. The
   build stays `CGO_ENABLED=0` on one Linux runner.
2. **`make guest-bins` already builds `linux/arm64`** for the embedded helper and `krayt-ask`
   (`add-krayt-guest-helper.md`), so nothing changes there. Under msb the guest's architecture equals
   the host's, so an arm64 host selects the arm64 guest binary with no new dimension.
3. **CI gets an arm64 Linux job** for `go build`/`go test`, path-filtered like the existing
   `integration-linux` job, so the target is actually exercised rather than merely shipped.

### Done when (Part A)

- `release-please.yml` builds and uploads `krayt_<tag>_linux_arm64.tar.gz`, and `checksums.txt`
  covers it.
- `krayt upgrade` resolves the new asset — `internal/selfupdate`'s platform-to-asset mapping must
  know about it; a unit test against an `httptest` fixture covers this the way `krayt-upgrade.md`
  established.
- `README.md`'s supported-platforms table lists linux/arm64.
- §15's linux/arm64 open question is marked closed with the reason.
- **Hardware (`HUMAN_TODO.md`)**: one real `krayt run` on a linux/arm64 host with KVM.

## Part B — Windows

### Decisions already made

1. **This is a port, not a matrix line.** `GOOS=windows` currently fails on real things, and the
   plan must name them before writing code. At minimum:
   - `orchestrator.AcquireSlot` uses `syscall.Flock` (`internal/orchestrator/climit.go`), which does
     not exist on Windows. It needs a `LockFileEx`-backed equivalent behind the same API, in
     build-tagged files, keeping the property that matters: the lock is released when the holder's
     handle closes, including on crash, so slots never leak.
   - The `ask_human` host socket is a unix socket. On Windows msb takes a **local named pipe**
     (`\\.\pipe\name:PORT`), so `internal/askbridge`'s listener and the `KRAYT_ASK_SOCKET` value need
     a Windows form. The `vsock://cid:port` URL the guest side dials is unchanged — the guest is
     Linux either way, which is the reason this port is tractable at all.
   - Path handling in the run-state and cache directories, and `internal/cli/resources_*.go`, which
     has no Windows implementation.
   - `internal/patch` shells out to `git`; confirm the argument and path handling it assumes.
2. **The guest side does not change.** The sandbox is Linux under msb on every host. Nothing in
   `cmd/krayt-helper` or the agent images is platform-dependent.
3. **`krayt doctor` must cover the Windows prerequisites** by delegating to `msb doctor`, which
   already reports WHP availability and offers `--fix` (it opens an elevated PowerShell prompt to
   enable Windows Hypervisor Platform). Do not reimplement that check.
4. **Secret handling is weaker on Windows than on unix, and the plan must say so.** On unix the
   resolved secret value reaches the `msb sandbox` process over an **anonymous** temp file handed
   over as `--config-fd` — no filesystem path, nothing on argv (`sdk/rust/lib/runtime/spawn.rs`,
   `write_launch_config_fd`). Windows has no such handoff: `write_launch_config_file` writes the
   same launch config, resolved secret included, to a `NamedTempFile` **under the sandbox's runtime
   directory** and passes its **path on argv**. It is short-lived — dropped once the child reports
   startup — but it is a real on-disk write of secret material with a process-listing-visible path,
   on a platform where krayt cannot rely on `0700` unix modes to protect it. Found while reading
   msb 0.6.16 for `probe-microsandbox-feasibility.md` P4 (2026-08-30). Do not discover this during
   the port: state it in the plan, decide whether Windows support ships with it as a documented
   residual or waits for msb to close it, and put the answer in §10's threat table either way.
5. **Published ports are the one behaviour to call out.** msb's docs note that opening a published
   port on Windows can trigger a Windows Defender Firewall prompt for `msb.exe`. krayt publishes no
   ports, so this is inert — record it in §12 so the first person who adds a port does not discover
   it in a support thread.

### Done when (Part B)

- `GOOS=windows GOARCH=amd64 go build ./...` succeeds, and `release-please.yml` ships the tarball
  (or `.zip`, if that is the friendlier Windows artifact — decide and say why).
- `go test ./...` passes on a Windows runner in CI for the OS-agnostic packages, with the
  build-tagged Windows implementations of `AcquireSlot` and the ask listener covered by their own
  tests.
- §12 gains a Windows section; §15's Windows entry is marked closed.
- **Hardware (`HUMAN_TODO.md`)**: one real `krayt run` on Windows 11 with WHP, including an
  `--on-question=wait` run over the named-pipe ask channel.

## Out of scope

- darwin/amd64 beyond what already builds — msb's macOS support is Apple Silicon only, so an Intel
  Mac has no local backend. Say so in the README rather than shipping a binary that cannot run.
- Any change to the sandbox lifecycle, network translation or secrets handling.
