# Task: add `krayt-helper` — the stateless, root-run guest binary that builds the patch

**Read `CLAUDE.md`, `docs/adr-microsandbox-sandbox-layer.md` ("The guest helper"), and
`KRAYT_SPEC.md` §6.7, §8.2, §10 first**, plus `docs/ai-tasks/fix-guest-git-config-rce.md` — the
Critical fix whose property this helper exists to preserve. Give a short plan (the two subcommands,
the embed mechanism, the Makefile/CI wiring) and proceed. Depends on `add-msb-sandbox-driver.md`.

**Unblocked 2026-08-30 — `probe-microsandbox-feasibility.md` P2 passed on msb 0.6.16.** `msb exec
--user root` works under `--security restricted`: it lands as uid 0, and — the property that
actually matters — a root-created 0700 directory stays unreadable to an `--user agent` exec
(`hack/msb-probes/p2-exec-root-restricted.sh`). **Take both.** `--security restricted` and the
helper's privilege separation are not a trade-off, so there is no profile choice to defer and no
`HUMAN_TODO.md` entry to leave: emit `--security restricted`, and run the helper as root against a
git dir the agent cannot write (`fix-guest-git-config-rce.md`'s property).

## Sequencing — additive only

The guest agent (`internal/guest`, `cmd/krayt-agent`) is still live and still does this work over
gRPC. This task adds a second, independent implementation that nothing calls yet;
`run-tasks-on-microsandbox.md` switches to it and deletes the first. Delete nothing here.

## Background

B1 deletes the guest agent, which leaves nobody trusted inside the sandbox to build the patch. The
replacement is a **small, stateless helper binary**, copied in per run and invoked with `msb exec`.

The load-bearing fact that makes it work is `msb exec`'s per-exec user override
(`crates/cli/lib/commands/exec.rs:31-33`, `-u/--user`). It restores exactly the privilege separation
`fix-guest-git-config-rce.md` bought: create the sandbox with `--user agent`, run the agent's exec as
`agent`, run the helper as `root`. The agent then cannot write into a root-owned git dir — which is
the whole content of that Critical fix. **Without per-exec `--user` the helper would run as the same
user the agent had, and the isolation would be theatre.**

Most of the work already exists and survives B1. `internal/patch` is OS-agnostic and shells out to
`git`, and it already exports the four functions the helper needs:
`Ingest` (verify + clone the bundle, set the bot identity, tag `krayt-baseline`), `SetupPatchGit`
(snapshot a pristine **root-only** git dir before the tree is relaxed), `Diff`, and `BundleCommits`.
The helper is therefore a thin argv/JSON wrapper over code krayt keeps — which is what keeps it at
roughly 300–600 LOC.

## Decisions already made (do not re-litigate)

1. **Scope boundary — a constraint, not a preference.** Stateless, exec'd, argv in and JSON on
   stdout, exits. **No gRPC, no control protocol, no long-running process, no supervising the
   workload, no listener of any kind.** If it ever grows one, krayt has re-created the guest agent
   inside someone else's sandbox while keeping none of B1's benefit, and the ADR's LOC ledger has to
   be re-examined at that point. Put this paragraph in the package doc comment.
2. **`ask_human` must not go through it.** Routing the question channel through the helper needs a
   listener, which is precisely the boundary above. `krayt-ask` dials `AF_VSOCK` directly — see
   `dial-ask-channel-over-vsock.md`.
3. **Two subcommands, nothing else.**
   - `krayt-helper setup --bundle <path> --workspace <path> --patch-git <path> --agent-user <name>`
     → `Ingest`, then `SetupPatchGit` **before** relaxing the tree, then relax the workspace so the
     non-root agent user can edit it. The ordering is not stylistic: the pristine root-only copy
     must be taken before the tree becomes container-writable, or the isolation is void
     (`internal/guest/service.go:271-284` states the same rule; move `makeContainerWritable`
     — `service.go:468` — into `internal/patch` so both callers share one definition rather than
     forking it).
   - `krayt-helper finish --workspace <path> --patch-git <path> --baseline <ref> --out <dir>`
     → `Diff` into `<out>/changes.patch`, `BundleCommits` into `<out>/commits.bundle` when the agent
     committed, and a JSON object on stdout carrying the baseline ref, whether a commits bundle was
     written, and the diff's byte length.
   Both print JSON on stdout and human-readable errors on stderr; both exit non-zero on failure.
4. **No secret handling at all.** The matched-secret-key-names scan is **host-side** under B1 (see
   `hand-secrets-to-msb.md` decision 6) because values never enter the guest. The helper must not
   take a secrets argument, read one, or scan for one. This is a deliberate narrowing of the ADR's
   description of the helper's job — the ADR assigned it artifact assembly "including the
   matched-secret-key-names list", written before it was noticed that the host is now strictly the
   better place. Note the change in your report.
5. **Distribution: `go:embed` a per-arch static binary, `msb copy` it in per run.** No registry, no
   OCI artifact, no Nix, no boot test, and — because it is neither a kernel nor a rootfs — no
   recurrence of §11.1's backend-tagged-image problem. It is versioned with krayt by construction,
   so there is no skew to manage. Under msb the guest's architecture always equals the host's
   (libkrun runs a same-arch VM), so selection is `runtime.GOARCH`, with no second dimension.
6. **The embedded binaries are built by `make guest-bins` and are NOT committed.** Concretely:
   - `internal/sandbox/guestbin/bin/` holds `krayt-helper-linux-amd64` and
     `krayt-helper-linux-arm64`, gitignored, with a committed `.gitkeep` so the directory exists.
   - The embed is `//go:embed all:bin` over that directory — an `embed.FS` of a directory tolerates
     a directory that contains only `.gitkeep`, so **a plain `go build ./...` on a fresh clone still
     compiles**. Missing binaries surface as a clear runtime error, not a compile error.
   - That error names both remedies: run `make guest-bins`, or use a release binary. It is the only
     user-visible cost of not committing them, and `go install …/cmd/krayt@latest` inherits it — say
     so in the README next to the install instructions.
   - A `//go:build` tag with a stub was the alternative. Rejected: it adds a build mode that CI, the
     release workflow, `golangci-lint` and every contributor have to get right, to buy a
     compile-time error over a clear runtime one.
7. **`cmd/krayt-helper` is `//go:build linux`**, matching `internal/guest/*`'s existing discipline —
   it only ever runs inside a guest. Keep `internal/patch` itself tag-free; it is shared.

## What to build

- `cmd/krayt-helper/` — the two subcommands, `//go:build linux`, no dependencies beyond
  `internal/patch` and the standard library. Hand-rolled argv parsing or `flag`; do not pull cobra
  into a binary that ships inside a sandbox.
- `internal/sandbox/guestbin/` — the embed package plus `Binary(name, goarch string) ([]byte, error)`
  and a `GuestPath` per binary for where each is copied (`/.krayt/krayt-helper` or similar — pick a
  path outside `/workspace` so it can never land in the diff, and say why in a comment). The package
  is named `guestbin`, not `helperbin`, and its Makefile target `guest-bins`, not `helper`, because
  `dial-ask-channel-over-vsock.md` adds a second binary (`krayt-ask`) to it — design both for two
  from the start rather than renaming later.
- `Makefile`: a `guest-bins` target cross-building both arches with
  `CGO_ENABLED=0 GOOS=linux GOARCH=… go build -trimpath -ldflags "-s -w"`, and a `.PHONY` entry.
  Make `build`/`test` depend on it where that does not create a chicken-and-egg problem.
- `.gitignore`: the two binaries, not the directory.
- `.github/workflows/ci.yml`: run `make guest-bins` before build/test, and assert the embed is
  non-empty (a test that skips when the binaries are absent is a test that never runs in CI —
  gate it on an env var CI sets, so it is a hard failure there and a skip locally).
- `.github/workflows/release-please.yml`: `make guest-bins` before the cross-build loop at lines 52-70.
  The release job already runs on a Linux runner with Go installed, so this is one line and no new
  runner. Keep `CGO_ENABLED=0` everywhere.
- `KRAYT_SPEC.md` §6.7: describe the helper, its two subcommands, the scope boundary of decision 1,
  and the root-only patch-git property it preserves. Additive — the guest-agent description is still
  true of the running code until cut-over.

## Done when

- `make guest-bins && go build ./...` and `go test -race ./...` are green on darwin and linux, and
  `golangci-lint run` is clean.
- **`go build ./...` succeeds on a fresh clone with no `make guest-bins` run** — the `.gitkeep` +
  `all:bin` property. Prove it in CI or state how you checked.
- `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/krayt-helper` succeeds from macOS.
- Offline tests over `internal/patch` + the helper's own argv layer cover: `setup` produces a
  `krayt-baseline` tag matching the bundle's tip; `SetupPatchGit` runs before the tree is relaxed
  (assert the patch-git dir's mode/ownership is unchanged by the relax step); `finish` produces a
  diff identical to what `internal/guest/service.go` produces today for the same inputs — a
  golden comparison against the existing guest path is the strongest form of this test, and it is
  available precisely because both still exist at this point in the arc.
- A test asserts the helper writes nothing outside `--out` and the patch-git dir.
- `guestbin.Binary` returns a clear, actionable error when the embed is empty.

## Out of scope

- Copying the helper into a sandbox or invoking it — `run-tasks-on-microsandbox.md`.
- Any `ask_human`/question handling.
- Deleting `internal/guest` or `cmd/krayt-agent`.
- Publishing the helper anywhere. It is embedded, never distributed on its own.
