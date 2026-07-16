# Task: record run provenance (commit SHAs + bundle/metadata digests) in meta.json and report.md

**Read `CLAUDE.md` and `KRAYT_SPEC.md` §6.7 (Code transfer & patch generation) and §8.4 (Run
output artifacts) first.** Give a short plan (the `CreateBundle`/`pushCode` signature changes,
the new `RunRecord` fields, the report section) and proceed — this is a contained addition (a
few new fields threaded through code that already computes most of the values), not a
repo-wide change. Still stop and ask if you hit something this doc doesn't cover.

## Background — why this needs care, not just "add a SHA field"

`meta.json` (§8.4) records `repo_path` but nothing about *which* commit a run was based on. The
naive fix — `git rev-parse HEAD` at bundle time — is subtly wrong on its own, because of how
`internal/patch.CreateBundle` (§6.7) actually builds what the guest imports as HEAD:

- In the **default** mode (`bundle_depth` defaults to `1`, a snapshot), the guest's imported
  HEAD is **never** the real HEAD SHA — it's a synthetic, **parentless** commit built by
  `commit-tree` from HEAD's tree (`patch.go`'s `snapshot` case), so bundling a shallow clone
  stays self-contained (§6.7: "a snapshot's baseline is synthetic").
- When `include_dirty` is set (either bundle depth), the imported HEAD is a *further* synthetic
  commit — `captureWorkTree` + `commit-tree`, parented on the real HEAD only in full-history
  mode, parentless in snapshot mode — folding in uncommitted changes without touching the
  user's real index/worktree/refs (§6.7 "Non-mutating dirty capture").
- Only in the one case of **full history + no dirty changes** does the bundle's HEAD equal the
  real `git rev-parse HEAD` exactly.

So "the commit SHA" is ambiguous between (a) the real, permanent, checkoutable HEAD the user
had, and (b) the synthetic commit `changes.patch` is actually diffed against inside the guest —
and `CreateBundle` already computes both today as local variables (`head`/`tip` in the
`includeDirty` branch, `tree`/`tip` in the `snapshot` branch), it just never returns them.
**Record both, distinctly labeled** (confirmed with the repo owner, rather than guessing which
one "the commit SHA" meant):

- `head_sha` — the real `git rev-parse HEAD` at bundle time (empty for an unborn-HEAD repo).
  Permanent, checkoutable, answers "what named commit was this run based on."
- `bundle_sha` — the commit actually imported as the guest's `krayt-baseline` tag and diffed
  against for `changes.patch`. Equals `head_sha` only in the full-history/no-dirty case;
  otherwise a synthetic commit not reachable from any of the user's branches. Answers "what
  exact tree did the agent actually operate on."

Also record `bundle_depth` and `include_dirty` — the request flags (`spec.BundleDepth`,
`spec.IncludeDirty`, already in `task.RunSpec` but not persisted anywhere in `meta.json`)
alongside, since they're what determines whether `head_sha == bundle_sha` is *expected*;
without them a future reader can't tell a fidelity gap from a bug.

**The bundle digest** (a second, independent piece, also confirmed in-scope): §6.7's own
"Integrity" paragraph already promises this and has never implemented it — "The bundle is a
single artifact, hashable and checkable (`git bundle verify` plus a digest), consistent with
the digest discipline already used for the OCI artifact (§6.11) and secrets (§6.8)."
`github.com/opencontainers/go-digest` (`v1.0.0`) is already a pinned dependency (§9.1) used for
exactly this convention in `internal/imagestore` and `internal/vmimage` — reuse it
(`digest.FromBytes` / `digest.Canonical.FromReader`, the `sha256:<hex>` string shape) rather
than hand-rolling `crypto/sha256`. Record it as `bundle_digest` — a digest of the actual bundle
bytes streamed to the guest.

