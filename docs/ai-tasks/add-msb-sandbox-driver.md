# Task: add `internal/sandbox` — the `msb` CLI driver and its `krayt doctor` checks

**Read `CLAUDE.md`, `docs/adr-microsandbox-sandbox-layer.md` ("Integration path: CLI or SDK" and
"Handing secrets over"), and `KRAYT_SPEC.md` §6.3, §12 first.** Give a short plan (the package
layout, the child-env allowlist, the fake-`msb` test seam) and proceed — this task is **purely
additive**. Nothing calls the new package yet and no existing behaviour changes, so it can land and
be reviewed on its own.

This is the foundation of the microsandbox migration arc (see `docs/ai-tasks/README.md`). Two of
its probes are still outstanding in `probe-microsandbox-feasibility.md`, but **neither blocks this
task** — nothing here depends on their answers.

## Background

Under ADR option B1, krayt drives the `msb` binary as a subprocess and speaks to it over argv,
stdio and its JSON output. That is the house idiom already: the vfkit and Firecracker providers
both drive a subprocess and talk to it over a socket (`KRAYT_SPEC.md` §6.3, §12). The Go SDK was
rejected because it is a cgo `dlopen` bridge that would cost `CGO_ENABLED=0` and the
single-Linux-runner cross-build in `.github/workflows/release-please.yml:52-70` — while still
requiring the `msb` binary on the host, which it downloads on first use. cgo buys a typed API and
buys nothing else.

The ADR was verified against microsandbox **0.6.16**. Everything asserted below is from that source
tree, not its documentation, and the two places where they disagree are called out.

## Decisions already made (do not re-litigate)

1. **New package `internal/sandbox`**, OS-agnostic, no build tags. It is the *only* place in krayt
   that knows msb exists — the same containment rule `internal/provider` had for the hypervisor.
   Nothing above it may construct an `msb` argv.
2. **CLI, not SDK.** Do not add `github.com/superradcompany/microsandbox/sdk/go` to `go.mod`. Do not
   introduce cgo. The build must stay `CGO_ENABLED=0`-clean on every target.
3. **`KRAYT_MSB_BIN` is the swap/test seam**, mirroring `EgressProxyBinEnv`
   (`internal/orchestrator/egressproxy.go:23-30`): when set, it replaces the resolved `msb` path.
   That is how the tests point at a fake without mocking the driver, and it is the documented
   escape hatch for a non-`PATH` install.
4. **The child environment is a closed allowlist, never `os.Environ()`** — the exact discipline of
   `egressProxyChildEnvKeys` (`internal/orchestrator/egressproxy.go:32-57`). Copy its shape,
   including the rule that every added key carries a comment saying why msb genuinely needs it. The
   starting set:
   - `PATH`, `HOME` — process hygiene; msb resolves its own runtime under `$HOME/.microsandbox`.
   - `MSB_HOME` — forwarded only when the operator set it; msb's documented state-dir override.
   - `SSL_CERT_FILE`, `SSL_CERT_DIR` — forwarded only when set, so msb can verify upstream
     registry/API certificates on distributions where Go's and Rust's root pools are only
     discoverable through them.
   - **`MSB_BACKEND=local`, always set, never forwarded.** See the next point.
   Secret values are added on top of this by `hand-secrets-to-msb.md`; do not add them here.
5. **krayt pins `MSB_BACKEND=local` in the child environment on every invocation.** This is a
   security requirement, not tidiness, and it is not in the ADR. msb resolves its backend as
   *programmatic → `MSB_BACKEND` → `MSB_PROFILE` → `active_profile` in `~/.microsandbox/config.json`
   → local* (`docs/getting-started/backends.mdx`). So an operator who has ever run
   `export MSB_BACKEND=cloud`, or who has a cloud `active_profile` saved from an unrelated session,
   would have `krayt run` silently execute the task — and hand the credentials — to
   microsandbox's hosted service. krayt's threat model and §10 assume the sandbox is local.
   Setting `MSB_BACKEND=local` explicitly defeats both the environment and the profile, because
   `MSB_BACKEND` outranks `MSB_PROFILE` and `active_profile`.
   **Also assert it**: a pre-flight `msb context --format json` must report the local backend, and
   krayt refuses to start a run if it does not. A pin that is never checked is a comment.
