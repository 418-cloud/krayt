# Task: harden the vfkit socket root (`/tmp/krayt`) against a hostile pre-created dir

**Read `CLAUDE.md` and `KRAYT_SPEC.md` (§6.3 provider, §6.12 vsock transport, §12 macOS specifics)
first. Proceed autonomously. This is a `//go:build darwin` provider change; full verification needs a
Mac — write the test and hand off via `HUMAN_TODO.md` (§14) for the runtime part.**

## Reason (the finding)

**Finding (Low) — predictable, unverified socket root.** The vfkit provider puts each VM's control +
REST sockets under a fixed path `sockRoot = "/tmp/krayt"`
(`internal/provider/vfkit/vfkit.go:144-156`), created with `os.MkdirAll(sockRoot, 0o700)`. `MkdirAll`
is a **no-op if the directory already exists** and does **not** reset its owner or mode. On a shared
macOS host, another local user could pre-create `/tmp/krayt` (world-writable, or owned by them) before
krayt runs; krayt would then create its per-VM `MkdirTemp("vm-…")` dirs inside an attacker-controlled
parent. The vfkit **REST control socket** (`rest.sock`) commands the VM lifecycle (stop/kill), and the
vsock **control socket** (`control.sock`) is the guest control channel — the directory guarding them
must be trustworthy. The per-VM `MkdirTemp` dir is itself `0700`, which bounds the exposure, but the
parent is unchecked.

The fixed `/tmp` path exists for a real reason (macOS `sockaddr_un.sun_path` is capped at 104 bytes
and `$TMPDIR` is too long — `vfkit.go:141-144`), so the fix is to **verify/repair** the root, not to
move it arbitrarily.

## Goal

Ensure the socket root krayt uses is a directory **owned by the current uid with mode `0700`**, or
fail fast with a clear error — so krayt never places control sockets under a directory another user
controls.

## Current behavior (grounding)

- `internal/provider/vfkit/vfkit.go:144` — `const sockRoot = "/tmp/krayt"`.
- `:147-156` — `newSockDir()`: `os.MkdirAll(sockRoot, 0o700)` then `os.MkdirTemp(sockRoot, "vm-")`.
- `:107-108` — `ctrlSock`/`restSock` live in that per-VM dir; `:181` binds the vsock to `ctrlSock`;
  `Start` (`:237`) passes `--restful-uri unix://<restSock>` to vfkit.

## Implement (`internal/provider/vfkit/vfkit.go`, darwin)

Replace `newSockDir`'s `MkdirAll` with a **verify-or-create** step:

1. `os.Lstat(sockRoot)`:
   - **Not exist** → `os.Mkdir(sockRoot, 0o700)` (not `MkdirAll`, so a symlink at the path is not
     silently followed into an attacker target; `Mkdir` fails if it already exists).
   - **Exists** → require **all** of: it is a directory (not a symlink/other), `sys.Uid == os.Getuid()`,
     and `mode.Perm() == 0o700`. Get owner/mode via `fi.Sys().(*syscall.Stat_t)`. If any check fails,
     return a clear error:
     `"vfkit: socket root %s is not a private directory owned by this user (mode %o, uid %d); refusing to place VM control sockets there — remove or fix it"`.
   - Do **not** auto-`chmod`/`chown` someone else's directory; fail closed and let the human fix it.
2. Keep the per-VM `os.MkdirTemp(sockRoot, "vm-")` as-is (it is already `0700` and atomic).
3. Consider using a per-user root to avoid cross-user collisions while staying short, e.g.
   `"/tmp/krayt-" + strconv.Itoa(os.Getuid())` — still well under the 104-byte limit and not shared.
   If you change the constant, update the comment explaining the length constraint and the per-user
   choice, and check no other code references `/tmp/krayt` (grep).

## Tests

Unit (`//go:build darwin`, host-runnable — no VM needed for the dir logic):
- Fresh root: `newSockDir` creates a `0700` dir owned by the current uid and a `vm-…` subdir.
- **Hostile pre-existing root:** create the root `0777` (or simulate a wrong mode) and assert
  `newSockDir` returns the refusal error rather than proceeding.
- Symlink at the root path → refused (not followed).
- Idempotent good case: an already-correct `0700` self-owned root is accepted.

(The socket bind/dial themselves need a real vfkit VM — cover those in the existing integration test
and note in `HUMAN_TODO.md`.)

## Docs (required)

- `KRAYT_SPEC.md` §6.12 (or §12 macOS specifics): document that the provider uses a short, private
  (`0700`, self-owned) socket root under `/tmp` for control/REST sockets, verified on each run, and
  fails closed if the directory is not trustworthy.
- `docs/ai-tasks/README.md`: add this task.

## Verify (offline)

```sh
go build ./...
GOOS=darwin GOARCH=arm64 go build ./...   # the changed file is darwin-only
go test -race ./internal/provider/vfkit/...
golangci-lint run
```

## Done when

- The socket root is verified/created safely and rejects an untrusted pre-existing directory; unit
  tests pass; spec updated.

## Constraints

- `//go:build darwin` only; do not affect the OS-agnostic core or the linux guest.
- Keep socket paths under the 104-byte `sun_path` limit (do not move under `$TMPDIR`).
