# Task: `krayt upgrade` — in-place self-update from GitHub Releases

**Read `CLAUDE.md` and `KRAYT_SPEC.md` (§13 CLI surface) first. Proceed autonomously — this is a
self-contained task run inside a krayt sandbox (or handed directly to an agent); there is no
interactive human to approve a plan (use the `ask_human` tool only if genuinely blocked). Every
design decision below has already been made — do not re-derive or second-guess them; implement as
specified.**

## Background

Upgrading krayt today means manually downloading a tarball from the GitHub Releases page and
replacing the binary on disk — see `README.md`'s "Prebuilt binaries" paragraph (`README.md:32-40`).
This task adds `krayt upgrade`, a command that does that same thing automatically: find the latest
(or a specific) release, download the right platform tarball, verify it against the release's
published `checksums.txt`, and atomically replace the currently-running binary.

**What already exists and must be reused, not reinvented:**

- `internal/cli/root.go:12` — `var Version = "0.6.1" // x-release-please-version`, the CLI's own
  version string. **Bare, no `v` prefix.**
- Release tags and GitHub Release names are **`v` + `Version`** (e.g. tag `v0.6.1` for CLI version
  `0.6.1`) — confirmed against the real repo's latest release. `RELEASING.md` also documents this
  ("tags `vX.Y.Z`"). Every place that compares `Version` to a tag, or builds an asset filename from
  a tag, must account for this prefix explicitly — it is a real, easy-to-miss bug source, not a
  hypothetical.
