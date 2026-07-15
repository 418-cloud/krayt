# Task: a one-command integration-test runner (+ the missing trivial-edit probe image)

**Read `CLAUDE.md`, the README's "Platform reality" / "Prerequisites" sections, and
`KRAYT_SPEC.md` §14 first.** This is a self-contained tooling task, not a `KRAYT_SPEC.md` phase —
the phase-gate rule doesn't apply. Give a short plan (script structure, new probe files, CI diff),
then proceed without waiting for further sign-off, per the decisions already made below. Stop and
ask only if you find something here conflicts with the spec.

## Background

The real-VM integration suite (`//go:build integration`) is currently run by hand: each of the
five test files
(`internal/provider/vfkit/integration_test.go`, `internal/provider/firecracker/integration_test.go`,
`internal/orchestrator/integration_test.go` + its two `integration_provider_{darwin,linux}_test.go`
halves) documents its own `go test` invocation and required env vars in a header comment. That's
fine for the person who wrote them, but a new contributor (or a maintainer without the history
memorized) has to go read Go source to figure out how to run "the integration tests." Today that
means asking an AI assistant for the steps every time. Goal: one script, one command, that does
what those header comments say, so the knowledge lives in something runnable instead of only in
prose.

**Two of the eight `Test*` functions currently cannot run at all**, on either backend, because
they need `KRAYT_IMAGE` — "a trivial user image that edits a file in `/workspace`" — and no such
image is committed. Every other probe image (`hack/netprobe`, `hack/hardening-probe`,
`hack/root-probe`, `hack/gitconfig-probe`, `hack/secrets-probe`) exists and is published multi-arch
by `.github/workflows/probe-images.yml`; this one is a gap. Closing it is in scope here (see
Decisions below) so the script can run the *whole* suite with zero live credentials.

## Decisions already made (do not re-litigate; these were chosen by the maintainer up front so this
task can be done autonomously)

1. **One script**, not per-OS scripts: `hack/run-integration-tests.sh`. It detects the host OS
   (`uname -s`) and branches internally. The two platforms' steps differ (Linux needs a
   compile+setcap+run dance for `CAP_NET_ADMIN`; macOS needs none), but the outer shape — pull the
   base image, resolve probe image refs, run `go test -tags integration ...` for this host's
   packages, report pass/fail — is shared, and one command is the whole point of this task.
2. **Add `hack/edit-probe/`**, a new deterministic, credential-free probe image, modeled exactly on
   `hack/gitconfig-probe/` (alpine, non-root uid 1000, entrypoint writes a fixed file into
   `/workspace`, exits 0). Wire it into `.github/workflows/probe-images.yml`'s `probe:` matrix and
   its two `paths:` filters (`hack/edit-probe/**`). The script defaults `KRAYT_IMAGE` to
   `ghcr.io/418-cloud/krayt-probe:edit-probe`. This is the only way `TestEndToEndRealVM` and
   `TestConcurrentRealVMs` can run without a live Anthropic credential.
3. **The script runs `sudo setcap cap_net_admin+ep` itself** on Linux, on the test binaries it just
   compiled (a narrowly-scoped, per-invocation grant on a throwaway `$TMPDIR` binary — not a
   system-wide change), rather than just printing the command. It must **not** attempt
   `hack/linux-net-setup.sh`'s persistent host setup (NAT rules, systemd unit, `kvm` group) itself —
   that's a separate, already-documented one-time step with different blast radius. Preflight it via
   `krayt doctor` (see below) and fail with that pointer if it's missing.
4. **Also add a Linux-only CI job** that runs this script. Because VM boots are slow (each
   integration test spins up a real micro-VM), do **not** run it on every PR like the `test` job in
   `ci.yml` — path-filter it to the packages/files that can affect it
   (`internal/provider/**`, `internal/orchestrator/**`, `internal/guest/**`, `cmd/krayt-agent/**`,
   `hack/*-probe/**`, `hack/netprobe/**`, `hack/run-integration-tests.sh`, and its own workflow
   file) plus `workflow_dispatch`, the same pattern `image.yml` already uses. Log in to `ghcr.io`
   with `GITHUB_TOKEN` (`permissions: packages: read`) before running anyway, even though
   `krayt-probe` is now public (confirmed in `HUMAN_TODO.md`) — cheap defense against the package
   ever going private again, and it's what `krayt-vmimage` pulls already rely on working without.

## The full env-var contract (catalogued from the source — verify against it, don't re-derive)

All `Test*` functions under `-tags integration` and the env vars each one additionally requires
beyond the base three:

