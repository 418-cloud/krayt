# Task: list, remove, and prune cached vmimage/container images (`krayt image ls/rm/prune`)

**Read `CLAUDE.md` and `KRAYT_SPEC.md` (§6.11 image acquisition, §11.4 base image caching, §13 CLI
surface) first. Proceed autonomously — this is a self-contained task run inside a krayt sandbox;
there is no interactive human to approve a plan (use the `ask_human` tool only if genuinely
blocked).**

## Background

krayt caches two kinds of OCI images on the **host** disk, both digest-keyed, neither ever cleaned
up today:

- `~/.cache/krayt/vmimage/<digest-or-sanitized-ref>/` — the base micro-VM image (kernel + initrd +
  rootfs), populated by `krayt image pull` (`internal/vmimage`, §11.4).
- `~/.cache/krayt/imagestore/<digest>/` — the user's agent/container image (e.g. `krayt-dev`,
  `claude-code`), populated automatically before every `krayt run --image <ref>`
  (`internal/imagestore`, §6.11).

Both grow unbounded. `krayt-dev` alone is a multi-GB Go-toolchain image rebuilt on every commit;
iterating on it over weeks leaves many multi-GB directories on disk with no way today to see or
reclaim them. Note (per project convention) that VMs themselves are fully ephemeral — nothing
inside the guest persists across runs — so this is purely a **host-side disk cache** problem; there
is no in-guest state to clean.

## Goal

Add `krayt image ls`, `krayt image rm <digest>`, and `krayt image prune` so a developer can see
what's cached, remove a specific image, or reclaim space in bulk, without ever silently deleting
the currently-pinned base VM image or an image plausibly still in use:

```sh
$ krayt image ls
KIND       DIGEST     REF                              SIZE     LAST USED
vmimage    a0c489cd   krayt-vmimage@sha256:a0c4...    412MiB   2026-07-10 (pinned)
container  9f3e21ab   krayt-dev:latest                 1.8GiB   2026-07-11
container  5b7c0012   claude-code:v0.3.0                980MiB  2026-06-02

$ krayt image rm 5b7c0012
removed 5b7c0012 (980MiB reclaimed)

$ krayt image prune
removed 1 image (980MiB reclaimed); kept 2 (pinned base image + 1 referenced by a running run)
```

## Current behavior (grounding)

- `internal/cli/image.go` — `newImageCmd` (`:15-22`) only has one subcommand today: `pull`
  (`newImagePullCmd`, `:24-49`). `cacheDir(ref, want)` (`:82-92`) computes the vmimage cache path —
  `filepath.Join(base, "krayt", "vmimage", key)`, where `key` is `want.Encoded()` when a digest is
  known, else `sanitizeRef(ref)` (`:94-97`).
- `internal/vmimage/store.go` `Pull` (`:83-114`) always copies into `destDir` — there is no
  cache-hit short-circuit for `krayt image pull` today (unlike `imagestore.Acquire` below).
- `internal/cli/run.go:294-308` `acquireUserImage` computes the imagestore cache root:
  `filepath.Join(base, "krayt", "imagestore")`. `internal/imagestore/imagestore.go` `Acquire`
  (`:64-91`) is keyed by `desc.Digest.Encoded()` under that root and short-circuits (cache hit) when
  `<root>/<digest>/index.json` already exists (`:72-75`) — **this cache-hit path does not update any
  timestamp today**, so there is currently no reliable "last used" signal, only "directory ctime"
  (which only reflects the first pull, not reuse).
- `internal/cli/run_darwin.go:50-55` `baseImageDir` — `krayt run` on macOS always resolves the
  vmimage cache dir via `cacheDir(vmimage.PinnedRef, vmimage.PinnedDigest)`; it never reads any
  *other* digest's directory. So for the vmimage cache, "keep the pinned digest, remove everything
  else" is the complete and correct retention rule — no age/usage heuristic is needed there.
- `internal/orchestrator/state.go` `RunRecord` (`:30-50`) has `ImageRef string` (the raw `--image`
  value the user passed — a tag **or** a digest reference, never the resolved digest) and
  `State string`; `Terminal()` (`:85-87`) reports whether a run has finished. `List(stateDir)`
  (`:132-`) returns every run under `<stateDir>/runs`, newest first. Runs are scoped **per repo**
  (`.krayt/runs/` inside whatever `--repo` points at, default `.`) — there is no cross-repo run
  registry.

## Design decisions (already made — do not re-derive)

1. **Command surface:** `krayt image ls`, `krayt image rm <digest>`, `krayt image prune` —
   parallel to the existing `krayt ls`/`krayt rm` for runs (`internal/cli/manage.go`).
2. **`ls`** lists every entry in both cache roots in one table: `KIND` (`vmimage`|`container`),
   `DIGEST` (12-hex-char short form for display; match/store the full digest internally), `REF`
   (best-effort — see "Last-used tracking" below; `-` if unknown), `SIZE` (recursive directory size,
   human-readable), `LAST USED`, and a `(pinned)` suffix on the vmimage row matching
   `vmimage.PinnedDigest`. Print a totals footer line (`N images, X total`).
