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

### Phase 3 — Security & capability controls — DONE ✅
Confirmed end-to-end on Apple Silicon: `TestEgressEnforcement` passed against a rebuilt image
(`ghcr.io/418-cloud/krayt-vmimage@sha256:d3f2991b…`) + the `test-krayt:network` probe under
`--net allowlist --allow api.anthropic.com` — **PASS 1** reached the allowlisted host through
the proxy, **PASS 2** the non-allowlisted host was 403'd, **PASS 3** the raw `1.1.1.1:443`
socket was dropped by nftables. `TestBootHello` and `TestEndToEndRealVM` also re-passed on the
final image (no regression). Secrets redaction, wall-clock timeout, include-dirty, and the
proxy L7 allowlist were already green in the automated suite. **Phase 3 complete.**

The OS-agnostic work is implemented and proven by automated tests (no VM):
- **Secrets + redaction (§6.8):** `internal/orchestrator` `TestSecretsRedactedInLogs` — a
  secret is mounted at `/run/secrets` for the agent but is scrubbed from the live log,
  `agent.log`, and `meta.json`.
- **Egress allowlist L7 (§6.6):** `internal/guest/proxy` tests — allowlisted host allowed,
  non-allowlisted blocked, `none` blocks all, `full` allows all (hand-rolled proxy behind a
  swappable `Factory` seam).
- **Wall-clock timeout (§6.1):** `TestRunTimeout` — a stuck agent is killed, the run records
  `timed_out: true`, the VM is torn down.
- **Include-dirty (§6.7):** `internal/patch` tests — uncommitted/untracked captured,
  `.gitignore` honored, source repo untouched, unborn-HEAD handled.

The L3 nftables lock (raw-socket block) is the one piece that needs a real VM. The handoffs
below block only that on-hardware confirmation.

## [Phase 3] Rebuild + republish the base VM image (Phase 3 changes), re-pin the digest — BLOCKING (real run)
- Needed: rebuild with the Phase 3 image additions and publish to GHCR, then update
  `internal/vmimage/pinned.go`. Image changes (all in `images/flake.nix`): a `proxyd`
  system user/group (§6.6), the `krayt-proxy` binary (added to the guest-agent
  `buildGoModule` subPackages), and `nftables` + `krayt-proxy` on the `krayt-agent` service
  PATH. Plus the updated guest-agent (secrets/redaction/timeout/network wiring).
- Why the agent can't: Linux builder/CI + registry credentials + real-hardware boot.
- Note: `vendorHash` does NOT change — the proxy is stdlib and secrets is first-party; no new
  Go module was added.
- **Host-netns fix (a second rebuild):** the first attempt at a real egress run surfaced a
  runner bug — the container was getting a fresh empty network namespace, so it had no route
  to the proxy and no egress at all. `internal/guest/runner/containerd_linux.go` now runs the
  container in the VM's own netns (`oci.WithHostNamespace(specs.NetworkNamespace)`, §6.6). Any
  image built before this fix must be rebuilt for the egress path to work; `vendorHash` is
  still unchanged. The `pinned.go` comment was also stale (still referenced the Phase 1
  rc5 image) — update it when re-pinning.
- **Proxy DNS fix (a third rebuild):** the on-hardware egress run then showed a 502 reaching
  an allowlisted host — the nftables lock was dropping `proxyd`'s DNS because the system stub
  resolver (`systemd-resolved`) does the upstream lookup as a *different* uid. The proxy now
  resolves via `DefaultDNSServer` (1.1.1.1:53) dialed as `proxyd`, so DNS is `proxyd`-owned
  and permitted by the lock while the container stays fully DNS-blocked (§6.6). Overridable
  with `krayt-proxy --dns`. `vendorHash` unchanged. Confirmed via a `--net full` run (which
  reaches the host, proving the network is fine and the failure was the lock/DNS path).
- Verify success by: `TestBootHello` still round-trips; `TestEndToEndRealVM` still passes
  (no regression from the Phase 3 wiring).
- Blocking: yes — the egress + secrets on-hardware tests need this image.

## [Phase 3] Provide a linux/arm64 network-probe image for the egress test — BLOCKING (egress run)
- Needed: an image whose entrypoint probes egress and exits 0 ONLY when all three hold:
  (a) HTTPS to `$KRAYT_ALLOW_HOST` via `HTTPS_PROXY` succeeds; (b) HTTPS to a
  non-allowlisted host fails; (c) a raw TCP connect that ignores `HTTP(S)_PROXY` to a
  non-allowlisted `host:443` fails (the nftables L3 lock). Otherwise exit non-zero.
- Why the agent can't: building/publishing an image needs a registry + builder; krayt does
  not build user images (Non-Goal §2).
- Exact steps/commands: e.g. a small script using `curl` (honors `HTTPS_PROXY`) for (a)/(b)
  and a raw `nc`/socket connect for (c); push to a registry the host can pull.
- Verify success by: the run exits 0 under `--net allowlist --allow $KRAYT_ALLOW_HOST`.
- Blocking: yes — for the egress integration test only.

## [Phase 3] Run the egress enforcement test on Apple-Silicon hardware — the L3 raw-socket proof
- Needed: run `internal/orchestrator` `TestEgressEnforcement` (build tag `integration,darwin`)
  on a Mac with vfkit, the republished image, and the network-probe image above.
- Why the agent can't: needs real virtualization + nftables + network egress.
- Exact steps/commands:
  `KRAYT_KERNEL=…/vmlinuz KRAYT_INITRD=…/initrd KRAYT_ROOTFS=…/rootfs.img
  KRAYT_NETPROBE_IMAGE=<ref> KRAYT_ALLOW_HOST=api.anthropic.com
  go test -tags 'integration darwin' -run TestEgressEnforcement -v ./internal/orchestrator/`