| Test | Package | Needs (beyond `KRAYT_KERNEL`/`KRAYT_INITRD`/`KRAYT_ROOTFS`) |
|---|---|---|
| `TestBootHello` | `internal/provider/vfkit` (darwin), `internal/provider/firecracker` (linux) | — |
| `TestGuestNetwork` | `internal/provider/firecracker` (linux only — no macOS analogue) | — |
| `TestEndToEndRealVM` | `internal/orchestrator` | `KRAYT_IMAGE` |
| `TestConcurrentRealVMs` | `internal/orchestrator` | `KRAYT_IMAGE` |
| `TestEgressEnforcement` | `internal/orchestrator` | `KRAYT_NETPROBE_IMAGE`, `KRAYT_ALLOW_HOST` |
| `TestContainerHardening` | `internal/orchestrator` | `KRAYT_HARDENING_IMAGE` |
| `TestRootImageFailsClosed` | `internal/orchestrator` | `KRAYT_ROOT_IMAGE` |
| `TestGuestGitConfigInjectionInert` | `internal/orchestrator` | `KRAYT_GITCONFIG_IMAGE` |
| `TestSecretConfinementInArtifacts` | `internal/orchestrator` | `KRAYT_SECRETS_IMAGE` (generates its own throwaway secret file — no script input needed) |

`KRAYT_CMDLINE` is optional everywhere and both backends already default it sensibly when unset —
the script should **not** set it.

`KRAYT_ALLOW_HOST` must equal `hack/netprobe`'s baked-in `ENV` default, which is `example.com`
(`hack/netprobe/main.go`) — default the script to that, overridable.

`ask-probe`/`krayt-ask-probe` are **not** part of this automated suite (no `Test*` references
them; per `probe-images.yml`'s own comment they're for manual on-hardware checks of the question
channel). Out of scope here.

## Deliverables

1. `hack/edit-probe/{Dockerfile,entrypoint.sh,README.md}` — copy the shape of
   `hack/gitconfig-probe/` exactly (alpine base pinned by digest, non-root `adduser -D -u 1000`,
   `COPY --chmod=0755 entrypoint.sh`, `USER agent`, `ENTRYPOINT`). The entrypoint should just do a
   single deterministic, idempotent edit — e.g. write/append a fixed line to a file in
   `/workspace` — log what it did to stdout, and `exit 0`. No sentinel/attack logic needed (unlike
   `gitconfig-probe`); this one is a positive control, not a security probe. README follows the
   other probes' template (what it's for, the `KRAYT_IMAGE` contract, that CI publishes it, manual
   build/push fallback).
2. `.github/workflows/probe-images.yml`: add `edit-probe` to the `probe:` matrix (with a comment
   noting it backs `KRAYT_IMAGE` for `TestEndToEndRealVM`/`TestConcurrentRealVMs`) and to both
   `paths:` filter lists.
3. `hack/run-integration-tests.sh` — see spec below.
4. `.github/workflows/ci.yml` — new job per decision 4 above.
5. `README.md` — replace/augment the implicit "go read the test file headers" story with a short
   "Running the integration tests" section under "Running an agent" or its own `---` section:
   one line for macOS, one for Linux, pointing at the script. Keep the file-header comments as-is —
   they remain the authoritative per-test manual fallback the script encodes, useful for running a
   single test by hand.
6. `docs/ai-tasks/README.md` — add this task's row.
7. `HUMAN_TODO.md` — see Verify below; this task cannot be fully verified without hardware/CI.

## Script spec (`hack/run-integration-tests.sh`)

Bash, `set -euo pipefail`. Rough algorithm:

1. **Detect OS.** `uname -s` → `Darwin` or `Linux`; anything else, print an error and exit
   non-zero (integration tests only exist for these two backends).
2. **Preflight.** `go build -o bin/krayt ./cmd/krayt` (or reuse `make krayt`), then run
   `./bin/krayt doctor`. If it exits non-zero, print its own output (it already names exactly
   what's missing and how to fix it — `/dev/kvm` access, `firecracker`/`vfkit` installed,
   `hack/linux-net-setup.sh` not yet run, etc.) and stop. Don't re-implement these checks.
3. **Resolve the base image.** If `KRAYT_KERNEL`, `KRAYT_INITRD`, and `KRAYT_ROOTFS` are all
   already set in the environment, skip this step (lets a caller/CI reuse a pulled image across
   runs). Otherwise run `./bin/krayt image pull`, parse its `  kernel: `, `  initrd: `,
   `  rootfs: ` output lines (see `runImagePull` in `internal/cli/image.go` for the exact format),
   and export the three paths.