3. **`rm <digest>`** accepts a full digest or an unambiguous hex prefix (`docker rmi`-style),
   searches both cache roots, and errors — without deleting anything — if: (a) no entry matches,
   (b) the prefix matches more than one entry, or (c) it's the vmimage entry matching the currently
   pinned digest (require `--force` for that one specifically — removing it just means the next
   `krayt run` fails with the existing, already-actionable "not cached, run `krayt image pull`"
   error, so `--force` is guard enough, not an outright block).
4. **`prune`** deletes by default (no `--dry-run` needed to take effect) under this retention
   policy:
   - **vmimage kind:** keep only the entry matching `vmimage.PinnedDigest` (if set); remove every
     other vmimage entry unconditionally (nothing else is ever read by `krayt run`, per the
     grounding above).
   - **container kind:** keep an entry if **either**:
     a. its last-used time is within `--older-than` of now (default **`24h`**), **or**
     b. its digest matches the `ImageRef` of any **non-terminal** run (`!rec.Terminal()`) found
        under `--repo` (default `.`) **whose `ImageRef` is itself a digest reference**
        (`...@sha256:<hex>` — matched by direct string comparison, no registry resolution). A
        tag-based `ImageRef` cannot be resolved to a cache digest offline and is not specifically
        protected by this rule — rely on (a) for those. This is a known, documented gap, not
        something to silently paper over.
   - `--older-than <duration>` overrides the `24h` default (Go duration syntax, e.g. `72h`).
   - `--all` bypasses **both** container-kind protections above. It still never removes the pinned
     vmimage entry — use `image rm --force <digest>` for that one specifically; `prune` never
     touches it even with `--all`.
   - `--dry-run` prints exactly what would be removed/kept and why, and the total reclaimable size,
     without deleting anything.
   - On completion (non-dry-run), print what was removed and the total size reclaimed, plus a
     one-line summary of what was kept and why (`ls`-style kind/digest, plus "pinned" / "in use by
     <run-id>" / "used <duration> ago").

## Last-used tracking (new)

Neither cache records when an image was last *acquired* today (only first-pull time, and even that
is unreliable on a cache hit, which writes nothing). Add an explicit sentinel file,
`<cache-dir>/.krayt-last-used`, whose own mtime is the signal:

1. In `internal/imagestore/imagestore.go` `Acquire` (`:64-91`), touch the sentinel on **both** the
   cache-hit return (`:73-75`) and after a successful fresh copy (`:84-90`).
2. In `internal/vmimage/store.go` `Pull` (`:83-114`), touch the sentinel after every successful
   pull. (`Pull` has no cache-hit short-circuit today — leave that behavior alone; this task only
   adds the sentinel touch, not a new cache-hit path. `krayt image pull` is an explicit, infrequent,
   user-initiated action, so re-copying on every invocation is an acceptable pre-existing tradeoff,
   out of scope here.)
3. Touching the sentinel is best-effort bookkeeping: on a write error, ignore it (return the
   already-successful `Acquire`/`Pull` result unchanged) — it can only affect the accuracy of the
   `ls` LAST USED column, never correctness of image acquisition.
4. `ls`/`prune` read LAST USED from the sentinel's mtime; if the sentinel is missing (an image
   cached before this change), fall back to the cache directory's own mtime — no error, no special
   marking, just a slightly stale first reading that self-corrects on next use.

## Implement