- Verify success by: the test passes (allowlisted reach + non-allowlisted block + raw-socket
  block all as expected).
- Blocking: no for the phase's automated proofs (secrets/timeout/include-dirty/proxy-L7 are
  green); yes for the on-hardware L3 confirmation. Depends on the two entries above.

## [Phase 4] Verify the agent-question bridge socket + `krayt answer` on hardware — DONE ✅
- Confirmed on Apple Silicon with the `hack/ask-probe` image (`docker.io/tjololo/test-krayt:ask`):
  (a) the guest opened the bridge and the runner bind-mounted it — probe logged
  `/run/krayt/ask.sock present (mode Srwxr-xr-x)`; (b) `--on-question=wait` drove the run to
  `waiting` and `krayt ls` **correctly showed `STATE=waiting`** during the wait (this is the
  fix — it wrongly showed `running` before removing the log-as-resume heuristic); (c)
  `krayt answer run_c63ca3fa yes` from a second terminal dialed the recorded `ctrl_socket` and
  resolved it (`answered … question q1`) — the run completed exit 0 with the answer in the
  patch. (d) The desktop notification path was not separately confirmed in this run; low risk.
- Full `--on-question*` matrix also confirmed on hardware: `--question-timeout 30s` with the
  default `sentinel` → the probe got `no_answer=true` and proceeded (exit 0); with
  `--on-question-timeout abort` → the run failed cleanly (`question timed out (abort policy,
  §6.13)`) and the VM was torn down. The self-correcting timer (`armQuestionTimeout`) worked.
- Interim state behavior (by design until the Phase-5 guest "question resolved" event): a run
  stays `waiting` until it reaches its terminal state rather than flipping back to `running`
  on answer. Original text kept below for reference.
- Needed: on an Apple-Silicon Mac with vfkit + the base image, confirm the container-facing
  ask bridge works end to end: (a) the guest opens `<root>/ask.sock` and the containerd runner
  bind-mounts it at `/run/krayt/ask.sock` in the container; (b) a `--on-question=wait` run whose
  container connects to that socket drives the run to `waiting`; (c) `krayt answer <id> <resp>`
  from a second terminal dials the recorded `ctrl_socket` and resolves it; (d) a desktop
  notification fires on macOS (`osascript`).
- Why the agent can't: the socket path needs a real VM + containerd bind mount, and the
  sandbox blocks `bind(2)` for unix sockets (the socket round-trip test `t.Skip`s here);
  cross-process `krayt answer` needs a live guest to dial.
- Exact steps/commands: use the ready-made probe in `hack/ask-probe/` (self-contained
  static image + full runbook in its README) — build/push it, `krayt run --on-question=wait`,
  observe `krayt ls` show `waiting`, then `krayt answer <id> yes`. (Or wait for the Phase-5
  `krayt-ask`/MCP front-ends, which supersede the probe.)
- Verify success by: `krayt ls` flips `waiting`→`running`→`done`; the run dir has
  `questions/<qid>.json`; the patch reflects the answered decision.
- Blocking: no — the channel is fully proven against the fakeProvider in-process
  (`TestQuestionWaitAnswer`, `TestQuestionFailModeSentinel`); this confirms the real socket
  transport + notification, and is naturally exercised once the Phase-5 front-ends land.

## [Phase 4] Rebuild + re-pin the VM image (guest-agent changed) — DONE ✅
- Resolved: the base image was rebuilt with the Phase-4 guest-agent and re-pinned; the
  `hack/ask-probe` run proves it (the new bridge socket existed in-VM, so the rebuilt
  guest-agent booted). `vendorHash` regenerated to
  `sha256-7NUdYBWhMvs+nJlHyoBWFzMYA83JXVyW6skWIB2T0Ws=` in `images/flake.nix` (adds
  gopkg.in/yaml.v3). If the new `internal/vmimage/pinned.go` digest isn't recorded here yet,
  it lives in that file. NOTE: the `waiting`-state host fix that followed is host-only
  (`bin/krayt`) and needs **no** further image rebuild.
- Needed: Phase 4 changed the guest-agent baked into the base image — the ask bridge
  (`internal/guest/ask`), the `Answer` RPC, the serialized `eventSender`, and the containerd
  ask-socket mount (§6.13). Also `gopkg.in/yaml.v3` was added to the module (config loader,
  §8.1), so the guest-agent `vendorHash` in `images/flake.nix` must be regenerated.
- Why the agent can't: nix build needs an aarch64-linux builder (CI or a Mac linux-builder);
  no Nix in the cloud sandbox. Cannot compute vendorHash or produce the image here.
- Exact steps/commands:
  1. In `images/flake.nix`, set `vendorHash = pkgs.lib.fakeHash;`, build the guest-agent,
     and paste the reported `got: sha256-…` back into the `vendorHash` field.
  2. Rebuild the VM image (kernel+initrd+rootfs), push/publish it.
  3. Update `internal/vmimage/pinned.go` with the new image digest (same as Phase 2/3).
- Verify success by: `TestBootHello` + `TestEndToEndRealVM` still pass on hardware; then the
  `hack/ask-probe` confirmation (above) runs.
- Blocking: yes — every on-hardware run (incl. the ask-probe confirmation) needs the rebuilt,
  re-pinned image; the automated fakeProvider proofs do not.

## [Phase 5] `krayt-ask` container placement + image rebuild
- Needed: (1) rebuild + re-pin the VM image so it includes the new `krayt-ask` binary —
  `images/flake.nix` now builds `cmd/krayt-ask` into the guest-agent derivation
  (`${guest-agent}/bin/krayt-ask`); `vendorHash` is **unchanged** (no new module deps).
  (2) Bind-mount that binary into the container on `PATH` so an agent can invoke `krayt-ask`,
  and wire the guest to do it (mirrors the ask-socket mount in
  `internal/guest/runner/containerd_linux.go`: add a `RunConfig.AskBinary` path, have the
  Service resolve it next to the guest-agent executable, and mount it read-only).