6. **Every subprocess gets the run's `context.Context`** and is killed with it, matching how
   `internal/orchestrator` treats every other step.
7. **Version floor.** Record a `MinVersion` constant (`0.6.16`, the version the ADR was verified
   against) parsed out of `msb --version`. Below it, `krayt doctor` reports a hard failure and
   `krayt run` refuses. msb is beta and has shipped a breaking wire change in a patch release; a
   silent version drift is how that becomes krayt's outage.
8. **No lifecycle policy lives here.** This package builds argv, runs a process, and parses output.
   Which flags a run *deserves* is `translate-network-policy-to-msb.md`'s and
   `hand-secrets-to-msb.md`'s job; the order of operations is `run-tasks-on-microsandbox.md`'s. Keep
   this package free of `krayt.yaml` vocabulary — it takes structs, not config.

## What to build

### `internal/sandbox/msb.go` — the driver

```go
// Client drives the msb CLI for one host. It is stateless and safe for concurrent use;
// every method spawns its own process.
type Client struct {
    Bin string // resolved msb path; KRAYT_MSB_BIN wins, else exec.LookPath("msb")
}
```

Methods, each a thin wrapper that builds argv and runs it:

| Method | msb form | Notes |
|---|---|---|
| `Version(ctx)` | `msb --version` | parsed into a comparable struct; used by `doctor` and the run pre-flight |
| `Context(ctx)` | `msb context --format json` | the `MSB_BACKEND=local` assertion of decision 5 |
| `Create(ctx, CreateSpec)` | `msb create …` | argv assembled from a struct, never from strings the caller pre-joined |
| `Exec(ctx, ExecSpec)` | `msb exec --stream …` | see "Streaming" below |
| `Copy(ctx, from, to)` | `msb copy` | `docker cp` syntax: `./local sandbox:/path` and back |
| `Logs(ctx, name, …)` | `msb logs --json` | JSON Lines, `s` field tags the stream |
| `Stop(ctx, name)` / `Remove(ctx, name)` | `msb stop` / `msb rm --force` | teardown, always called with `context.WithoutCancel` |
| `Pull(ctx, ref)` | `msb pull` | image acquisition, for `retire-vm-image-pipeline.md` |

`CreateSpec` carries typed fields — image ref, name, user, cpus, memory, root-disk, `--max-duration`,
env pairs, `--vsock` routes, net rules, secrets, TLS flags, `--security` — and one
`ExtraArgs []string` field reserved for `add-msb-extra-conf-escape-hatch.md`. **Render argv in a
pure function** (`func (s CreateSpec) Args() []string`), so the whole surface is unit-testable
without spawning anything. That function is where most of this task's tests live.

### Streaming — use `--stream`, not the default