- `.github/workflows/release-please.yml:60-72` builds exactly three targets — `darwin/arm64`,
  `darwin/amd64`, `linux/amd64` — and uploads two kinds of asset per release:
  `krayt_${TAG}_${os}_${arch}.tar.gz` (a tar.gz containing one file, `krayt`, at its root — see the
  workflow's `tar -C dist -czf ... krayt`, `:69`) and one shared `checksums.txt`
  (`sha256sum *.tar.gz > checksums.txt`, `:72` — GNU coreutils text-mode format, confirmed against
  the real published file: `<64-hex-char digest><two spaces><filename>`, one line per tarball, no
  `*` prefix). There is no `linux/arm64` build (see `README.md:36-40` for why — the base VM image
  isn't just arch-tagged).
- Confirmed against the real repo's latest release JSON (`GET
  https://api.github.com/repos/418-cloud/krayt/releases/latest`): each asset has a
  `browser_download_url` of the form
  `https://github.com/418-cloud/krayt/releases/download/<tag>/<asset-name>`, which redirects to the
  actual CDN host — **do not hardcode the redirect target**, just follow it (Go's default
  `http.Client` follows redirects automatically).
- `cmd/krayt/main.go` already builds a signal-cancelable `context.Context`
  (`signal.NotifyContext(..., os.Interrupt, syscall.SIGTERM)`) and calls
  `cli.NewRootCmd().ExecuteContext(ctx)` — every cobra `RunE` already receives that context via
  `cmd.Context()`. Use it for every network call in this task so Ctrl-C aborts cleanly.
- `internal/cli/run.go:443` already reads from `cmd.InOrStdin()` (not raw `os.Stdin`) — follow that
  convention here too, so tests can inject stdin.
- No existing code in this repo does a raw HTTPS download + checksum verify (the closest relative,
  `internal/imagestore`, talks to an OCI registry via `oras-go`, a different protocol entirely) —
  this task's download/verify logic is new, stdlib-only, and should not try to reuse `imagestore`.

**Decisions already made (do not re-litigate):**

1. **On-demand only.** `krayt upgrade` is the *only* thing that ever talks to GitHub. No other
   command does a background/passive update check. Do not add one.
2. **Confirm by default.** Replacing the binary prints `<current> → <target>` and asks
   `Upgrade? [y/N]` before touching anything; `--yes`/`-y` skips the prompt.
3. **`--version vX.Y.Z` is supported** (accepts either `vX.Y.Z` or `X.Y.Z`, normalize by ensuring a
   leading `v` before using it as a tag) for pinning, downgrade, or reinstall/repair. Same
   download+verify+swap path as latest — only the resolved tag differs.
4. **No GitHub token support.** Unauthenticated GitHub API calls only (60 req/hour/IP is fine for a
   command a human runs occasionally). Do not add `GITHUB_TOKEN`/`GH_TOKEN` handling — that pattern
   exists elsewhere in this repo (`hack/krayt-dev/entrypoint.sh`) for a *guest-image* `gh` CLI
   use case and is unrelated; do not port it here.

---

## 1. `internal/selfupdate` — core logic (new package, host-side, no build tag)

Pure standard library — `net/http`, `encoding/json`, `crypto/sha256`, `archive/tar`,
`compress/gzip`, `bufio`, `io`, `os`, `path/filepath`, `runtime`, `context`, `time`. **No new
dependency** (CLAUDE.md §9.1: use pinned deps exactly, don't guess new libraries — none is needed
here).

```go
package selfupdate

const githubRepo = "418-cloud/krayt" // owner/repo, single source of truth for API paths

// apiBaseURL is overridable in tests to point at an httptest.Server instead of the real GitHub API.
var apiBaseURL = "https://api.github.com"

// Release is the subset of the GitHub Releases API response this package needs.
type Release struct {
	TagName string  `json:"tag_name"` // e.g. "v0.6.1" — always "v"-prefixed
	Assets  []Asset `json:"assets"`
}

type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}
```

**Functions to implement:**

- `LatestRelease(ctx context.Context, client *http.Client) (Release, error)` — `GET
  {apiBaseURL}/repos/{githubRepo}/releases/latest`, 15s request timeout
  (`context.WithTimeout(ctx, 15*time.Second)`, wrapping the passed-in `ctx`, not replacing it).
- `ReleaseByTag(ctx context.Context, client *http.Client, tag string) (Release, error)` — `GET
  {apiBaseURL}/repos/{githubRepo}/releases/tags/{tag}`, same timeout. A 404 must produce a clear
  error like `release <tag> not found` (check the HTTP status, don't just fail JSON-decoding a
  GitHub error body).
- Both of the above **must** set a `User-Agent` header (GitHub's API returns `403` without one —
  use e.g. `"krayt-upgrade/"+cliVersion`, passed in) and `Accept: application/vnd.github+json`. Take
  `client *http.Client` as a parameter in every exported function — never reach for
  `http.DefaultClient` implicitly — so tests can inject one bound to an `httptest.Server`.
- `AssetName(goos, goarch, tag string) (string, error)` — returns
  `fmt.Sprintf("krayt_%s_%s_%s.tar.gz", tag, goos, goarch)` **only** for the three published
  combinations (`darwin/arm64`, `darwin/amd64`, `linux/amd64`); any other combination (notably
  `linux/arm64`, and anything non-darwin/non-linux) returns an error naming the unsupported
  combination and pointing at `README.md`'s explanation. Keep the three-combination list as an
  explicit `switch`/map literal with a comment cross-referencing
  `.github/workflows/release-please.yml`'s build matrix, since the two can't share code across
  YAML/Go and must be kept in sync by hand.
- `FindAsset(rel Release, name string) (Asset, error)` — look up by exact `Name` match; error if
  absent (e.g. a malformed/partial release missing an expected asset).
- `ParseChecksums(data []byte) (map[string]string, error)` — parse GNU `sha256sum` text-mode
  output: each non-empty line is `<64-hex-char digest>` + whitespace + filename; tolerate an
  optional leading `*` on the filename (binary-mode marker) by stripping it, but the real generated
  file never has one. Return `map[filename]lowercase-hex-digest`. Malformed line → error identifying
  the line.
- `DownloadAndVerify(ctx context.Context, client *http.Client, url, wantSHA256Hex, destDir string)
  (tmpPath string, err error)` — `context.WithTimeout(ctx, 2*time.Minute)`; create a temp file
  **inside `destDir`** (`os.CreateTemp(destDir, ".krayt-upgrade-*.tmp")` — same directory as the
  eventual install target, so the later rename is same-filesystem/atomic); stream the response body
  through `io.TeeReader` into both the file and a `sha256.New()` hasher; on mismatch, `os.Remove`
  the temp file and return an error showing both digests (never leave an unverified temp file
  behind on failure). On success, return the temp file's path with the verified bytes.
- `ExtractBinary(tarGzPath, destDir string) (tmpBinaryPath string, err error)` — open + gunzip +
  untar; the archive has exactly one entry, a regular file (see `release-please.yml:69` — the
  workflow always tars a single file named `krayt`); write it to a new temp file in `destDir`
  (`os.CreateTemp(destDir, ".krayt-upgrade-bin-*.tmp")`), `chmod 0755`. Error if the archive has
  zero entries, more than one regular-file entry, or the one entry isn't a regular file — fail
  closed on anything unexpected rather than guessing which entry to use.
- `CompareVersions(a, b string) (int, error)` — strip an optional leading `v` from each, split on
  `.`, parse each of exactly 3 components as an integer, compare numerically component-by-component
  (return `-1`/`0`/`1`). **Must not be a string compare** — `"0.9.0"` vs `"0.10.0"` compares wrong
  lexically (`'1' < '9'`) but must compare correctly numerically (`0.9.0 < 0.10.0`). Malformed input
  (wrong number of components, non-numeric component) → error.
- `Apply(currentPath, newBinaryPath string) (backupPath string, err error)`:
  1. `dir := filepath.Dir(currentPath)`; verify it's writable by attempting to create+remove a probe
     temp file in it (`os.CreateTemp` + `os.Remove`) — if that fails, return an actionable error
     (`fmt.Errorf("%s is not writable by the current user — re-run with sufficient permissions, or
     reinstall krayt to a user-writable location: %w", dir, err)`), **do not** attempt any privilege
     escalation (no shelling out to `sudo`).
  2. `backupPath = currentPath + ".bak"`; copy the existing `currentPath` there (best-effort content
     copy, then `chmod` to match; overwrite any prior `.bak`). If this copy fails, abort before
     touching `currentPath` — a failed backup must never be silently skipped.
  3. `os.Rename(newBinaryPath, currentPath)` — atomic same-filesystem replace. (Safe even though
     `currentPath` may be the currently-running process's own executable: on Unix, replacing/renaming
     over an open, executing file is well-defined — the running process keeps its already-mapped
     inode until it exits.)
  4. Return `backupPath, nil`.
- `ResolveCurrentExecutable() (string, error)` — `os.Executable()` then `filepath.EvalSymlinks(...)`
  on the result, so a symlinked install (e.g. `/usr/local/bin/krayt -> /opt/krayt/bin/krayt`) gets
  its **real target** replaced, leaving the symlink itself intact. Expose this as its own function
  (not inlined in the CLI command) so `internal/cli/upgrade.go` can hold it behind an overridable
  func var for tests (§3).

## 2. Version-comparison + asset-selection glue

In `internal/selfupdate`, add one more small helper the CLI layer calls directly:

- `TargetAssetName(tag string) (string, error)` — thin wrapper: `AssetName(runtime.GOOS,
  runtime.GOARCH, tag)`. Keeps `runtime.GOOS`/`GOARCH` out of the testable `AssetName` signature
  (table-test `AssetName` directly with explicit goos/goarch instead).

## 3. `internal/cli/upgrade.go` — the `krayt upgrade` command

```
krayt upgrade [--version vX.Y.Z] [--yes|-y] [--check]
```

Flags:
- `--version string` — target a specific release instead of latest. Accepts `vX.Y.Z` or `X.Y.Z`
  (normalize by adding a leading `v` if missing before calling `ReleaseByTag`).
- `--yes` / `-y bool` — skip the confirmation prompt.
- `--check bool` — report status only; never downloads, verifies, prompts, or writes anything.

**Testability seams** (package-level func vars in `upgrade.go`, overridden in tests — mirrors this
repo's existing pattern of keeping OS/filesystem access behind swappable seams, e.g. the
`doctor_darwin.go`/`doctor_linux.go`/`doctor_other.go` split for `hostChecks`):

```go
var execPath = selfupdate.ResolveCurrentExecutable
var httpClient = &http.Client{} // zero-value: uses http.DefaultTransport, honors HTTP_PROXY/HTTPS_PROXY/NO_PROXY
```

Tests must never let a real `krayt upgrade` run touch the actual `go test` binary — always override
`execPath` to point at a temp file standing in for "the installed krayt".

**`RunE` logic, in order:**

1. Resolve the target release:
   - If `--version` set: normalize, `selfupdate.ReleaseByTag(cmd.Context(), httpClient, tag)`.
   - Else: `selfupdate.LatestRelease(cmd.Context(), httpClient)`.
   - Any error (network, 404, rate-limited 403, etc.) → return it directly (cobra's
     `SilenceErrors`/`SilenceUsage`, already set at root, means this just prints the error and
     exits non-zero — no special formatting needed).
2. `targetVersion := strings.TrimPrefix(rel.TagName, "v")`; compare to `cli.Version` via
   `selfupdate.CompareVersions(cli.Version, targetVersion)`.
3. **`--check`**: print one line reporting current version, target version, and whether it's
   up to date / an upgrade / a downgrade (based on the comparison sign) — then `return nil`
   immediately. No further steps run.
4. If **no `--version` was given** and the comparison shows equal versions: print `krayt is already
   at the latest version (v<Version>).` and `return nil` — no prompt, no download. (`--version`
   pinned to the *current* version is allowed to proceed past this point deliberately — it's a
   supported reinstall/repair path, not blocked as a no-op.)
5. Resolve the asset name: `selfupdate.TargetAssetName(rel.TagName)` — if this errors (unsupported
   platform), return that error immediately, before any network call for the tarball itself.
6. Resolve the install path: `path, err := execPath()`. Compute `dir := filepath.Dir(path)`.
7. Print `krayt <cli.Version> → <targetVersion>` (say "downgrade" in the message when the comparison
   sign is negative, "upgrade" otherwise) and the resolved install path.
8. **Confirmation**, unless `--yes`:
   - Print `Upgrade? [y/N] ` to `cmd.OutOrStdout()` (no separate stderr stream — this repo's CLI
     commands write everything through `cmd.OutOrStdout()`, e.g. `version.go`).
   - Read one line from `cmd.InOrStdin()` via `bufio.NewReader(...).ReadString('\n')`.
   - **On `io.EOF` with no bytes read at all** (empty/closed stdin — the non-interactive/CI case):
     do not hang, do not treat as yes. Print a message telling the user to pass `--yes` in
     non-interactive contexts, and `return nil` (declined, not an error).
   - Trim + lowercase the input; only `"y"` or `"yes"` proceeds. Anything else (including a plain
     Enter) → print `Aborted.` and `return nil`.
9. Find the `checksums.txt` asset via `selfupdate.FindAsset`, download its bytes (small — read the
   whole body with a 15s-timeout `http.Client.Do`, no need to route this one through
   `DownloadAndVerify`, which is sized for the multi-MB tarball), parse with
   `selfupdate.ParseChecksums`.
10. Find the target tarball asset via `selfupdate.FindAsset`; look up its expected digest in the
    parsed checksums map — missing entry is an error (`fmt.Errorf("checksums.txt has no entry for
    %s", assetName)`), fail closed rather than skipping verification.
11. `selfupdate.DownloadAndVerify(cmd.Context(), httpClient, asset.BrowserDownloadURL, wantSHA256,
    dir)` → tarball temp path. `defer os.Remove` it regardless of outcome past this point.
12. `selfupdate.ExtractBinary(tarballTmpPath, dir)` → binary temp path. `defer os.Remove` it too
    (the deferred removes are no-ops once `Apply` successfully renames it away).
13. `backupPath, err := selfupdate.Apply(path, binaryTmpPath)`. On error, return it directly — no
    partial state should exist (per `Apply`'s own ordering: writability check, then backup, then
    rename).
14. On success, print: the new version now installed, the `backupPath` for manual rollback
    (`cp <backupPath> <path>` if needed), and then run **the newly-installed binary** as a
    subprocess (`exec.CommandContext(cmd.Context(), path, "version")`, output piped to
    `cmd.OutOrStdout()`) to show real, non-fabricated confirmation of what's now on disk — do not
    just print the expected version string; actually invoke it. If that subprocess call itself
    fails, report it as a warning (the swap already succeeded; don't turn this into a command
    failure) rather than an error return.

Register in `internal/cli/root.go`: add `root.AddCommand(newUpgradeCmd())` after
`root.AddCommand(newVersionCmd())` (`root.go:33`), keeping the file's existing one-command-per-line
style.

## Tests

### `internal/selfupdate/selfupdate_test.go` — fully offline, `httptest.Server`-backed

Build a real small `.tar.gz` fixture in-test (a single file named `krayt` containing arbitrary
bytes, e.g. `"fake-krayt-binary-contents"`), compute its real SHA-256, and serve everything from one
`httptest.NewServer`:

- `/repos/418-cloud/krayt/releases/latest` and `/repos/418-cloud/krayt/releases/tags/<tag>` return
  a `Release` JSON whose asset `browser_download_url`s point back at the same test server (e.g.
  `srv.URL+"/download/"+name`).
- `/download/checksums.txt` serves the real GNU-format checksums line(s) for the fixture tarball(s).
- `/download/<tarball-name>` serves the fixture tarball bytes.
- Point `apiBaseURL` at `srv.URL` for the duration of each test (save/restore the package var, or
  add an unexported setter — either is fine, just don't leak the override across tests).

Cases:
1. `TestLatestRelease` / `TestReleaseByTag` — happy path parses correctly; unknown tag → 404 →
   error containing the tag name.
2. `TestAssetName` — table test: the three supported combos produce the exact expected string;
   `linux/arm64`, `windows/amd64`, and one made-up combo all error.
3. `TestParseChecksums` — parse the literal real-world fixture captured during this task's design
   (`28b4aacb...  krayt_v0.6.1_darwin_amd64.tar.gz` — two spaces, no asterisk) plus a synthetic
   multi-line file; missing-filename lookup on the resulting map is a normal Go map miss (no need
   for a special error path there — that's the CLI layer's job at step 10 above, not this parser's).
   One malformed-line case → error.
4. `TestDownloadAndVerify` — success case: digest matches, returned temp file's content equals the
   fixture bytes, file lives inside the passed `destDir`. Mismatch case: flip one byte of the
   expected digest string; assert an error and assert `destDir` ends up empty (no leftover temp
   file).
5. `TestExtractBinary` — round-trips the fixture tarball; asserts extracted content matches and mode
   is `0755`. Separately: a tarball with zero entries, and one with two entries, both error.
6. `TestCompareVersions` — `("0.9.0","0.10.0")` → `-1`; `("0.10.0","0.9.0")` → `1`; equal → `0`;
   leading-`v` on either/both input still works; malformed (`"1.2"`, `"1.2.x"`) → error.
7. `TestApply`:
   - Happy path: temp dir with a fake "current" file and a separately-created "new" file; after
     `Apply`, the current path's content equals the new content, mode is `0755`, and `<path>.bak`
     contains the original content.
   - Non-writable dir: `chmod 0500` the temp dir (skip the assertion with `t.Skip` if running as
     root, where permission bits don't block writes — check `os.Geteuid() == 0` the same way any
     other permission-dependent test in this repo would need to, since CI sometimes runs as root);
     assert an error mentioning the path, and assert the original "current" file content is
     unchanged.

### `internal/cli/upgrade_test.go` — offline, same `httptest` approach, plus CLI seams

Use the `execPath`/`httpClient` package vars (§3) to point at a temp "installed" file and the
`httptest.Server` from the pattern above (a small local helper building the fixture release/tarball
server is reasonable to share between the two test files, or duplicate it — whichever keeps each
file self-contained is fine, this is a small fixture).

Cases:
- Already up to date (fixture release's tag equals a fixed `cli.Version` used for the test run) →
  no prompt (no stdin needed at all — pass an empty `bytes.Buffer` for stdin and assert the command
  doesn't block), no changes to the fake "current" file, output mentions "already at the latest".
- Confirmation declined: stdin `"n\n"`, no `--yes` → fake "current" file content unchanged, command
  returns no error.
- Confirmation accepted: stdin `"y\n"` → fake "current" file now has the fixture tarball's binary
  content, `.bak` has the original content.
- `--yes` skips the prompt entirely: empty stdin, `--yes` set → still upgrades.
- `--check`: assert the fake "current" file is byte-for-byte unchanged after the call, regardless of
  stdin content or absence of `--yes`.
- `--version` targeting an older fixture tag than the fixture "current" version → message says
  "downgrade", and (with `--yes`) actually installs that older content.
- Non-interactive/empty stdin without `--yes` and without `--check` → declines safely (matches
  §3 step 8's EOF handling), no error, no changes, output hints at `--yes`.
- Checksum mismatch (serve a `checksums.txt` with a deliberately wrong digest for the target
  tarball) → command returns an error, fake "current" file unchanged.

## Docs (required)

- `README.md`: right after the existing "Prebuilt binaries" paragraph (`README.md:32-40`, which
  documents the manual download + `checksums.txt` verification — **keep that paragraph**, it's
  still the correct bootstrap path for a first install), add a short paragraph: `krayt upgrade`
  updates an already-installed krayt in place — same verification (`checksums.txt`) automated, plus
  an atomic swap and a `.bak` of the previous binary. Mention `--check` (report only) and
  `--version vX.Y.Z` (pin/downgrade/reinstall).
- `KRAYT_SPEC.md` §13: add `krayt upgrade [--version] [--yes] [--check]` to the fenced command
  block (`KRAYT_SPEC.md:1552-1574`), plus one short prose sentence after the block (near the
  existing `run`/`--task` explanatory paragraphs, `:1576-1595`) noting it re-verifies against the
  release's `checksums.txt` and never touches any other command's behavior (on-demand only, per the
  decision above).
- `docs/ai-tasks/README.md`: add a row to the top table for this task once implemented, following
  the existing row format (Task / What it does / Status), status `✅ Done` with a one-line summary
  of what shipped and how it was verified.

## Verify (offline)

```sh
go build ./...
GOOS=linux GOARCH=arm64 go build ./...
go test -race ./...
golangci-lint run
```

No new dependency. Every test above runs fully offline against `httptest.Server` fixtures — no real
GitHub network call in the test suite.

**Manual smoke test (real GitHub network + a real installed binary — do not fabricate a result; log
it in `HUMAN_TODO.md` per CLAUDE.md's handoff protocol instead):** install a real older `krayt`
release binary somewhere writable, run the real `krayt upgrade` against the real
`418-cloud/krayt` GitHub repo, confirm the confirmation prompt shows the correct old→new versions,
accept it, and confirm `krayt version` afterward reports the new version and that `<path>.bak`
contains the old binary. Also smoke-test `krayt upgrade --check` (no changes) and `krayt upgrade
--version <an-older-tag>` (downgrade path) for real. This needs a real filesystem the agent is
comfortable mutating and outbound network to `api.github.com`/`github.com` — reasonable to attempt
directly if the sandbox has both and a disposable install location; otherwise hand off honestly.

## Done when

- `krayt upgrade` (no flags) checks the latest release, prompts, and on confirmation downloads,
  checksum-verifies, and atomically replaces the resolved current binary, leaving a `.bak`.
- `--yes` skips the prompt; `--check` never mutates anything; `--version vX.Y.Z` targets a specific
  release (including downgrade) instead of latest.
- Checksum verification is unconditional — no code path installs bytes that weren't verified against
  `checksums.txt` first.
- Non-interactive/empty stdin without `--yes` declines safely instead of hanging.
- Unsupported platform (e.g. `linux/arm64`) fails with a clear error before any tarball download is
  attempted.
- All of the above is unit-tested offline against `httptest` fixtures (no real network in
  `go test`); `go build`/`go test -race`/`golangci-lint run` pass for both the host and
  `linux/arm64` guest cross-compile target.
- `README.md` and `KRAYT_SPEC.md` §13 document the command; `docs/ai-tasks/README.md` gets its row;
  the real-network manual smoke test is logged in `HUMAN_TODO.md` (attempted for real if the
  sandbox allows it, honestly handed off if not — never fabricated).

## Constraints

- Host-side CLI only — this command runs as krayt-the-program on the developer's machine, not
  inside any krayt guest/VM. It has nothing to do with, and must not be routed through, krayt's own
  guest egress-allowlist model (§6.6) — that governs sandboxed *agent* traffic, not krayt's own
  process talking to GitHub.
- No new third-party dependency — stdlib only.
- Never attempt privilege escalation (no `sudo` shell-out) — an unwritable install path is a clear
  error, not something this command tries to work around.
- Never install bytes that haven't passed checksum verification against `checksums.txt`.
- Do not add passive/background update checks anywhere else in the CLI, and do not add
  `GITHUB_TOKEN`/`GH_TOKEN` support — both explicitly out of scope per the decisions above.
- Keep the three-target platform list (`darwin/arm64`, `darwin/amd64`, `linux/amd64`) as the single
  source of truth in `selfupdate.AssetName`, cross-referenced by comment to
  `.github/workflows/release-please.yml`'s build matrix — if that workflow ever adds/removes a
  target, this is the other place that needs the matching edit (call this out, don't silently drift).