- Why the agent can't: needs an aarch64-linux Nix builder (no Nix in the sandbox) to rebuild
  the image; and the container mount destination is image-dependent (a `scratch`/distroless
  image has no `/usr/local/bin`), so the exact `PATH` placement must be chosen and validated
  against a real image — the fakeProvider runner does not perform mounts.
- Exact steps/commands:
  1. Decide the mount destination. Recommended: mount at `/run/krayt/bin/krayt-ask` (a dir
     krayt already owns) and prepend `/run/krayt/bin` to the container `PATH` (or set the
     adapter env `KRAYT_ASK_BIN`), avoiding reliance on `/usr/local/bin` existing.
  2. Implement the mount + `RunConfig.AskBinary` wiring in the guest runner; the host path is
     `filepath.Join(filepath.Dir(os.Executable()), "krayt-ask")` inside the VM.
  3. Rebuild + re-pin the image (same procedure as the Phase 4 image entry above).
- Verify success by: in a `--on-question=wait` run, `krayt-ask "…"` inside the container
  prints the answer supplied by `krayt answer` (exit 0); with `--on-question=fail`, it exits 2
  immediately. The `hack/ask-probe` runbook can be extended to shell out to `krayt-ask`.
- Blocking: no — the `krayt-ask` binary, its client logic, the exit-code contract, and the
  adapter's `KRAYT_ASK_SOCKET` wiring are all proven host-side
  (`cmd/krayt-ask` tests + `internal/cli` adapter tests); only the in-container round-trip on
  real hardware is deferred.

