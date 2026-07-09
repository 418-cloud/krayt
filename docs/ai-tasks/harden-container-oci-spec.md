# Task: harden the container OCI spec (drop caps, enforce non-root, seccomp, opt-in rootfs)

**Read `CLAUDE.md` and `KRAYT_SPEC.md` (§6.6 egress, §6.10 containerd runner, §8.2 container
contract, §10 security model) first. Proceed autonomously — this is a self-contained task; there is
no interactive human to approve a plan. Where a step needs a real Apple-Silicon Mac (a live
containerd run), do everything you can, add a `HUMAN_TODO.md` entry per §14, and continue.**

This task closes **two findings** from the security review:

- **Finding #1 (Critical) — egress allowlist bypass.** The nftables egress lock
  (`internal/guest/proxy/firewall_linux.go:17`) permits egress for `meta skuid "proxyd"`, and the
  container shares the VM network namespace (`internal/guest/runner/containerd_linux.go:95`). The
  container OCI spec drops **no** capabilities, so it keeps containerd's default set — which
  **includes `CAP_SETUID`/`CAP_SETGID`** (verified in the pinned `containerd v2.3.2`,
  `pkg/oci/spec.go:118-134`). A container process can therefore `setuid()` to proxyd's uid (learned
  from `/proc/net/tcp` in the shared netns, or brute-forced over the small system-uid range) and its
  egress is accepted — **the default-deny allowlist is fully bypassed.** It can also run as root
  if the image says so, because krayt does not enforce non-root.
- **Finding #3 (High) — permissive OCI spec.** A repo-wide grep confirms the spec applies **no**
  capability drop, **no** seccomp profile, **no** enforced non-root user, and **no** read-only
  rootfs. This maximizes the blast radius of any container-runtime/kernel bug inside the VM.

## Goal

Rebuild the OCI spec in `internal/guest/runner/containerd_linux.go` so the untrusted agent container
runs with least privilege:

1. **Drop all capabilities by default**, with a per-task opt-in to re-grant specific ones.
2. **Fail the run if the container would run as root** (uid 0 / unset `USER`), with a clear error.
3. **Apply containerd's default seccomp profile** by default, with a per-task opt-out.
4. **Read-only rootfs is a per-task opt-in (default OFF)** — see the caveat below.

Keep `NoNewPrivileges=true` (already the containerd default). Do **not** change the nftables rule or
the proxy — dropping `CAP_SETUID`/`CAP_SETGID` plus enforcing non-root is what closes the egress
bypass while leaving the existing `skuid "proxyd"` L3 lock in place.

### Decisions already made (do not re-litigate)

- Capability policy: **drop all** by default; add a per-task `container.capabilities: [..]` opt-in.
- Root images: **fail the run** (do not silently force a uid).
- Seccomp: **containerd default profile** by default; per-task `container.seccomp: unconfined` opt-out.
- Read-only rootfs: **per-task `container.readonly_rootfs: true`, default OFF.** Two reasons:
  1. **Compatibility.** The reference agent images (`hack/krayt-dev/Dockerfile`,
     `hack/claude-code/Dockerfile`) run as `USER agent` (uid 1000) and **write into `/home/agent`**
     (nix profile, `~/.claude`, Go caches). A read-only rootfs breaks those images, and a writable
     tmpfs over `$HOME` would hide the image's pre-installed tooling.
  2. **Marginal benefit in krayt's model, by design.** krayt's isolation is the **ephemeral
     VM + single-use container**: one run per VM, CoW disk destroyed on teardown, no host fs shared,
     and the trusted guest-agent runs *outside* the container and never executes anything from the
     container rootfs. Read-only rootfs mainly buys *persistence/tamper* resistance — which has almost
     no blast radius here because nothing outlives the run and nothing privileged reads the container
     fs. The load-bearing container controls are the default-on ones (dropped `CAP_SETUID`/`SETGID`,
     enforced non-root, seccomp); read-only rootfs is a nice-to-have for images that don't write to
     their own fs.
  So make it opt-in, paired with writable ephemeral mounts for `/tmp` and `/run` only — never a
  blanket tmpfs over a populated dir. Document **both** reasons in §8.2/§10 so it reads as a
  deliberate architectural stance, not a compatibility workaround.

