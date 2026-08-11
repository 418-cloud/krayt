# Task: compress `rootfs.img` for transport, decompress on pull

**Read `CLAUDE.md` and `KRAYT_SPEC.md` §11.4/§11.5/§11.6 first. Proceed autonomously — this is a
self-contained task run inside a krayt sandbox (or handed directly to an agent); there is no
interactive human to approve a plan (use the `ask_human` tool only if genuinely blocked). Every
design decision below has already been made — do not re-derive or second-guess them; implement as
specified.**

## Background

The base VM image's `rootfs.img` is a raw ext4 filesystem image (built by
`images/flake.nix`'s `make-ext4-fs.nix` call) roughly **~2 GiB per architecture**
(`internal/vmimage/store.go:151`'s comment). Today it is pushed to the registry and pulled back
**uncompressed**: `.github/workflows/image.yml:134-137` does `oras push ... rootfs.img:application/vnd.krayt.rootfs`
with no compression step, so the full ~2 GiB crosses the network on every cold pull. On an unstable
connection (a mobile hotspot was the case that surfaced this) a single dropped HTTP/2 stream mid-transfer
fails the whole pull. This task doesn't add retry (that was deliberately rejected — the user should
choose when to re-attempt a large download, not have krayt silently redo it and burn a data cap); it
shrinks what has to cross the network in the first place.