4. **Resolve probe image refs.** For each of `KRAYT_IMAGE`, `KRAYT_NETPROBE_IMAGE`,
   `KRAYT_HARDENING_IMAGE`, `KRAYT_ROOT_IMAGE`, `KRAYT_GITCONFIG_IMAGE`, `KRAYT_SECRETS_IMAGE`:
   if already set in the environment, leave it; otherwise default to
   `ghcr.io/418-cloud/krayt-probe:<probe>` (`edit-probe`, `netprobe`, `hardening-probe`,
   `root-probe`, `gitconfig-probe`, `secrets-probe` respectively). Default `KRAYT_ALLOW_HOST` to
   `example.com` if unset.
5. **Run the suite for this OS:**
   - **Darwin:**
     `go test -tags 'integration darwin' -v ./internal/provider/vfkit/... ./internal/orchestrator/...`
   - **Linux:** for each of `./internal/provider/firecracker/` and `./internal/orchestrator/`:
     compile with `go test -c -tags 'integration linux' -o "$tmp/<pkg>.test" <pkg>`, then
     `sudo setcap cap_net_admin+ep "$tmp/<pkg>.test"`, then run `"$tmp/<pkg>.test" -test.v`. Use a
     `mktemp -d` scratch dir, cleaned up on exit (`trap ... EXIT`).
6. **Propagate failure.** If any invocation fails, the script must exit non-zero (don't let a
   later success mask an earlier failure — accumulate a status flag across steps 5's
   invocations on Linux; Darwin is a single invocation so this is automatic there).
7. **Optional passthrough (nice-to-have, not required for "Done when"):** an `-run <pattern>` (or
   `--run`) flag forwarded to `go test -run` / `-test.run`, so a contributor can run just
   `TestBootHello` while iterating. Skip if it adds meaningful complexity — the full-suite path is
   what matters.

Keep it readable over clever — this is the thing a first-time contributor reads to understand what
"running the integration tests" actually does.

## Verify

What you can check yourself, no hardware needed:
```sh
bash -n hack/run-integration-tests.sh      # syntax
shellcheck hack/run-integration-tests.sh   # if available
bash -n hack/edit-probe/entrypoint.sh
go build ./...
go vet ./...
```
If your sandbox happens to have `/dev/kvm` access and network egress to `ghcr.io`, running the
script for real on Linux is the strongest verification — do it if you can, and report the actual
result (pass/fail, what you saw), never a guess. `krayt-probe` is public, so this no longer needs
registry credentials — only `/dev/kvm` and network egress gate it.

What you **cannot** verify yourself, and must log to `HUMAN_TODO.md` instead of assuming:
- **The macOS path** — no Mac in this environment. Log it as a handoff: run
  `hack/run-integration-tests.sh` on an Apple-Silicon Mac with `vfkit` installed, confirm all
  `TestBootHello`/`TestEndToEndRealVM`/etc. pass, report the actual output.
- **The new `edit-probe` image** — needs `docker buildx build` + a real run, same as every other
  probe; log the build/push/first-run as a handoff if you can't do it in-sandbox.
- **The new CI job's first real run** — whether the standard GitHub-hosted Ubuntu runner actually
  exposes usable `/dev/kvm` (it's known to work for some KVM-dependent workflows, e.g. Android
  emulator actions, but confirm rather than assume). `krayt-probe` being public means image
  access shouldn't be the blocker, but confirm the pull actually succeeds in CI rather than
  assuming it from the package's visibility alone. Log the first CI run's outcome; if `/dev/kvm`
  genuinely isn't available on the hosted runner, say so plainly rather than leaving a job that
  silently no-ops or reports green without having run anything.

Never fabricate any of the above (a "should work" is not a "ran and passed").

## Done when

- `hack/run-integration-tests.sh` exists, is executable, passes `bash -n`, and correctly
  implements the algorithm above (verified by code reading + whatever real execution your sandbox
  allows).
- `hack/edit-probe/` exists, matches the other probes' structure, and is wired into
  `probe-images.yml`.
- `ci.yml` has the new path-filtered/`workflow_dispatch`-gated Linux integration job, logging into
  GHCR with `GITHUB_TOKEN` first.
- `README.md` documents the one-command path; `docs/ai-tasks/README.md` lists this task.
- `HUMAN_TODO.md` has honest, specific entries for everything above that needs hardware/CI to
  actually confirm — no fabricated "boot succeeded" or invented CI run results.

## Constraints

- Don't rename or repurpose any existing `KRAYT_*` env var, and don't touch the existing
  `//go:build integration` test files at all — the script encodes their documented manual steps,
  it doesn't change their contract. If something here turns out to disagree with what those files
  actually require, the files are the source of truth; flag the conflict rather than guessing.
- Don't touch `hack/linux-net-setup.sh` or fold its one-time, persistent host setup into the
  per-run script (decision 3 above).