## Current behavior (grounding)

`internal/guest/runner/containerd_linux.go:84-97` builds the container with only:

```go
container, err := r.client.NewContainer(ctx, containerID,
    containerd.WithImage(image),
    containerd.WithNewSnapshot(snapshotID, image),
    containerd.WithNewSpec(
        oci.WithImageConfig(image),
        oci.WithProcessCwd(guest.ContainerWorkspace),
        oci.WithEnv(envSlice(cfg.Env)),
        oci.WithMounts(contractMounts(cfg)),
        oci.WithHostNamespace(specs.NetworkNamespace),
    ),
)
```

`contractMounts` (`:184-215`) binds `/workspace` rw, `/task` ro, `/output` rw, `/run/secrets` ro,
and the ask socket/bin. The spec-opt inputs (`cfg.Env` etc.) come from `guest.RunConfig`
(`internal/guest/runner.go:25-36`), which the Service fills in `Start`
(`internal/guest/service.go:318-329`). Non-secret task fields reach the guest via `PushTask`
(`internal/protocol/krayt.proto:36`, `TaskSpec{prompt, env}`) and are stored on the Service
(`internal/guest/service.go:157-173`).

The pinned `containerd v2.3.2` provides everything you need — **no new go.mod dependency**:
- `oci.WithCapabilities([]string{})`, `oci.WithRootFSReadonly()` (`pkg/oci/spec_opts.go:1067,500`).
- `github.com/containerd/containerd/v2/contrib/seccomp` → `seccomp.WithDefaultProfile() oci.SpecOpts`.

## Implement

### 1. Carry the per-task container policy host→guest (proto + config + plumbing)

The opt-in/opt-out values must travel to the guest, which builds the spec. Extend `TaskSpec`
(the message already carrying non-secret task config):

- `internal/protocol/krayt.proto:36` — add fields:
  ```proto
  message TaskSpec {
    bytes prompt = 1;
    map<string, string> env = 2;
    repeated string add_capabilities = 3; // opt-in caps, CAP_-prefixed, empty = drop all
    bool seccomp_unconfined = 4;          // opt-out of the default seccomp profile
    bool readonly_rootfs = 5;             // opt-in read-only rootfs (default false)
  }
  ```
- Regenerate with `make proto` and commit `internal/protocol/pb/*`. **If the pinned Nix codegen
  toolchain is unavailable in this sandbox, do not hand-edit the generated files** — write the
  `.proto` change, add a `HUMAN_TODO.md` entry (per §14) asking the maintainer to run `make proto`,
  and continue with the Go changes that don't require the regenerated types to compile-check (or
  stub locally and clearly mark it). Note the Phase-0 precedent: `make proto` is a maintainer step.
- Host config (`internal/task/config.go:15`): add a `Container` block:
  ```go
  Container struct {
      Capabilities   []string `yaml:"capabilities"`
      Seccomp        string   `yaml:"seccomp"`         // "" (default profile) | "unconfined"
      ReadonlyRootfs *bool    `yaml:"readonly_rootfs"`
  } `yaml:"container"`
  ```
  `KnownFields(true)` is already set, so unknown/typo keys still fail — good.
- Host spec (`internal/task/spec.go:14`): add a resolved `Container ContainerPolicy` field and a
  `ContainerPolicy` type with `AddCapabilities []string`, `SeccompUnconfined bool`,
  `ReadonlyRootfs bool`. Add a `ParseSeccompMode`/validation helper mirroring `ParseNetworkMode`
  so an invalid `seccomp:` value fails fast at config load, and a capability validator (below).
- CLI resolution (`internal/cli/run.go`, where the config overlays onto the spec — follow the
  existing network/questions pattern): map `Config.Container` → `RunSpec.Container`. No new CLI
  flags are required for v1 (config-file only is fine); if you add them, mirror the existing flag
  style. State the choice in the task's PR description.
- Orchestrator push (`internal/orchestrator/orchestrator.go:180`): include the new fields in the
  `PushTask` call. Store them on the Service (`internal/guest/service.go` `PushTask`, alongside
  `s.taskEnv`) and thread them into `guest.RunConfig` in `Start` (`service.go:318`). Add the
  matching fields to `guest.RunConfig` (`internal/guest/runner.go:25`).