**This does not touch the raw-format requirement.** `KRAYT_SPEC.md:1489,1541` are explicit:
`rootfs.img` must stay **raw** on disk — vfkit boots raw/ISO images only (no qcow2), and it's the
file CoW-cloned per run (`provider.Create`, §6.3). This task only changes what's transmitted over
the wire between CI and the client's local cache; `vmimage.Pull` decompresses immediately after
download, so the file that ends up at `<cache>/vmimage/<digest>/rootfs.img` — and everything
downstream of it (CoW cloning, `krayt doctor`'s `baseImageCheck`, `krayt image ls/rm/prune`) — is
byte-for-byte the same plain raw image as today, unaware compression ever happened. **On-disk cache
footprint is unchanged; only the download shrinks.**

**What already exists and must be reused, not reinvented:**

- `internal/vmimage/store.go:62-77` (`Open`) and `:90-140` (`Pull`) — the pull path. `Pull` calls
  `oras.Copy` into a `file.New(destDir)` store, which extracts each blob to disk under the filename
  given by its `org.opencontainers.image.title` annotation (this is exactly how `vmlinuz`/`initrd`/
  `rootfs.img` land in `destDir` today — `oras push <file>:<mediatype>` sets that annotation from
  the pushed file's basename).
- `internal/vmimage/store.go:192-200` (`verifyFiles`) — checks `img.Kernel`/`img.Initrd`/`img.RootFS`
  exist after `Pull`. Must still pass unchanged — it checks the **final decompressed** `rootfs.img`
  path, which this task must produce regardless of which wire format was pulled.
- `internal/vmimage/store.go:120-126` — on any `oras.Copy` (or, after this task, decompression)
  error, `destDir` is removed before returning, so a rejected/partial artifact never survives to be
  mistaken for a cache hit. Extend this same guarantee to the new decompression step.
- `.github/workflows/image.yml:118-139` (the `publish` job's "Push OCI artifact + record digest"
  step) — per-arch, runs on `ubuntu-24.04`/`ubuntu-24.04-arm`. `result` (the `nix build` output) is
  a **symlink into `/nix/store`, which is read-only** — the existing comment at `:114-117` already
  documents this bit them once for `--config`; it will bite the compression step the same way if
  you try to write `rootfs.img.zst` next to `rootfs.img` inside `result/`.
- `github.com/klauspost/compress v1.18.6` is **already a dependency** (`go.mod:47`, currently
  `// indirect` — pulled in transitively). Its `zstd` subpackage is what this task uses for
  decompression; importing it directly and running `go mod tidy` is a housekeeping move of an
  already-pinned dependency, not "guessing a new library" (CLAUDE.md §9.1).
- `internal/vmimage/store_test.go`'s `fakeArtifact` helper (`:24-55`) — builds an in-memory OCI
  artifact via `oras.land/oras-go/v2/content/memory`, no registry needed. Mirror this pattern for
  the new tests; don't reach for a real registry or `httptest`.

**Decisions already made (do not re-litigate):**

1. **No retry, no resume.** This task is purely "make the download smaller." A failed pull still
   fails outright and cleans up, exactly as today — the user reruns `krayt image pull` when they
   choose to.
2. **zstd, not gzip.** Better ratio and much faster decode than gzip, and the library is already a
   pinned (if indirect) dependency — see above.
3. **rootfs.img only.** The kernel (`vmlinuz`) is tens of MiB (already stripped of debug info for
   x86_64, `images/flake.nix`'s `kernelImage` let-binding) and `initrd` is smaller still; compressing
   either buys negligible bytes for added complexity. Out of scope — leave `vmlinuz`/`initrd` pushed
   and pulled exactly as today.
4. **Old (already-published) vmimage tags keep working.** `Pull` must handle **both** the old raw
   `rootfs.img` media type (every tag published before this change, and reachable again any time
   someone pins/rolls back to one) and the new compressed one. Detect which one arrived and handle
   each — don't require re-publishing history.
5. **No `--long` zstd window.** Simpler and avoids a real pitfall: `zstd --long=N` needs the decoder
   to opt into a matching max window size (`klauspost/compress/zstd`'s `zstd.WithDecoderMaxWindow`)
   or decoding fails outright. Plain `zstd -19 -T0` (no `--long`) keeps window sizes well inside the
   decoder's default limit, so `zstd.NewReader(r)` needs no special options. Not using `--long`
   costs some ratio on a highly-redundant multi-GiB image; that tradeoff is accepted for the
   simplicity — revisit only if the achieved ratio (see Verify) is disappointing.

---

## 1. CI: compress before push (`.github/workflows/image.yml`)

In the `publish` job's existing "Push OCI artifact + record digest" step (`:118-139`), before the
`oras push` line:

```sh
- name: Push OCI artifact + record digest
  run: |
    set -euo pipefail
    tag="${GITHUB_REF_NAME#vmimage-}"
    if [ "${GITHUB_EVENT_NAME}" = "workflow_dispatch" ]; then
      dispatch_tag="${{ inputs.tag }}"
      if [ -n "$dispatch_tag" ]; then
        tag="${dispatch_tag#vmimage-}"
      else
        tag="manual-${GITHUB_SHA::12}"
      fi
    fi
    ref="ghcr.io/${GITHUB_REPOSITORY_OWNER}/krayt-vmimage:${tag}-${{ matrix.arch }}"
    config="${RUNNER_TEMP}/oci-config-${{ matrix.arch }}.json"
    printf '{"architecture":"%s","os":"linux"}' "${{ matrix.arch }}" > "$config"

    # result/ is a symlink into the read-only /nix/store (same reason --config can't live
    # there either, see the comment above) — stage the push inputs in a writable dir instead
    # of trying to write rootfs.img.zst next to rootfs.img.
    command -v zstd >/dev/null || (sudo apt-get update && sudo apt-get install -y zstd)
    stage="${RUNNER_TEMP}/oci-stage-${{ matrix.arch }}"
    mkdir -p "$stage"
    cp result/vmlinuz result/initrd "$stage/"
    zstd -19 -T0 -o "$stage/rootfs.img.zst" result/rootfs.img
    ls -la result/rootfs.img "$stage/rootfs.img.zst"   # sizes in the log, for the ratio check in Verify

    cd "$stage"
    oras push --config "${config}:application/vnd.oci.image.config.v1+json" "$ref" \
      vmlinuz:application/vnd.krayt.kernel \
      initrd:application/vnd.krayt.initrd \
      rootfs.img.zst:application/vnd.krayt.rootfs+zstd
    digest="$(oras manifest fetch "$ref" --descriptor | jq -r .digest)"
    echo "::notice title=krayt-vmimage (${{ matrix.arch }})::pushed $ref  digest=$digest"
```

Notes:
- `command -v zstd` defensively installs it — GitHub-hosted `ubuntu-24.04`/`ubuntu-24.04-arm`
  runners are expected to already carry it, but this task's own author couldn't confirm that from a
  sandbox with no access to a real runner; don't assume, guard it.
- `zstd -T0` uses all available cores; `-19` is a high (slow-ish) compression level chosen for ratio
  since this only runs on RC/graduate tag pushes, not every commit. If real CI runs show this step
  taking long enough to matter, dropping to `-15` or `-12` is an acceptable, easy follow-up — note
  the actual measured time in `HUMAN_TODO.md` either way (see Verify).
- `vmlinuz`/`initrd` are just copied (cheap, tens of MiB) into the staging dir so the whole push
  still reads from one cwd-relative location, exactly as today — no change to how those two are
  pushed or pulled.
- Media type: `application/vnd.krayt.rootfs+zstd` — the `+zstd` suffix follows the same
  compression-suffix convention OCI itself uses (e.g. `...tar+gzip`), applied to this artifact's own
  custom vendor media type.

Update the doc sketch too — `KRAYT_SPEC.md:1454-1458`'s `oras push` example currently shows the old
uncompressed line; update it to match (see Docs section).

## 2. Client: recognize + decompress on pull (`internal/vmimage/store.go`)

New constants, next to the existing `File*`/`MediaType*` block (`:32-45`):

```go
// FileRootFSZstd is the on-wire/on-disk name of rootfs.img when zstd-compressed for transport
// (introduced to shrink the ~2 GiB cold-pull download). Pull decompresses it into FileRootFS
// immediately, so nothing downstream of Pull (CoW cloning, doctor, image ls/rm/prune) ever sees
// this name or cares that compression happened on the wire.
const FileRootFSZstd = FileRootFS + ".zst"

// MediaTypeRootFSZstd is rootfs.img's blob media type when zstd-compressed. Artifacts published
// before this task used MediaTypeRootFS (uncompressed) and remain pullable — Pull checks for
// FileRootFSZstd's presence after the copy and only decompresses when it's there.
const MediaTypeRootFSZstd = MediaTypeRootFS + "+zstd"
```

In `Pull` (`:90-140`), after the `oras.Copy` call succeeds and before `img.verifyFiles()`:

```go
if compressed := filepath.Join(destDir, FileRootFSZstd); fileExists(compressed) {
    if err := decompressRootFS(compressed, img.RootFS); err != nil {
        _ = os.RemoveAll(destDir)
        return nil, fmt.Errorf("vmimage: decompress rootfs: %w", err)
    }
}
```

(`fileExists` — a trivial `os.Stat` + `err == nil` helper if one doesn't already exist in this
package; do not add a dependency for this.)

Add `decompressRootFS`:

```go
// decompressRootFS streams the zstd-compressed blob at src into dst as plain raw bytes, then
// removes src — only the decompressed file is kept, so the cache never pays double disk for the
// same content twice. Writes through a same-directory temp file + rename so an interrupted or
// failed decompression (killed process, corrupt stream, disk full) can never leave a partial,
// truncated dst mistaken for a good cached rootfs — mirrors Pull's own "no half-written cache
// survives an error" guarantee (store.go:120-126).
func decompressRootFS(src, dst string) error {
    f, err := os.Open(src)
    if err != nil {
        return fmt.Errorf("open %s: %w", src, err)
    }
    defer func() { _ = f.Close() }()

    dec, err := zstd.NewReader(f) // default options: window sizes from plain `zstd -19` (no
    if err != nil {               // --long) are well inside the default max window, no
        return fmt.Errorf("zstd reader: %w", err) // WithDecoderMaxWindow needed — see decision 5.
    }
    defer dec.Close()

    tmp, err := os.CreateTemp(filepath.Dir(dst), ".rootfs-*.tmp")
    if err != nil {
        return fmt.Errorf("create temp file: %w", err)
    }
    tmpPath := tmp.Name()
    if _, err := io.Copy(tmp, dec); err != nil {
        _ = tmp.Close()
        _ = os.Remove(tmpPath)
        return fmt.Errorf("decompress: %w", err)
    }
    if err := tmp.Close(); err != nil {
        _ = os.Remove(tmpPath)
        return fmt.Errorf("close temp file: %w", err)
    }
    if err := os.Rename(tmpPath, dst); err != nil {
        _ = os.Remove(tmpPath)
        return fmt.Errorf("rename into place: %w", err)
    }
    if err := os.Remove(src); err != nil {
        return fmt.Errorf("remove compressed source %s: %w", src, err)
    }
    return nil
}
```

Import `github.com/klauspost/compress/zstd`; run `go mod tidy` afterward so `go.mod` drops the
`// indirect` marker on `github.com/klauspost/compress` now that it's imported directly.

No context/cancellation plumbing for the decompression step — it's a bounded, local, CPU-bound copy
(unlike the network download `oras.Copy` already does under `ctx`); adding cancellation here would
be complexity with no real payoff for something this fast.

**Backward compatibility, concretely:** an old pinned digest (or `--ref`/`--digest` pointing at a
tag published before this task) has no `rootfs.img.zst` in `destDir` after `oras.Copy` — `oras.Copy`
already wrote the plain `rootfs.img` directly (media type `MediaTypeRootFS`, unchanged). The
`fileExists` check is `false`, the decompression branch is skipped entirely, and `Pull` behaves
exactly as it does today. Nothing needs to look at the blob's media type explicitly — presence of
`FileRootFSZstd` on disk after the copy is sufficient and simpler.

## Tests

Add to `internal/vmimage/store_test.go` (mirror `fakeArtifact`'s in-memory-store pattern — no real
registry, no real zstd CLI subprocess; compress the fixture bytes with the `zstd` package directly
in Go):

```go
// fakeArtifactCompressed is fakeArtifact's rootfs layer swapped for a zstd-compressed one, media
// type MediaTypeRootFSZstd, titled FileRootFSZstd — proves Pull's decompression path end to end.
func fakeArtifactCompressed(t *testing.T, plaintext []byte) (oras.ReadOnlyTarget, string, digest.Digest) {
    t.Helper()
    // ... same shape as fakeArtifact, but the rootfs layer is:
    //   enc, _ := zstd.NewWriter(nil)
    //   compressed := enc.EncodeAll(plaintext, nil)
    //   layer(vmimage.MediaTypeRootFSZstd, vmimage.FileRootFSZstd, compressed)
}
```

- `TestPullDecompressesRootFS` — pull the compressed fixture; assert `img.RootFS` contains
  `plaintext` byte-for-byte; assert `filepath.Join(dest, vmimage.FileRootFSZstd)` no longer exists
  (the `.zst` is removed after a successful decompress, not left behind alongside the plain file).
- `TestPullExtractsAndVerifies` (existing, `store_test.go:57-81`) — must keep passing **unchanged**.
  It exercises the old uncompressed `fakeArtifact` fixture; its continuing to pass is the regression
  guard for decision 4 (old artifacts still pull correctly, no compression-path code runs for them).
- `TestPullRejectsCorruptZstdStream` — push a `.zst` layer whose bytes are **not** a valid zstd
  stream (e.g. `[]byte("not zstd")`) under `MediaTypeRootFSZstd`/`FileRootFSZstd`; assert `Pull`
  returns an error and, per the existing cleanup contract, `destDir` does not survive (same
  assertion style as `TestPullRejectsDigestMismatch`, `:97-112`: `os.Stat(dest)` →
  `os.IsNotExist`).
- `TestPullTouchesLastUsed` (existing, `:83-95`) — no change needed, but confirm it still passes
  against the compressed fixture too (either parametrize it or add a second call using
  `fakeArtifactCompressed` — either is fine, just don't leave the compressed path's last-used
  bookkeeping unverified).

## Docs (required)

- `KRAYT_SPEC.md`:
  - §11.5's sketch (`:1454-1458`) — update the `oras push` example line for `rootfs.img` to show
    the compressed push (`rootfs.img.zst:application/vnd.krayt.rootfs+zstd`), with a one-line note
    that the client decompresses on pull.
  - §11.6 "Output artifacts" (`:1489-1494`) — add a sentence: `rootfs.img` is compressed
    (`+zstd`) for the registry transfer only; `krayt image pull` decompresses it back to the same
    raw format before anything touches it, so the raw-on-disk / CoW-clone contract stated here is
    unchanged.
  - §11.4 (`:1387-1406`) — no factual change needed (still "packaged as a standard OCI artifact",
    still digest-verified, still cached raw) but fine to add a short parenthetical on the same
    "Distribute" bullet noting the rootfs layer is compressed in transit.
- `docs/ai-tasks/README.md` — add a row for this task once implemented, following the existing
  format (Task / What it does / Status).

## Verify

**Offline (must pass in this sandbox, no registry, no real `zstd` CLI needed for the Go side):**

```sh
go build ./...
GOOS=linux GOARCH=arm64 go build ./...
go test -race ./...
golangci-lint run
go mod tidy && git diff --exit-code go.mod go.sum   # confirm klauspost/compress moved off `// indirect` cleanly
```

**Cannot be verified from this sandbox — hand off honestly via `HUMAN_TODO.md`, do not fabricate:**
building the real ~2 GiB rootfs and running the real CI compression step needs a Linux builder /
real GitHub Actions run (same category as every other vmimage CI change in this repo's history —
CLAUDE.md's "what needs real hardware" list, "a Linux builder or CI run to build/boot the Nix
image"). When that real run happens, record in `HUMAN_TODO.md`:
- The **actual measured sizes** from the `ls -la` line in §1's script (uncompressed vs. `.zst`) and
  the resulting ratio — this task exists to shrink the download, so the real number is the point;
  don't estimate or guess it.
- Actual CI wall-clock time added by the `-19 -T0` compression step, so the level-vs-time tradeoff
  in decision 3 is a real data point, not a guess.
- A real `krayt image pull` against the newly-published compressed artifact, confirming
  `img.RootFS` boots correctly on real hardware afterward (a decompression bug that silently
  truncates or corrupts the image would otherwise only surface at boot, not at pull time).

## Done when

- CI compresses `rootfs.img` with zstd before pushing, for both arches, under
  `application/vnd.krayt.rootfs+zstd` / `rootfs.img.zst`; `vmlinuz`/`initrd` are unchanged.
- `vmimage.Pull` decompresses a `rootfs.img.zst` blob into a plain `rootfs.img` after copy, removes
  the compressed file, and leaves everything downstream (`verifyFiles`, CoW cloning, `krayt doctor`,
  `image ls/rm/prune`) working exactly as before, unaware compression occurred.
- Pulling an **old, already-published** (pre-this-task) vmimage tag still works unchanged — no
  regression for existing pinned digests or manual `--ref`/`--digest` rollbacks.
- A corrupt/invalid `.zst` blob fails `Pull` cleanly with no leftover `destDir`, same as every other
  `Pull` failure mode today.
- All of the above is unit-tested offline (in-memory OCI store, no registry, no `zstd` subprocess
  needed for the Go tests); `go build`/`go test -race`/`golangci-lint run` pass for both host and
  `linux/arm64` cross-compile; `go.mod` cleanly reflects `klauspost/compress` as a direct dependency.
- `KRAYT_SPEC.md` §11.4/§11.5/§11.6 and `docs/ai-tasks/README.md` are updated.
- The real compression ratio, real CI time cost, and a real post-decompress boot are logged in
  `HUMAN_TODO.md` once a real CI run happens — not fabricated in the meantime.

## Constraints

- No retry/resume logic anywhere in this task — explicitly out of scope, see decision 1.
- `rootfs.img` only — do not compress `vmlinuz`/`initrd` (decision 3).
- Do not use `zstd --long` without also setting a matching `zstd.WithDecoderMaxWindow` — either keep
  both sides on plain (non-`--long`) settings as specified, or change both together deliberately;
  never let them drift (decision 5).
- Never leave both a `.zst` and a decompressed `rootfs.img` on disk after a successful `Pull` — only
  the decompressed file survives, to avoid doubling the on-disk cache footprint for no benefit.
- Never let a failed/interrupted decompression leave a partial `rootfs.img` that a later `Pull`
  could mistake for a valid cache hit — temp file + rename, exactly as specified in §2.
- Keep the on-wire compression entirely invisible to everything outside `vmimage.Pull` — no other
  package should need to know `rootfs.img` was ever compressed.