**The metadata hash in report.md** (confirmed with the repo owner): this is **a drift/
consistency check, not tamper-evidence**, and must be documented as such rather than implied to
be more. `meta.json` and `report.md` are written back-to-back in the same function
(`orchestrator.Run`'s deferred finalizer → `writeRecord` then `writeReport`), from the same
in-memory `RunRecord`, by the same trusted host process — so a hash of `meta.json` embedded in
`report.md` cannot detect a deliberate, *consistent* edit of both files after the fact. What it
*does* do: let someone holding `report.md` separately from `meta.json` (e.g. one pasted into a
ticket) confirm the two still match, or notice `meta.json` was later corrupted/overwritten.
Label it exactly that way in the rendered report — do not word it as an integrity/security
guarantee.

## Decisions already made (do not re-litigate)

1. **`CreateBundle`'s signature changes** to return the values it already computes internally
   instead of just `error`:
   ```go
   type BundleResult struct {
       HeadSHA   string // "" if unborn HEAD
       BundleSHA string // always set on success — the tip actually bundled
   }
   func CreateBundle(ctx context.Context, repoPath, outBundle string, depth int, includeDirty bool) (BundleResult, error)
   ```
   Resolve `HeadSHA` once via `rev-parse HEAD` whenever `hasCommits` — hoisted above the
   `switch` so every branch has it (today only the `includeDirty` branch computes a `head`
   local, and the `snapshot`-only branch doesn't need it for the commit itself but must still
   report it). `BundleSHA` is whatever `tip` ends up being, falling back to `HeadSHA` in the
   `default:` case (full history, no dirty) where `tip` is never set today.

2. **`pushCode` (`internal/orchestrator/orchestrator.go`) computes `bundle_digest`** right
   after `CreateBundle` succeeds (the bundle file is already on disk at that point, before
   `client.PushCode` streams it) and returns a populated `ProvenanceMeta` (new type, below)
   alongside its existing `error`. `Run` stores it on `rec.Provenance` right after the
   `pushCode` call (~`orchestrator.go:187`), before the `rec.State = StateRunning` write, so
   it's already captured if a later step fails and only `writeReport`'s deferred call ever
   runs. Leave `rec.Provenance` nil if `pushCode` fails before returning (mirrors how
   `rec.Patch` stays nil until a patch is actually collected) — no zero-value placeholders in
   `meta.json`.

3. **New `RunRecord` field**, matching the existing `NetworkMeta`/`ResourceMeta` nested-struct
   pattern in `internal/orchestrator/state.go`:
   ```go
   type ProvenanceMeta struct {
       HeadSHA      string `json:"head_sha,omitempty"`
       BundleSHA    string `json:"bundle_sha"`
       BundleDepth  int    `json:"bundle_depth"`
       IncludeDirty bool   `json:"include_dirty,omitempty"`
       BundleDigest string `json:"bundle_digest"`
   }
   // on RunRecord:
   Provenance *ProvenanceMeta `json:"provenance,omitempty"`
   ```

4. **`writeRecord` returns the digest of the bytes it just wrote** (`internal/orchestrator/
   state.go`), so `writeReport` can render it without a second read-and-rehash of the file:
   ```go
   func writeRecord(runDir string, rec RunRecord) (digest.Digest, error)
   ```
   Compute the digest (`digest.FromBytes`, go-digest) over the exact marshaled bytes written to
   `meta.json` — not a separate `json.Marshal` call, so it can never drift from what's actually
   on disk. `orchestrator.go`'s finalizer threads this into `writeReport`.

5. **`writeReport` gains a `metaDigest digest.Digest` parameter** and renders a new
   `## Provenance` section (only when `rec.Provenance != nil` — mirrors the existing optional
   `## Safety`/`## Questions` sections), e.g.:
   ```
   ## Provenance
   - Commit: <head_sha>  (bundle: <bundle_sha>, depth: <bundle_depth>, dirty: <yes|no>)
   - Bundle digest: <bundle_digest>
   - Metadata digest (consistency check, not a signature): <metaDigest>
   ```
   The parenthetical on the metadata digest line is load-bearing — keep it, per the Background
   note above. If `head_sha == bundle_sha`, still print both (don't special-case away the
   redundancy — a reader scanning many reports benefits from the field always being in the same
   place).

6. **`KRAYT_SPEC.md` updates**: §8.4's example `meta.json` JSON blob gains the `provenance`
   object; its `report.md` template gains the `## Provenance` section; §6.7's "Integrity"
   paragraph changes from promising a digest to stating where it's actually recorded now
   (`bundle_digest` in `meta.json`) — don't leave the old promise dangling once it's
   implemented.

7. **Scope**: the optional reverse `commits.bundle` (§6.7) is untouched — no digest for it in
   this task. `task.RunSpec`'s existing `BundleDepth`/`IncludeDirty` fields are read, not
   changed.

## Deliverables

1. `internal/patch/patch.go` — `CreateBundle` returns `(BundleResult, error)` per decision 1.
2. `internal/orchestrator/orchestrator.go` — `pushCode` computes `bundle_digest` and returns
   `(ProvenanceMeta, error)`; `Run` wires it into `rec.Provenance` per decision 2.
3. `internal/orchestrator/state.go` — `ProvenanceMeta` type + `RunRecord.Provenance` field;
   `writeRecord` returns `(digest.Digest, error)` per decisions 3–4.
4. `internal/orchestrator/report.go` — `writeReport` takes `metaDigest digest.Digest` and
   renders the `## Provenance` section per decision 5.
5. `KRAYT_SPEC.md` — §6.7 and §8.4 updates per decision 6.
6. `docs/ai-tasks/README.md` — add this task's row once done.

## Verify

What you can do yourself, all offline (no VM needed — `CreateBundle` and the digest/record
logic are pure host-side git-shelling and hashing, consistent with `CLAUDE.md`'s "test the core
against `fakeProvider`" rule):
```sh
go test ./internal/patch/... ./internal/orchestrator/...
```
- `patch_test.go`: exercise `CreateBundle` against scratch repos in each of five states — full
  history/no dirty (`BundleSHA == HeadSHA`), snapshot/no dirty (`BundleSHA != HeadSHA`, both
  non-empty), snapshot/dirty (parentless, `BundleSHA != HeadSHA`), full-history/dirty (parented
  on `HeadSHA`, still `!=`), and unborn-HEAD + dirty (`HeadSHA == ""`, `BundleSHA` set) — and
  assert the returned SHAs match `git rev-parse` run independently against the scratch
  repo/clone, not just "non-empty."
- `report_test.go` / `report_internal_test.go`: assert the rendered `## Provenance` section is
  absent when `rec.Provenance == nil`, and that a re-read of the written `meta.json`'s bytes,
  hashed independently, equals the `metaDigest` string printed in `report.md`.
- Confirm `go vet` / `golangci-lint run` (repo convention) still pass with the new signatures.

Nothing here needs real hardware or a live VM — this is pure host-side logic already covered by
the existing fake-provider test strategy, so no `HUMAN_TODO.md` entry is expected for this task.

## Done when

- `meta.json` for a completed run has a `provenance` object with `head_sha`, `bundle_sha`,
  `bundle_depth`, `include_dirty`, and `bundle_digest`, all matching independently-verified
  values (not just present).
- `report.md` shows the `## Provenance` section (commit SHAs + both digests) when provenance
  was captured, with the metadata digest explicitly labeled as a consistency check.
- `KRAYT_SPEC.md` §6.7/§8.4 reflect the implemented reality, not the old unfulfilled promise.
- All new/changed logic is unit-tested offline; `go test ./...` passes.

## Constraints

- Don't add any new third-party dependency — `github.com/opencontainers/go-digest` is already
  pinned (§9.1); reuse it rather than hand-rolling `crypto/sha256`.
- Don't touch `commits.bundle` (the optional reverse bundle) — out of scope.
- Don't imply the metadata digest is a security control anywhere (code comments, `report.md`
  text, or `KRAYT_SPEC.md` prose) — it's explicitly a drift/consistency check only, per
  Background.
- Keep `rec.Provenance` nil (not a zero-value struct) when `pushCode` never completed — mirrors
  the existing `rec.Patch` convention.

## Output

When this task is done, output a suggested branch name and commit message (don't create the
branch or commit yourself unless separately asked to) — kebab-case branch name matching this
file's own naming (e.g. `record-run-provenance`), and a Conventional Commits message. This is
CLI-facing behavior (new `meta.json`/`report.md` fields), so type it `feat:`.