### 2. Validate + normalize opt-in capabilities (host-side, fail fast)

Add a validator (host-side, e.g. in `internal/task`) that:
- Uppercases each entry and adds a `CAP_` prefix if missing (`"net_bind_service"` →
  `"CAP_NET_BIND_SERVICE"`).
- Rejects anything not in a known Linux capability allow-set (define the constant list; reject
  unknown names with a clear error so a typo can't silently grant nothing or everything).
- **Always reject `CAP_SETUID`, `CAP_SETGID`, `CAP_SETPCAP`, `CAP_SYS_ADMIN`, `CAP_NET_ADMIN`,
  `CAP_NET_RAW`, `CAP_DAC_READ_SEARCH`, `CAP_BPF`, `CAP_SYS_PTRACE`** even via opt-in — these either
  re-enable the egress bypass (setuid class, net_admin) or are broad escape primitives. Document the
  denylist. A task that needs one of these should use `network.mode: full` (a deliberate, separate
  opt-in), not a capability grant.

### 3. Rebuild the OCI spec in the guest runner (`containerd_linux.go`)

Replace the `WithNewSpec(...)` opt list with (order matters — image config first, security opts
after, host netns last):

```go
specOpts := []oci.SpecOpt{
    oci.WithImageConfig(image),
    oci.WithProcessCwd(guest.ContainerWorkspace),
    oci.WithEnv(envSlice(cfg.Env)),
    oci.WithMounts(contractMounts(cfg)),
    withEnforceNonRoot(),                 // (a) below
    oci.WithCapabilities(cfg.AddCapabilities), // (b) empty slice ⇒ drop ALL caps
    withClearAmbient(),                   // (c) belt-and-suspenders: Ambient = nil
    oci.WithHostNamespace(specs.NetworkNamespace),
}
if !cfg.SeccompUnconfined {
    specOpts = append(specOpts, seccomp.WithDefaultProfile())
}
if cfg.ReadonlyRootfs {
    specOpts = append(specOpts,
        oci.WithRootFSReadonly(),
        withWritableTmpfs("/tmp"),        // ephemeral, does not shadow a populated dir
        withWritableTmpfs("/run"),        // mount BEFORE the /run/secrets + ask-socket binds
    )
}
```

- **(a) `withEnforceNonRoot()`** — a custom `oci.SpecOpt` appended *after* `WithImageConfig` (which
  resolves `s.Process.User` from the image config). Return a clear error when `s.Process.User.UID == 0`:
  `"krayt: image runs as root (uid 0); set a non-root USER in the image (§8.2) — egress and secret
  confinement require non-root"`. This fails `NewContainer`, which the runner already surfaces as an
  infrastructure error (`containerd_linux.go:98-100`).
- **(b) `oci.WithCapabilities(cfg.AddCapabilities)`** — with an empty slice this sets
  Bounding/Effective/Permitted/Inheritable to empty (drop all). Opt-in caps (already validated
  host-side) are passed through.
- **(c) `withClearAmbient()`** — set `s.Process.Capabilities.Ambient = nil` explicitly so nothing
  is inheritable-ambient regardless of the image config.
- **Read-only rootfs mounts:** if you enable it, ensure the tmpfs `/run` mount is ordered *before*
  the `/run/secrets` and `/run/krayt/ask.sock` binds in `contractMounts`, or the binds get shadowed.
  Mount options for the tmpfs: `nosuid,nodev,noexec` where the workload tolerates `noexec` (drop
  `noexec` from `/tmp` if the agent execs from it — verify against the dev image).

Keep `contractMounts` mostly as-is; only reorder if read-only rootfs requires the `/run` tmpfs first.

## Tests

Unit-testable **without a VM** (the Service + spec-building logic; the containerd `Runner` itself is
`//go:build linux` and only runs on real hardware — see the integration note):

- Capability validator: `"net_bind_service"` → `"CAP_NET_BIND_SERVICE"`; unknown name errors; each
  denylisted cap (`SETUID`, `NET_ADMIN`, …) is rejected even when requested.
- Config parsing: `container:` block round-trips; invalid `seccomp:` value errors; `readonly_rootfs`
  pointer distinguishes unset vs false.
- Plumbing: a `RunSpec.Container` with opt-in caps + `seccomp: unconfined` produces the expected
  `TaskSpec` proto fields and the expected `guest.RunConfig` fields (assert through the orchestrator
  push path with a fake client, mirroring existing push tests).
- **Spec builder unit test:** factor the `[]oci.SpecOpt` assembly into a pure helper
  (e.g. `func buildSpecOpts(cfg guest.RunConfig, image oci.Image) []oci.SpecOpt` or, more testably, a
  function that applies the opts to a `*specs.Spec` in memory) so a test can apply them to a spec
  seeded like containerd's default and assert: `Capabilities.{Bounding,Effective,Permitted,
  Inheritable,Ambient}` empty by default; opt-in cap present; `Process.User.UID==0` ⇒ error;
  `Linux.Seccomp != nil` by default and `nil` when unconfined; `Root.Readonly` matches the flag.
  containerd spec opts take `(ctx, client, container, *Spec)` — pass `nil` client/container and a
  seeded spec, as containerd's own `spec_test.go` does.

**Real-hardware (integration) test** — add to the existing `//go:build integration` vfkit path and
route through `HUMAN_TODO.md`: run a container that prints `/proc/self/status` and asserts
`CapEff: 0000000000000000`, `CapAmb: 0000000000000000`, `NoNewPrivs: 1`, `Seccomp: 2` (filter mode),
and that `id -u` != 0. Add a negative test: an image with `USER root` fails the run with the
non-root error. Add the **egress regression** (also in `fix-egress-allowlist-bypass.md`): a helper
that tries `setuid(proxyd_uid)` fails with `EPERM`.