**`msb exec`'s default non-interactive mode buffers the entire output and writes it only after the
command exits** (`crates/cli/lib/commands/exec.rs:213-223`: `exec_with` → `ExecOutput` →
`write_all`). krayt streams agent logs to the terminal live (`internal/orchestrator`'s `LogOut`),
so the default mode is unusable.

`msb exec --stream` is the right mode (`exec.rs:266-311`): it pumps `ExecEvent::Stdout` /
`ExecEvent::Stderr` to the host's stdout/stderr as they arrive, keeps the two streams separated end
to end, forwards host stdin incrementally, and returns the guest's exit code. Its one constraint is
that **stdin must be piped, not a terminal** (`exec.rs:80-84` bails otherwise) — which is automatic
when krayt spawns it with an `exec.Cmd` stdin pipe, but must be done deliberately rather than left
to inherit.

**Exit-code ambiguity, and how to resolve it.** `msb exec` propagates the guest's exit code by
calling `std::process::exit(exit_code)` (`exec.rs:125-127`), while msb's *own* failures surface as
an `anyhow` error and exit `1`. So exit `1` alone cannot distinguish "the agent returned 1" from
"msb could not start the command". Resolve it structurally rather than guessing: have the helper
and agent execs write their real exit status where krayt can read it unambiguously, and treat a
non-zero `msb` exit with **no** terminal event observed as a driver failure (`ErrMsbFailed`) rather
than an agent exit code. Model this as a distinct error type the orchestrator can branch on; do not
paper over it by mapping everything to the agent's exit code.

### `internal/sandbox/doctor.go` — checks for `krayt doctor`

Extend `commonChecks()` (`internal/cli/doctor.go:20`) with, in order:

1. `msb` found on `PATH` (or via `KRAYT_MSB_BIN`), with the resolved path shown.
2. `msb --version` ≥ `MinVersion`.
3. `msb context --format json` resolves to the **local** backend under krayt's own pinned child env.
   Report the resolved backend either way — an operator with a cloud profile should see krayt
   overriding it, not silently benefit from it.
4. `msb doctor` passthrough: msb ships its own host-readiness command (hypervisor availability, KVM
   interrupt acceleration on Linux, a clone probe inside `MSB_HOME`). Run it and surface its exit
   status as one krayt check rather than reimplementing any of it — msb knows more about its own
   prerequisites than krayt can.

Keep the existing vfkit/firecracker checks for now; `run-tasks-on-microsandbox.md` removes them
along with the providers.

### Testing — a fake `msb`, no real one

The repo's established pattern for a spawned-child test is re-execing the test binary as the helper
process (`internal/orchestrator/climit_test.go`'s `TestMain`, and
`TestSpawnEgressProxyRealChildProcess` in `egressproxy_internal_test.go`). Do the same here: a
`TestMain` that, when `KRAYT_FAKE_MSB` is set in its own environment, behaves as a scriptable `msb`
— echoing its argv to a file, emitting canned JSON or JSON-Lines, exiting with a chosen code — and
tests that point `KRAYT_MSB_BIN` at `os.Args[0]`.

Cover at minimum:

- `CreateSpec.Args()` renders every field, in a stable order, with values quoted/escaped where msb's
  grammar needs it. `--net-rule`'s tokens use `@`, `:` and `,`; assert they are passed as single
  argv elements and never shell-joined.
- The child env is **exactly** the allowlist: assert the fake `msb` observes `MSB_BACKEND=local`,
  observes no key outside the list, and that an exported `MSB_BACKEND=cloud`, `MSB_PROFILE=prod`,
  `ANTHROPIC_API_KEY` or `AWS_SECRET_ACCESS_KEY` in the parent does **not** reach it. This is the
  test that makes decision 4 and 5 real.
- `Exec` passes `--stream`, gives the child a piped stdin, and separates stdout from stderr.
- A non-zero `msb` exit with no terminal event maps to `ErrMsbFailed`, not to an agent exit code.
- `Version` rejects a below-floor version and parses the real `msb --version` output shape.
- Teardown (`Stop`/`Remove`) still runs when the caller's context is already cancelled.

## Done when

- `go build ./...` on darwin **and** linux, `go test -race ./...`, and `golangci-lint run` are all
  green.
- `internal/sandbox` has no build tags, no cgo, and no import of `internal/{provider,guest,protocol,
  proxy,vmimage,controlclient}`.
- The new `doctor` checks appear in `krayt doctor` output on a host with no `msb` installed, as
  clean failures with actionable text (the install one-liner), not panics.
- The child-env test above passes with an `MSB_BACKEND=cloud` deliberately exported in the test
  process.
- `KRAYT_SPEC.md` gains a §6.15 (or the next free number) describing `internal/sandbox`, the child
  env allowlist, the `MSB_BACKEND=local` pin and why, and the version floor. Amend rather than
  contradict: this section is additive, so nothing existing needs rewriting yet.

## Out of scope

- Running anything through msb. No orchestrator changes, no `krayt run` wiring.
- `krayt.yaml` translation, secrets, the guest helper, the ask channel — each has its own task.
- Deleting any existing package.
