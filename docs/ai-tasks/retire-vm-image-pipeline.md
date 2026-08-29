# Task: retire the Nix VM image pipeline and rewire `krayt image` onto msb's store

**Read `CLAUDE.md`, `docs/adr-microsandbox-sandbox-layer.md` ("What is actually at stake"), and
`KRAYT_SPEC.md` §6.11, §11 (all of it), §13 first.** Give a short plan (the deletion list, the four
`krayt image` subcommands' new implementations) and proceed. **Depends on
`run-tasks-on-microsandbox.md`**, including its hardware `Done when` — this is the task that makes
the migration irreversible in practice, so it should not land before a real run has passed.

## Background

`run-tasks-on-microsandbox.md` deleted everything that *executes*. What is left is an artifact
pipeline with no consumer: a Nix flake that builds a NixOS rootfs + kernel + initrd, the CI that
builds it on a Linux arm64 runner, the OCI publish/pull/zstd path, the RC-tagging and graduation
workflows, and the host-side cache and digest verification that fed the (now deleted) vsock
pre-load. None of it has a caller.

The ADR's LOC table understates this column, and deliberately: beyond `internal/vmimage`'s 771 lines
and `images/*.nix`'s 266, it also removes the Nix image CI and its Linux-builder requirement, the
backend-tagged image variants, and the `zstd` compression path — the machinery
`automate-vmimage-releases.md` and `compress-vmimage-rootfs.md` exist to maintain. That machinery is
the "40% that cannot be verified without an Apple-Silicon Mac or a KVM host" the ADR is mostly about.

`krayt image` is the one user-facing thing here worth keeping. msb has a direct equivalent for every
subcommand, all with `--format json`, so krayt keeps the UX its users already have and stops owning
a cache.

## Decisions already made (do not re-litigate)

1. **Delete, in full:** `internal/vmimage`, `images/` (the flake, its lock, and the `.nix` files),
   `.github/workflows/{image,vmimage-rc,vmimage-graduate}.yml`, `hack/next-vmimage-tag.sh`, and
   `internal/imagecache`. The base-image `PinnedRef`/`PinnedDigest` constants and every reference to
   them go with them.
2. **`krayt image` survives as a thin front-end over msb's store**, not as a reimplementation of it.
   Do not read, walk or delete anything under `MSB_HOME` — msb is beta and its cache layout is its
   own business. Shell out through `internal/sandbox`:
   | krayt | msb |
   |---|---|
   | `krayt image pull <ref>` | `msb pull <ref>` (the base-VM `--ref`/`--digest` flags are deleted; there is no base VM image any more) |
   | `krayt image ls` | `msb images --format json`, rendered in krayt's existing table shape |
   | `krayt image rm <ref>` | `msb rmi` (`--force` maps to msb's `--force`, which allows removing an image a sandbox still references) |
   | `krayt image prune` | see decision 3 |
3. **`krayt image prune` keeps its age retention by using run records, not a cache sidecar.**
   msb's `image prune` removes images not used by any sandbox or indexed snapshot, but has no
   age policy — krayt's `--older-than` (default 24h), `--repo`, `--all` and `--dry-run` do. Keep
   them, implemented as: collect image refs from `.krayt/runs/*/meta.json` (which already records
   `image_ref`), treat a ref used by a non-terminal run or within the window as protected, `msb rmi`
   the rest, then `msb image prune` to sweep dangling artifacts. This is strictly better than the
   `.krayt-last-used` sentinel it replaces — the run records are the real evidence of use, and they
   already exist.
   `--dry-run` must still report without deleting, and must not call `msb rmi` at all.
4. **`krayt image rm` takes a reference, not a digest.** Today it takes a digest or unambiguous
   prefix, because krayt owned a content-addressed store. msb's store is ref-keyed. This is a CLI
   surface change: update §13, the shell completion (`internal/cli/complete.go` completes cached
   digests today), and the README.
5. **The Linux-builder requirement disappears and should be recorded as gone.** `KRAYT_SPEC.md`
   §11.3 ("the macOS build caveat — settled: build in CI") and `docs/macos-linux-builder.md` exist
   only to serve the Nix image build. Delete the doc; replace §11 with a short section saying the
   sandbox image is msb's concern and krayt ships no VM image, and pointing at the ADR.
6. **Two open questions close here and should be marked closed, not silently dropped.** §15's
   linux/arm64 entry was blocked because the image index carried an arch dimension but no backend
   dimension (`internal/vmimage/store.go:217` matched on `runtime.GOARCH` alone); with no image
   there is no index and no conflict. Windows had no path at all. Both become
   `expand-platforms-under-msb.md`'s subject — record that in §15 rather than deleting the entries.

## What to build

- The deletions of decision 1, plus every dangling reference: `flake.nix`'s image inputs if any,
  `Makefile` targets, `renovate.json` managers that track the image tags, `RELEASING.md`'s
  vmimage-graduation section, and `.gitignore` entries for image artifacts.
- `internal/cli/image*.go` rewritten against `internal/sandbox`, keeping the existing flag names,
  output shape and completion behaviour wherever msb can back them, and deleting the flags it
  cannot (decision 2's `--ref`/`--digest`).
- `KRAYT_SPEC.md`: replace §11 wholesale, delete §6.11, update §13's CLI surface, and mark §15's
  linux/arm64 and Windows entries as unblocked. `docs/ai-tasks/README.md`: update the status rows for
  `automate-vmimage-releases.md`, `compress-vmimage-rootfs.md` and `prune-cached-images.md` to say
  what replaced them — those rows are a durable record and must not be left pointing at deleted
  files.
- `README.md`: the install section no longer mentions `krayt image pull` as a first-run step (msb
  pulls on demand), and the prerequisites are `msb` alone.

## Done when

- `go build ./...` (both `GOOS`), `go test -race ./...` and `golangci-lint run` are green.
- `grep -rn "vmimage\|imagecache\|rootfs.img\|PinnedDigest" --include=*.go --include=*.yml --include=*.nix .` returns nothing.
- `.github/workflows/` contains no job that needs a Linux arm64 runner or a Nix builder.
- `krayt image ls|rm|prune --dry-run` are unit-tested offline against the fake `msb`, including the
  run-record-based retention of decision 3 (protected by a non-terminal run, protected by the age
  window, and pruned when neither applies).
- Shell completion still completes image references for `rm`, now sourced from `msb images -q`.
- `HUMAN_TODO.md` carries no entry that only existed to serve the image pipeline; any that did are
  deleted **after** their outcome is recorded in §14's phase checkboxes and this file's row, per
  §14's entry lifecycle.

## Out of scope

- Anything that runs a sandbox.
- Platform expansion — `expand-platforms-under-msb.md`.
- Snapshots and flat rootfs — `warm-start-msb-sandboxes.md`. Note that `--root-disk flat:…` and
  `msb pull --materialize flat` live in this command's neighbourhood; leave them alone here.