## [Phase 5] Agent adapter end-to-end with live credentials — DONE ✅
- Resolved: verified on Apple Silicon with `docker.io/tjololo/test-krayt:claude`. A
  `krayt run --agent claude-code --secrets … --allow api.anthropic.com` completed a real coding
  task (add `hello()` + a pytest test + README note): Claude Code authenticated via
  `CLAUDE_CODE_OAUTH_TOKEN`, the run reached `done` (exit 0), and the run dir had `changes.patch`
  (3 files, +12/-0), `report.md` (with Claude's summary under Notes), and `meta.json`; the token
  never appeared in any of them; `krayt apply` landed the patch cleanly. (Still worth a one-off:
  the exactly-one guard rejecting a two-credential file before boot — proven by
  `TestApplyAdapterAuthGate`.)
- Needed: exercise a real agent image (`--agent claude-code`) with a live credential in the
  secrets file, confirming the container entrypoint exports the resolved credential from
  `/run/secrets` into the environment (§8.2/§6.14) and the agent authenticates and runs.
- Why the agent can't: needs a live `ANTHROPIC_API_KEY` (or `CLAUDE_CODE_OAUTH_TOKEN`) and a
  real agent image on hardware; the sandbox has neither.
- Exact steps/commands: use the ready-made image + full runbook in **`hack/claude-code/`**
  (Dockerfile installs Claude Code, runs it non-root headlessly, exports the credential from
  `/run/secrets`). Build/push it (`docker buildx build --platform linux/arm64 -t <ref> --push .`),
  then `krayt run --agent claude-code --secrets ./secrets.env --image <ref> --task ./task.md
  --allow api.anthropic.com`; put exactly one of `ANTHROPIC_API_KEY` / `CLAUDE_CODE_OAUTH_TOKEN`
  in `secrets.env`. Confirm the run fails fast (before boot) if both are present.
- Verify success by: the run completes with a patch + report + meta; the exactly-one guard
  rejects a two-credential secrets file with a clear error (`§6.14`) before any VM boots.
- Blocking: no — the host-side auth gate + `krayt-ask` env wiring are proven
  (`TestClaudeCodeExactlyOne`, `TestApplyAdapterAuthGate`, `TestApplyAdapterWiresAsk`); the
  live-credential run is part of the Phase 5 "Done when".

## [Phase 5] Detached "park and walk away" — end-to-end on hardware — DONE ✅
- Resolved: verified on Apple Silicon with the `hack/ask-probe` image
  (`docker.io/tjololo/test-krayt:ask`). `krayt run --detach --on-question=wait
  --question-timeout 120s --on-question-timeout abort` returned immediately printing the run id
  + supervisor pid (34206); `krayt ls` showed the background run `starting`, then `waiting` once
  the probe hit `/run/krayt/ask.sock`; `krayt answer run_afbb910f yes` from a **separate shell**
  resolved `q1` (the supervisor outlived the launcher); a re-`attach` showed the probe receive
  `response="yes"`, write its decision file, and finish `done: success`. Confirms the
  session-detached supervisor + cross-process `krayt answer` + `waiting` persistence on a real
  VM. Still open (their own HUMAN entries): the `krayt-ask` **binary** round-trip (this used the
  probe's own socket client) and a real agent with live keys; a `--max-concurrency` queue check
  across invocations is still worth a quick pass but the primitive is proven
  (`TestAcquireSlotCrossProcess`).
- Needed: confirm the whole detached flow on a real VM: `krayt run --detach …` returns
  immediately, the run keeps executing after the launching terminal closes, its `waiting`
  question fires a notification, and `krayt answer <id> <resp>` from a **separate** invocation
  resolves it; then `krayt stop <id>` (SIGTERM to the recorded supervisor pid) tears the VM
  down. Also sanity-check `--max-concurrency N` queues the N+1-th run across separate
  `krayt run` invocations.
- Why the agent can't: needs a bootable VM (vfkit + the pinned image) on Apple Silicon; the
  sandbox has no VM, and the detached child re-execs `krayt run`, which calls `newRunDeps()`
  (vfkit) — unavailable here.
- Exact steps/commands: `krayt run --detach --on-question=wait --image <img> --task ./task.md`;
  note the printed run id; close the terminal; from a new shell `krayt ls` (shows `waiting`),
  `krayt answer <id> <resp>`, `krayt attach <id>`. For the limit: launch 3× `krayt run --detach
  --max-concurrency 1 …` and confirm only one boots at a time (`krayt ls`).
- Verify success by: the supervisor process (its pid is printed and in `meta.json`) outlives the
  launcher; the run reaches `done` with patch+report+meta; the second/third `--max-concurrency
  1` runs stay queued until the first finishes.
- Blocking: no — the mechanism is proven host-side: cross-process limit
  (`TestAcquireSlotCrossProcess`, real subprocesses), the session-detached spawn
  (`TestSpawnDetached`), and the file-lock cap (`TestMaxConcurrency`). Only the on-VM run is
  deferred.

## [Phase 5] Rebuild VM image for the non-root container-filesystem fixes — DONE ✅
- Resolved: base image rebuilt + re-pinned with all four non-root fixes; verified on Apple
  Silicon by a real `docker.io/tjololo/test-krayt:claude` run (uid 1000 `agent`): Claude Code
  authenticated via `CLAUDE_CODE_OAUTH_TOKEN`, edited `/workspace` (main.py + new test_main.py +
  README.md), wrote `/output/report.md`, and the run reached `done` (exit 0) with a clean
  `changes.patch` that `krayt apply` landed. (The ask-socket fix is shipped but not yet exercised
  — needs a `--on-question=wait` + `krayt-ask` run.)
- Needed: rebuild + re-pin the base VM image so the guest-agent makes the container-contract
  paths usable by a **non-root** container (§8.2 requires non-root; Claude Code refuses uid 0).
  All fixed in `internal/guest/service.go`, found by testing the `claude-code` image, each with a
  regression test:
  1. **`/run/secrets`** world-readable (dir `0755`, files `0644`) — else exit 78 "no credential".
     (`writeSecrets`; `TestSecretsRedactedInLogs`.)
  2. **`/workspace`** made writable after ingest (`makeContainerWritable`: g+o rw, dirs +x) — else
     the agent can't edit/create files. `.git` stays root-owned so the guest's own git is fine.
  3. **`/output`** `0777` — else the agent can't write `report.md` (`tee: Permission denied`).
  4. **ask socket** `0777` — else a non-root agent can't connect to `/run/krayt/ask.sock` (§6.13).
  (2–4 proven by `TestEndToEndRun`'s writability asserts.)
- Why the agent can't: the guest-agent is baked into the Nix rootfs; changing it needs an
  aarch64-linux Nix build (no Nix in the sandbox), same as the Phase 4 image rebuild.
- Exact steps/commands: rebuild the image (guest-agent picks up the fixes — `vendorHash`
  unchanged, no new deps), re-pin `internal/vmimage/pinned.go` (same procedure as the Phase 4
  entry above), push/publish. Also rebuild/push the `hack/claude-code` **image** (its entrypoint
  now sets `git safe.directory` so the agent's own git tolerates the root-owned `.git`).
- Verify success by: re-run the `hack/claude-code` demo (non-root image) — it prints
  `authenticated via …`, Claude edits `/workspace`, writes `/output/report.md`, and the run
  reaches `done` with a real `changes.patch`. Inside the container, `ls -la /run/secrets` shows
  `-rw-r--r--` and `/workspace` is writable.
- Stopgap (no base rebuild): run the container as **root** — drop `USER agent` from
  `hack/claude-code/Dockerfile` and add `ENV IS_SANDBOX=1` (lets Claude Code tolerate root with
  `--dangerously-skip-permissions`) — then rebuild/push just the test image. Root sidesteps all
  four perms issues. Revert to non-root once the base image has the fixes.
- Blocking: partially — a **non-root** agent image (the §8.2 contract, incl. Claude Code) can't
  read secrets / write the workspace until this ships; root images (e.g. `ask-probe`) are
  unaffected. Host-side proof is in place; only the on-VM confirmation waits on the rebuild.

## [Phase 5] Rebuild VM image to ship the krayt-ask CLI front-end — DONE ✅
- Resolved: shipped in base image **v0.0.0-rc16** (`pinned.go` digest `01b32a57…`) and verified on
  Apple Silicon with `docker.io/tjololo/test-krayt:krayt-ask` (non-root, uid 1000). A
  `--on-question=wait` run drove the container to shell out to `krayt-ask` (found on PATH at
  `/usr/local/bin/krayt-ask`), reach `waiting`, and — after `krayt answer <id> yes` from a second
  shell — log `got answer: yes` and finish `done` (exit 0) with `changes.patch` adding
  `krayt-ask-decision.txt` = `yes`. Closes the last Phase-5 "Done when" clause. Also confirmed
  `--on-question=fail`: `krayt-ask` gets the no-answer sentinel immediately and the agent proceeds
  autonomously (`krayt-ask-decision.txt` = `no-answer-sentinel`) — the default stays non-blocking.
- Needed: rebuild + re-pin the base VM image so it (1) contains the `krayt-ask` binary
  (`flake.nix` builds `cmd/krayt-ask` into the guest-agent derivation) and (2) bind-mounts it
  into the container at `/usr/local/bin/krayt-ask` (guest resolves it next to the guest-agent and
  passes `RunConfig.AskBinary`; the runner mounts it read-only). This closes the last Phase-5
  "Done when" clause — an agent's `krayt-ask` call round-tripping to `krayt answer`.
- Why the agent can't: the guest-agent + the mount are baked into the Nix rootfs; needs an
  aarch64-linux Nix build (no Nix in the sandbox), same as the other image rebuilds. `vendorHash`
  unchanged (krayt-ask imports only `internal/guest/ask` + stdlib, already vendored).
- Exact steps/commands: rebuild the image, re-pin `internal/vmimage/pinned.go`, push/publish; then
  build/push the ready-made **`hack/krayt-ask-probe`** image (non-root; shells out to `krayt-ask`)
  and follow its README — `krayt run --on-question=wait`, then `krayt answer <id> yes` from a
  second shell.
- Verify success by: the probe logs `got answer: yes`, the run reaches `done`, and
  `changes.patch` adds `krayt-ask-decision.txt` = `yes`. In `fail` mode it exits 0 with
  `no-answer-sentinel`. (Resolution logic proven host-side by `TestAskBinaryIn`.)
- Blocking: no other Phase-5 work depends on it, but this is the final clause needed to mark the
  Phase 5 "Done when" fully complete.

## [Phase 6] Rebuild VM image for precise resume + the ask_human MCP server — DONE ✅
- Resolved: shipped in base image **v0.0.0-rc17** (`pinned.go` digest `149aab02…`) and verified on
  Apple Silicon with `docker.io/tjololo/test-krayt:claude` (non-root, `--on-question=wait`). Given
  a task with a genuine DB choice, Claude Code registered the MCP server (`registered ask_human
  MCP server`), **called the `ask_human` MCP tool** → run went `waiting` (question persisted:
  "PostgreSQL or SQLite?"); after `krayt answer <id> postgres` the answer flowed back and Claude
  **implemented the chosen database** (`db.py` with `psycopg`, `requirements.txt`) and finished
  `done` (exit 0) — the whole §6.13 premium path. Precise resume **directly observed**: `krayt ls`
  showed `run_f671edac` flip `waiting`→`running` immediately after `krayt answer` (guest `Resolved`
  event; `TestQuestionResolvedResumes`). `q1.json` also confirms the MCP tool passed
  `choices: [PostgreSQL, SQLite]`. Closes the Phase 6 "Done when".
- Needed: one base image rebuild carrying both Phase-6 pieces, then a hardware round-trip.
  1. `make proto` regen is already committed (`RunEvent.Resolved`); the **guest-agent** now emits
     the Resolved event, so a rebuild ships the precise `waiting`→`running` resume on-VM.
  2. `cmd/krayt-ask --mcp` (the MCP server) is built into the image; it pulls a **new** module
     (`github.com/modelcontextprotocol/go-sdk`), so the `flake.nix` **`vendorHash` MUST be
     regenerated** (set `lib.fakeHash`, build, paste the reported `got: sha256-…`).
  3. Rebuild + re-pin `internal/vmimage/pinned.go`, push/publish.
- Why the agent can't: aarch64-linux Nix build + the guest is baked into the rootfs (no Nix in
  the sandbox); the MCP round-trip needs a real MCP-speaking agent + live keys.
- Exact steps/commands: after the rebuild, rebuild/push the `hack/claude-code` image (its
  entrypoint now writes an `.mcp.json` registering `krayt-ask --mcp` when `KRAYT_ASK_SOCKET` is
  set) and run `krayt run --agent claude-code --secrets … --on-question=wait --allow
  api.anthropic.com` with a task that forces a genuine decision (e.g. "if it's ambiguous whether
  to target Postgres or SQLite, ask the human"). Answer from a second shell with `krayt answer`.
- Verify success by: Claude calls the `ask_human` MCP tool → run shows `waiting` → after `krayt
  answer` the run **flips back to `running`** (not held at waiting) and completes using the
  answer. Precise-resume + MCP handler are host-proven (`TestQuestionResolvedResumes`,
  `TestBridgeOnResolved`, `TestAskHumanHandler`).
- Watch-out: the Claude Code MCP registration is agent-specific glue — the entrypoint uses
  `--mcp-config <.mcp.json>`; if that flag/format shifted in the installed Claude Code version,
  adjust the entrypoint (try `claude mcp add ask-human -- krayt-ask --mcp`). This is the one spot
  that may need a tweak against the live CLI.
- Blocking: no — closes the Phase 6 "Done when"; nothing else depends on it.

## [Release] Install the Renovate GitHub App
- Needed: install the **Renovate** GitHub App (https://github.com/apps/renovate) on the repo (or
  the `418-cloud` org). The committed `renovate.json` configures it, but the config alone does
  nothing until the App is installed and runs — it then opens the dependency-update PRs.
- Why the agent can't: installing a GitHub App is an org/repo admin action in the GitHub UI.
- Exact steps/commands: install the Renovate App, grant it the repo; Renovate opens an onboarding
  PR, then starts creating grouped `deps:` PRs (Go modules, GitHub Actions, Nix flake inputs,
  `hack/**` Dockerfiles). Auto-merge is off, so review + merge them yourself.
- Verify success by: an onboarding/first Renovate PR appears within a few hours.
- Blocking: no — release-please and the CLI build work without it; this only enables automated
  dependency updates.
- Note (no token needed): release-please + the CLI binary build run in one workflow via the
  default `GITHUB_TOKEN`, so no PAT/App token is required for releases. The VM image releases on
  its own `vmimage-v*` tag (`image.yml`); see RELEASING.md for the boot-test → pin flow.

## [Verify] Self-contained bundle fix over a real multi-commit repo — DONE ✅
- Resolved: verified on Apple Silicon by dogfooding krayt over its **own** repo (a real merge-commit
  history — the exact failing shape). The **old** globally-installed `krayt` reproduced the bug
  (`clone bundle … Could not read b5295a9… / Failed to traverse parents / remote did not send all
  necessary objects`); the **rebuilt** `./bin/krayt` (with the fix) cloned cleanly and drove a full
  `--agent claude-code --on-question=wait` run to completion — Claude Code authenticated, registered
  the `ask_human` MCP server, edited `README.md`, and the run reached `done` (exit 0) as
  `run_9ba953aa` with a clean `changes.patch`. **Host-only fix, no image rebuild** (`CreateBundle`
  runs in the host orchestrator; the guest-agent's ingest/clone code is unchanged).
- What: dogfooding exposed a bug — `krayt run` over a repo with real history failed at guest clone
  with *"remote did not send all necessary objects"*. Root cause: the old shallow-clone-then-bundle
  produced a non-self-contained bundle (git bundle doesn't record the shallow boundary). Fixed in
  `internal/patch/patch.go`: `bundle_depth >= 1` now bundles a **parentless single-commit snapshot**
  of the current state; `bundle_depth 0` bundles full history. Reproduced + covered by
  `TestRoundTripMultiCommitMerge` / `TestCreateBundleMultiCommitIncludeDirty`.
- Still worth a one-off: confirm `--bundle-depth 0` (full-history path) on hardware too.
- Blocking: no — done; unit tests reproduce the failure and prove the fix, now confirmed on-VM.

## [Dev image] Build + push hack/krayt-dev, then run a first real dogfood task
- Needed: a real `docker buildx` multi-arch build/push of `hack/krayt-dev` to
  `ghcr.io/418-cloud/krayt-dev` (via `.github/workflows/dev-image.yml`, or manually), plus a live
  `krayt run` on Apple-Silicon hardware exercising the image against krayt's own repo.
- Why the agent can't: no `docker`/`buildx` in this sandbox, no registry credentials, and no
  Apple-Silicon Mac to run `krayt` itself (same constraints as every other real-hardware/CI item
  in this file). I have **not** run `docker build` or fabricated a build/push result — the
  Dockerfile/entrypoint/workflow are written and `bash -n`-checked, but genuinely untested.
- Exact steps/commands:
  1. Merge to `main` (or `workflow_dispatch`) so `dev-image.yml` builds + pushes `linux/amd64` +
     `linux/arm64` to GHCR; watch the Action run for the actual build/push to succeed (first
     build is the real proof — `go install`/`protoc` version pins below could be wrong).
  2. Then, on the Mac:
     ```sh
     krayt run --image ghcr.io/418-cloud/krayt-dev --agent claude-code \
       --allow api.anthropic.com,proxy.golang.org,sum.golang.org \
       --secrets ./secrets.env --task hack/krayt-dev/task.example.md --repo .
     ```
- Verify success by: the Action run shows a real green multi-arch build + push (note the actual
  image digest in this entry once done — do not invent one); `krayt ls` shows the run reaching
  `done`/`EXIT 0`; `krayt patch <id>` shows the agent's real `go build`/`test`/`lint` output
  reflected in its summary/changes.
- Blocking: no — nothing else in the roadmap depends on this image; it only enables dogfooding.
- Watch-out: the pinned tool versions in `hack/krayt-dev/Dockerfile` (`PROTOC_VERSION`,
  `PROTOC_GEN_GO_VERSION`, `PROTOC_GEN_GO_GRPC_VERSION`, `BUF_VERSION`, `ORAS_VERSION`,
  `GOLANGCI_LINT_VERSION`) were chosen from training knowledge, not verified against a live
  registry (this sandbox has no network egress at all, not even to resolve a version string) — if
  the first build fails on a `go install .../<tool>@vX.Y.Z: unknown revision` error, bump that one
  `ARG` to a real current release and retry; nothing else in the Dockerfile should need to change.
- Watch-out (**untested — validate on first build**): the single-user Nix install block
  (`USER agent` + `curl … nixos.org/nix/install | sh -s -- --no-daemon`, `/nix` pre-`chown`ed to
  agent, flakes enabled). Non-root single-user Nix in a Docker build is finicky: (a) if the upstream
  installer prompts / refuses non-interactively, switch to the Determinate installer
  (`curl -sSf -L https://install.determinate.systems/nix | sh -s -- install linux --no-confirm
  --init none …`) or add the appropriate non-interactive flag; (b) confirm `nix --version` +
  `nix build` work **as the agent user** in the built image (`docker run --rm <img> nix --version`).
  Then validate the real capability with `bin/task-test.md` (regenerates `images/flake.nix`
  `vendorHash`), whose run needs the egress
  `--allow api.anthropic.com,proxy.golang.org,sum.golang.org,cache.nixos.org,github.com,codeload.github.com`.

## [Dev image] Verify GitHub Action digest pins in dev-image.yml — DONE ✅
- Resolved: the three actions in `.github/workflows/dev-image.yml` are now SHA-pinned to the latest
  release **within the majors the workflow already used** (digests resolved from the GitHub API,
  not fabricated): `docker/setup-qemu-action@c7c5346… # v3.7.0`,
  `docker/setup-buildx-action@8d2750c… # v3.12.0`, `docker/build-push-action@10e90e3… # v6.19.2`.
  The "authored offline" comment was removed. (Newer majors exist — setup-qemu/buildx v4.x,
  build-push v7.x — left for Renovate to propose as reviewable bumps rather than folding an
  untested major jump into the pinning fix.)
- Blocking: no — was only supply-chain hygiene to match repo convention; done.

## [Security review] Rotate the working-tree `secrets.env` token — DONE ✅
- Resolved: the maintainer revoked the exposed `CLAUDE_CODE_OAUTH_TOKEN` (scope `user:inference`)
  in the Claude web UI on 2026-07-06; the token in `secrets.env` is now dead.
- Follow-up (not a security item): drop a fresh token into `secrets.env` (gitignored) to run krayt
  again. Never inline a token into the tracked `krayt.yaml` — see
  `docs/ai-tasks/fix-krayt-yaml-tracking.md`.
- Blocking: no.

## [Security review] Rebuild + re-pin the VM image for the container-hardening changes — DONE ✅
- Resolved: the guest-agent's `vendorHash` in `images/flake.nix` was regenerated to
  `sha256-jM5Xcp/sE2nuYKpi8H1P9YNhx0S33gd+JcelJO+9tzE=` (no new Go module — the hardening task
  added no dependency; the hash moved because `nixpkgs/nixos-unstable` is a floating input), the
  image rebuilt and published as **v0.2.0-rc1**, and `internal/vmimage/pinned.go` re-pinned to
  `ghcr.io/418-cloud/krayt-vmimage@sha256:8e5e90c76b0b21261bd1d350b62e04b5ce1eaf37d7d96776e2e6bcfefb61e4fe`.
  `TestBootHello` re-passed on the new image (`containerd=v2.3.1`, no regression) before the
  hardening tests below were trusted.
- Why the agent can't: needs an aarch64-linux Nix builder (no Nix in the sandbox) — the guest-agent
  (`internal/guest/runner/containerd_linux.go` et al., the whole `harden-container-oci-spec.md`
  task) is baked into the rootfs, so the previously-pinned image (v0.1.2) predated these changes
  and could not prove them.
- Blocking: yes — was blocking the tests below (an unrehardened image would prove nothing); now
  resolved.

## [Security review] Run the container-hardening integration tests on a Mac (findings #1/#3) — DONE ✅
- Resolved: both tests **PASS** on Apple Silicon against the rebuilt image (v0.2.0-rc1) with the
  `hack/hardening-probe` and `hack/root-probe` images
  (`docker.io/tjololo/test-krayt:hardening-probe` / `:root-probe`):
  `TestContainerHardening` (13.67s) — the probe's own log confirms every control: `CapEff`/`CapAmb`
  all-zero, `NoNewPrivs=1`, `Seccomp=2`, uid 1000 (non-root), proxyd found at uid 998 via
  `/proc/net/tcp`, and `setuid(998)` failed with `EPERM` — the egress-allowlist bypass (finding #1)
  is closed. `TestRootImageFailsClosed` (17.45s) also passed — the root image's run was rejected
  before it launched. No regression: `TestBootHello` re-passed on the same image. Findings #1 and
  #3 are now confirmed closed on real hardware, not just in the in-memory spec-builder unit tests.
- Needed: on the Apple-Silicon Mac + vfkit (same host as the other integration tests), run the
  new gated tests that prove the least-privilege OCI spec on a real containerd:
  `TestContainerHardening`, `TestRootImageFailsClosed` (both in
  `internal/orchestrator/integration_test.go`). These verify the caps-drop / non-root / seccomp /
  no-new-privs / setuid(proxyd)=EPERM behavior end to end — the unit tests only cover the spec
  builder in memory (`internal/guest/runner/spec_linux_test.go`, run in CI on linux).
- Why the agent can't: needs virtualization hardware, the base VM image (containerd + nftables),
  and purpose-built linux/arm64 probe images — none available in the cloud sandbox (§14, §11.6).
- Exact steps/commands:
  1. Build/publish two linux/arm64 probe images and set their refs:
     - `KRAYT_HARDENING_IMAGE` — **non-root** (e.g. `USER 1000`) image whose entrypoint exits 0
       ONLY when ALL hold: `/proc/self/status` has `CapEff: 0000000000000000`,
       `CapAmb: 0000000000000000`, `NoNewPrivs: 1`, `Seccomp: 2` (filter mode); `id -u` != 0; and
       a `setuid(<proxyd uid>)` returns `EPERM` (read proxyd's uid from `/proc/net/tcp` owners or
       brute-force the system-uid range, then attempt the syscall). Non-zero exit on any failure.
     - `KRAYT_ROOT_IMAGE` — a linux/arm64 image with `USER root` (or no USER); its entrypoint is
       irrelevant because the run must fail before it launches.
  2. Run:
     ```
     KRAYT_KERNEL=…/vmlinuz KRAYT_INITRD=…/initrd KRAYT_ROOTFS=…/rootfs.img \
     KRAYT_HARDENING_IMAGE=ghcr.io/you/krayt-hardening-probe:latest \
     KRAYT_ROOT_IMAGE=ghcr.io/you/krayt-root-probe:latest \
       go test -tags 'integration darwin' \
       -run 'TestContainerHardening|TestRootImageFailsClosed' -v ./internal/orchestrator/
     ```
- Verify success by: both tests PASS — `TestContainerHardening` exits 0 (all assertions hold),
  and `TestRootImageFailsClosed` gets a run error naming the non-root / uid-0 requirement.
- Blocking: no — the Go changes, unit tests (caps validator, spec builder, plumbing), `go build`
  for `linux/arm64`, and `go test -race ./...` all pass in the sandbox; these hardware assertions
  are the final on-metal confirmation of findings #1/#3, mirroring the existing egress-probe
  handoff. `fix-egress-allowlist-bypass.md` depends on this task and shares the setuid regression.

## [Security review] Normalize the protobuf codegen version comment (make proto) — DONE ✅
- Resolved: `make proto` re-run with the pinned Nix toolchain; `internal/protocol/pb/krayt.pb.go`
  now reads the canonical `protoc v7.34.1` header, and `git diff HEAD` on the generated files is
  empty — the hardening commit (`e71df7d`) already carries the canonical, toolchain-verified
  codegen for the new `TaskSpec` fields (`add_capabilities`, `seccomp_unconfined`,
  `readonly_rootfs`).
- Needed: re-run `make proto` with the pinned Nix toolchain and commit the result, so
  `internal/protocol/pb/krayt.pb.go`'s header reads the canonical `protoc v7.34.1` rather than the
  `v7.35.1` the sandbox's `protoc` emits.
- Why the agent can't: `nix run .#proto` needs to build the pinned codegen derivation, which needs
  network/substituter access unavailable in this sandbox; the sandbox `protoc` is v7.35.1, one
  patch ahead of the canonical v7.34.1 (Phase 0 precedent: only the version *comment* differed,
  the generated code was otherwise identical). The new `TaskSpec` fields (`add_capabilities`,
  `seccomp_unconfined`, `readonly_rootfs`) were regenerated with the sandbox `protoc` and are
  functionally correct — the code compiles, builds for `linux/arm64`, and passes `go test -race`.
  `krayt_grpc.pb.go` was restored to the canonical committed copy (it had no real change).
- Exact steps/commands: `make proto && git diff internal/protocol/pb` (expect only the `protoc`
  version comment to change back to v7.34.1); commit if so.
- Verify success by: `git diff` shows just the header comment normalized; `go build ./...` and
  `go test -race ./...` still pass.
- Blocking: no — cosmetic; the committed generated code is functionally correct.

## [Security review] Run the guest git-config-injection escape test on a Mac (finding #2) — DONE ✅
- Resolved: `TestGuestGitConfigInjectionInert` **PASSES** on Apple Silicon against the
  `hack/gitconfig-probe` image (`docker.io/tjololo/test-krayt:git-conf-probe`) (19.65s):
  `changes.patch` was produced with no `PWNED_BY_ROOT` new-file entry — the injected
  `core.fsmonitor`/`textconv` never ran as root — while the normal `greeting.txt` edit landed.
  Finding #2 is now confirmed closed on real hardware, not just in the `internal/patch` unit tests.
  (An earlier run against the same image failed the test; that was a bug in the test's own
  assertion — a bare substring check on `PWNED_BY_ROOT` matched `pwn.sh`'s own source text, which
  necessarily names that path, not an actual sentinel file. Fixed to check for the sentinel's own
  `diff --git a/PWNED_BY_ROOT b/PWNED_BY_ROOT` header instead.)
- Needed: on the Apple-Silicon Mac + vfkit (same host as the other integration tests), run the new
  gated test that proves a container cannot make the ROOT guest-agent's git execute attacker-written
  `.git` config — the container→guest-root escape of §10 finding #2:
  `TestGuestGitConfigInjectionInert` in `internal/orchestrator/integration_test.go`. The unit tests
  (`internal/patch`: `TestDiffConfigInjectionInert`, `TestDiffBaselineTamperInert`) already prove the
  patch-generation isolation on the host; this is the end-to-end on-metal confirmation through a real
  containerd + the writable-`/workspace` bind mount.
- Why the agent can't: needs virtualization hardware, the base VM image (git + containerd), and a
  purpose-built linux/arm64 probe image — none available in the cloud sandbox (§14, §11.6).
- Exact steps/commands: use the ready-made probe in `hack/gitconfig-probe/` (self-contained
  image + full runbook in its README) — build/push it, then:
  1. Build/publish the linux/arm64, **non-root** probe image and set `KRAYT_GITCONFIG_IMAGE`:
     ```
     cd hack/gitconfig-probe
     docker buildx build --platform linux/arm64 -t <your-registry>/krayt-gitconfig-probe:latest --push .
     ```
     Its entrypoint (see `hack/gitconfig-probe/entrypoint.sh`) writes an executable
     `/workspace/pwn.sh` whose body is `#!/bin/sh` + `touch /workspace/PWNED_BY_ROOT` — note
     `pwn.sh` itself is a normal new file that always lands in changes.patch (it's just a file
     sitting in the workspace); the escape signal is whether the sentinel it *creates*,
     `PWNED_BY_ROOT`, shows up as its own new-file entry — appends the injection to the writable
     git config and attributes (`printf '[core]\n\tfsmonitor = /workspace/pwn.sh\n[diff "evil"]\n\ttextconv = /workspace/pwn.sh\n' >> /workspace/.git/config`
     and `printf '* diff=evil\n' > /workspace/.gitattributes`), makes one normal tracked edit
     (`printf 'hello world\n' > /workspace/greeting.txt`), and exits 0.
  2. Run:
     ```
     KRAYT_KERNEL=…/vmlinuz KRAYT_INITRD=…/initrd KRAYT_ROOTFS=…/rootfs.img \
     KRAYT_GITCONFIG_IMAGE=ghcr.io/you/krayt-gitconfig-probe:latest \
       go test -tags 'integration darwin' \
       -run TestGuestGitConfigInjectionInert -v ./internal/orchestrator/
     ```
- Verify success by: the test PASSES — `changes.patch` is produced, does **not** contain a
  `diff --git a/PWNED_BY_ROOT b/PWNED_BY_ROOT` entry (the injected fsmonitor/textconv never ran as
  root and so never created the sentinel file — `pwn.sh`'s own source mentioning that path is
  expected and not itself a failure), and still carries the normal `greeting.txt` edit. As an extra
  manual check on the same run, `nft list ruleset` inside the guest still shows the egress lock and
  no secret was exfiltrated (root code never executed to flush it).
- Blocking: no — the Go changes, the two `internal/patch` injection/tamper unit tests, `go build`
  for `linux/arm64`, `go vet -tags 'integration darwin'`, and `go test -race ./...` all pass in the
  sandbox; this hardware assertion is the final on-metal confirmation of finding #2, mirroring the
  existing hardening/egress probe handoffs.
