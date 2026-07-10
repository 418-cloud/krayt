# Contributing to krayt

Thanks for your interest in krayt. This document covers how to get set up, what's expected of a
PR, and where to start a conversation before writing code.

**Found a security vulnerability?** Don't open a public issue — see `SECURITY.md`.

## Before you start

For anything beyond a small, self-contained fix (typo, docs, a clear bug with an obvious fix),
please **open an issue or a discussion first**, describing what you want to change and why.
`KRAYT_SPEC.md` is the source of truth for the design — for anything that touches behavior it
describes, we'll want to agree on the approach before you invest time in an implementation.

## Getting set up

See the README's **Prerequisites** section for the three dependency tiers. In short:

```sh
go build ./...        # OS-agnostic core + macOS provider compile
go test ./...          # unit tests via the fake VM provider — no real VM needed
go run ./cmd/krayt doctor
```

Regenerating protocol code (`internal/protocol/krayt.proto`) or building the VM image needs the
Nix dev shell (`nix develop`) — see the README; most contributions won't need either.

### Hardware constraints (read this before assuming a test failed)

krayt's VM provider (vfkit) only runs on **Apple Silicon macOS** — it's built on
`Virtualization.framework`, which doesn't exist anywhere else. As a result:

- The OS-agnostic core (most of the codebase) builds and unit-tests on any machine via a fake VM
  provider (`internal/provider/fake`) — this is what `go test ./...` exercises, and it's what CI
  runs on every PR.
- Tests behind the `integration,darwin` build tag boot a real VM and **cannot run in CI or on a
  non-Mac machine** — they're skipped by default and only runnable on Apple Silicon hardware with
  vfkit installed.

If you don't have an Apple-Silicon Mac, you can still contribute to and test the core. If your
change touches anything VM/provider/guest-related, say so in the PR description — a maintainer
with real hardware will run the relevant `integration,darwin` test before merging.

## Code style

- Idiomatic Go, `gofmt`-clean, and matching the conventions already present in the package you're
  touching.
- `golangci-lint run` must be clean (see `.golangci.yml` for the configured linters).
- Prefer small, focused diffs over broad refactors bundled with a fix — easier to review, easier
  to revert if something's wrong.
- Don't add abstractions, config knobs, or error handling for cases that can't happen — match the
  existing code's preference for straightforward, direct implementations.

## Commit messages

This repo uses [Conventional Commits](https://www.conventionalcommits.org/) (`fix:`, `feat:`,
`docs:`, `chore:`, etc.) — `release-please` parses them to generate the changelog and version
bumps, so please follow the format. Look at `git log` for examples.

## Before opening a PR

```sh
go build ./...
GOOS=linux GOARCH=arm64 go build ./...   # the guest-agent/core must cross-compile
go test -race ./...
golangci-lint run
```

If your change affects documented behavior, update `KRAYT_SPEC.md` alongside the code — it's the
project's source of truth, and a behavior change without a matching spec update will get flagged
in review.

## Pull request expectations

- Describe **what** changed and **why**, not just what the diff shows.
- Include or update tests for the behavior you changed.
- If a real Apple-Silicon Mac is needed to fully verify the change, say so explicitly — see
  [Hardware constraints](#hardware-constraints-read-this-before-assuming-a-test-failed) above.
- Be responsive to review feedback; for anything non-trivial, expect a design discussion rather
  than a rubber stamp — see [Before you start](#before-you-start).

## License

By contributing, you agree that your contributions are licensed under this project's
[Apache License 2.0](./LICENSE), same as the rest of the codebase. No separate CLA is required.

## A note on how this project is built

The maintainer develops krayt largely with Claude Code, following the working agreement in
`CLAUDE.md` (phase-by-phase development against `KRAYT_SPEC.md`, with a `HUMAN_TODO.md` handoff
log for steps that need real hardware or credentials). You're welcome to read `CLAUDE.md` for
context on how the codebase evolved, but **you don't need to use Claude Code, or follow that
workflow, to contribute** — a standard fork/branch/PR flow with the tests above passing is all
that's expected.