## Docs (required — task is not done without these)

- `KRAYT_SPEC.md` §6.10: document the hardened spec (drop-all caps + opt-in, enforced non-root +
  fail-closed error, default seccomp + opt-out, opt-in read-only rootfs) as part of the runner
  contract.
- `KRAYT_SPEC.md` §8.1: document the new `container:` config block (`capabilities`, `seccomp`,
  `readonly_rootfs`) with the capability denylist and the read-only-rootfs caveat.
- `KRAYT_SPEC.md` §8.2: state that the container **must** run non-root (now enforced, not just
  convention) and that images writing to `$HOME` are incompatible with `readonly_rootfs: true`.
- `KRAYT_SPEC.md` §10: update the security table — capabilities are dropped, seccomp is applied, the
  container runs non-root; note that this is what makes the `skuid`-based egress lock unbypassable.
- `configs/krayt.yaml`: add a commented `container:` example.
- `docs/ai-tasks/README.md`: add this task to the table.

## Verify (offline in the sandbox)

```sh
make proto            # if the toolchain is available; else HUMAN_TODO handoff
go build ./...
GOOS=linux GOARCH=arm64 go build ./...   # guest cross-compile
go test -race ./...
golangci-lint run
```

The container runtime itself needs a real Mac — the `go build` for `linux/arm64` proves the guest
compiles; the runtime assertions above are the integration test.

## Done when

- Caps are dropped to empty by default (opt-in validated + denylisted); a root image fails the run;
  seccomp default profile applied with a working opt-out; read-only rootfs works as an opt-in without
  breaking the `krayt-dev` image (verify both states).
- Unit tests above pass; the integration assertions are written and logged in `HUMAN_TODO.md` for the
  maintainer to run on hardware.
- KRAYT_SPEC §§6.10/8.1/8.2/10 updated.

## Constraints

- **No new go.mod dependency** — everything is in the pinned `containerd v2.3.2`.
- Do **not** change the nftables ruleset or the proxy in this task (see
  `fix-egress-allowlist-bypass.md`, which depends on this one).
- Keep the guest OS-agnostic core build-tag-clean; the new spec code stays in the
  `//go:build linux` `internal/guest/runner` package. Host-side config/validation stays cross-OS.
- Small, reviewable diffs; do not hand-edit generated protobuf.