New package `internal/imagecache`, shared by both cache roots (avoids duplicating "walk a
digest-keyed directory tree, sum sizes, read a sentinel" across `vmimage` and `imagestore`):

```go
package imagecache

type Entry struct {
	Digest   string    // full "sha256:<hex>", or "" for a non-digest-named directory
	Dir      string    // cache directory
	SizeB    int64     // recursive size
	LastUsed time.Time // sentinel mtime, or dir mtime if no sentinel
}

// List returns every entry directly under root. Non-digest-named entries (e.g. stale
// sanitized-ref vmimage dirs from before a pin was set) are still listed, with Digest == "".
func List(root string) ([]Entry, error)

// Remove deletes an entry's directory.
func Remove(e Entry) error

// Touch creates or refreshes an entry's last-used sentinel (best-effort; caller decides
// whether to surface an error).
func Touch(dir string) error
```

Wire `vmimage.Pull` and `imagestore.Acquire` to call `imagecache.Touch` per "Last-used tracking"
above.

New CLI files:
- `internal/cli/image_ls.go` — `newImageLsCmd`: calls `imagecache.List` on both roots (factor a
  shared `imageCacheRoots() (vmimageRoot, imagestoreRoot string, err error)` into
  `internal/cli/image.go`, reusing the same `os.UserCacheDir()`-based computation `cacheDir` and
  `acquireUserImage` already use, and share it across `ls`/`rm`/`prune`), tags each `Entry` with its
  `KIND`, marks the pinned vmimage row, and prints a tabwriter table (mirror `newLsCmd`'s tabwriter
  usage, `internal/cli/manage.go:44-64`).
- `internal/cli/image_rm.go` — `newImageRmCmd`: prefix-match a digest across both roots (a shared
  `resolveDigestPrefix(root, prefix string) (imagecache.Entry, error)` helper returning a clear
  no-match / ambiguous-match error), refuse the pinned vmimage digest without `--force`.
- `internal/cli/image_prune.go` — `newImagePruneCmd`: implements the retention policy above; needs
  `--repo` (default `.`) to read non-terminal runs via `orchestrator.List`
  (`internal/orchestrator/state.go:132`), `--older-than` (default `24h`), `--all`, `--dry-run`.

Register all three under `newImageCmd()` in `internal/cli/image.go`, alongside the existing
`newImagePullCmd()`.

## Tests

- `internal/imagecache` (new, no network — mirror `internal/imagestore/imagestore_test.go`'s style
  of building fixtures directly in `t.TempDir()`): `List` sizes/enumerates correctly, including a
  non-digest-named directory; `Touch` creates then refreshes the sentinel; `Remove` deletes the
  directory.
- `internal/imagestore`: extend or add a sibling to `TestAcquireExportCache` asserting `Acquire`
  writes/updates `.krayt-last-used` on both the fresh-pull and cache-hit paths (call `Acquire` twice
  with the same ref, assert the sentinel's mtime advances on the second call — same in-memory
  `memory.New()` source pattern already in the file, no registry).
- `internal/vmimage`: same idea against `store_test.go`'s existing fixtures for `Pull`.
- `internal/cli` (mirror `internal/cli/manage_test.go`'s `run(t, cmd, args...)` harness and
  `seedRun` helper): seed fake vmimage/imagestore cache roots under `t.TempDir()`. `cacheDir` and
  `acquireUserImage` resolve their roots via `os.UserCacheDir()` today with no override seam — add
  the smallest one that lets tests point at a temp root instead (e.g. a `KRAYT_CACHE_DIR` env var
  checked once, falling back to `os.UserCacheDir()`, used consistently by `cacheDir`,
  `acquireUserImage`, and the new `imageCacheRoots()`). Then:
  - `ls` lists seeded vmimage + container entries with correct KIND/SIZE and marks the pinned
    digest.
  - `rm <digest>` removes the matching directory; ambiguous/no-match prefixes error without
    deleting anything.
  - `rm <pinned-digest>` without `--force` errors; with `--force` succeeds.
  - `prune` with a seeded non-terminal run (`seedRun(t, repo, id, "running")`) whose `image_ref` in
    the seeded `meta.json` is a digest reference matching a cached container entry keeps that entry
    and removes an unrelated, old one; `--older-than 0s` removes everything unpinned/unreferenced;
    `--all` removes even a recently-touched image; `--dry-run` deletes nothing and still reports
    what it would do.

## Docs (required)

- `KRAYT_SPEC.md` §11.4 (base image cache) and §6.11 (image acquisition): document the new
  `ls`/`rm`/`prune` commands, the retention policy, and the last-used sentinel.
- `KRAYT_SPEC.md` §13 CLI surface: add `krayt image pull|ls|rm|prune` (the block currently omits
  `krayt image` entirely).
- `README.md`: mention image cache cleanup wherever disk-space / cache locations are already
  discussed.
- `docs/ai-tasks/README.md`: add this task to the top table with a status.

## Verify (offline)

```sh
go build ./...
GOOS=linux GOARCH=arm64 go build ./...
go test -race ./...
golangci-lint run
```

No new dependency — everything here is stdlib (`os`, `path/filepath`, `time`) plus packages already
imported (`orchestrator`, `vmimage`, `imagestore`). Runs fully offline.

## Done when

- `krayt image ls` shows every cached vmimage/container image with kind, digest, size, and
  last-used, with the pinned base image marked.
- `krayt image rm <digest>` removes exactly the matching image, or gives a clear error for
  no-match / ambiguous / pinned-without-force.
- `krayt image prune` removes everything outside the retention policy above by default, reports
  what it removed and reclaimed, and never removes the pinned base image or an image referenced by
  a non-terminal run (exact for digest-pinned refs and the vmimage pin, best-effort via the age
  floor for tag-based refs) unless `--all` is passed.
- All new logic is unit-tested offline (no registry, no VM); `go build`/`go test -race`/
  `golangci-lint run` pass for both the host and `linux/arm64` guest target.

## Constraints

- Host-side only — no protobuf, no guest, no VM-image rebuild.
- Never delete anything outside the two cache roots.
- Keep `krayt image pull`'s existing copy behavior unchanged (only add the last-used touch).
- Small, focused diff — resist adding auto-pruning-on-run or a size-based `--max-size` trigger;
  this task is manual `ls`/`rm`/`prune` only.
