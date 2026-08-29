# Task: dial the `ask_human` channel over `AF_VSOCK`, with no guest daemon

**Read `CLAUDE.md`, `docs/adr-microsandbox-sandbox-layer.md` ("The guest helper" → "`ask_human` must
not go through it"), and `KRAYT_SPEC.md` §6.13, §6.12, §8.2 first.** Give a short plan (where the
bridge moves, the dial-address form, how `krayt-ask` ships) and proceed. Depends on
`add-msb-sandbox-driver.md` and `add-krayt-guest-helper.md` (it extends that task's `guestbin`
package).

**Blocked on `probe-microsandbox-feasibility.md` P1**: whether a *non-root* guest process can open
`AF_VSOCK` under msb. The agent runs as `agent` (uid 1000). If P1 fails, this design does not work
and the alternative — a root-owned in-guest forwarder — is the guest daemon B1 exists to delete, so
it needs a fresh decision rather than an improvised workaround. Build everything that does not
depend on the answer; stop and ask before implementing a fallback.

## Sequencing — additive only

`cmd/krayt-vsock-forward` and `internal/guest/ask`'s in-guest half are still live. Add the new path
beside them; `run-tasks-on-microsandbox.md` switches over and deletes both.

## Background

Today the question channel is four hops: `krayt-ask` (or its `--mcp` front-end) inside the container
→ a unix socket at `/run/krayt/ask.sock` → the guest agent's `ask.Bridge` → a
`RunEvent.Question` on the gRPC Start stream → the host. B1 deletes the guest agent and the gRPC
protocol, so hops two and three have to go somewhere.

They go to **nowhere**, which is the point. msb's `--vsock HOST_PATH:PORT` exposes a host unix socket
at guest CID 2 on `PORT` (`docs/networking/host-sockets.mdx`), so `krayt-ask` can dial the host
*directly* and krayt can serve the existing wire protocol host-side. No guest daemon, no listener
inside the sandbox, and it additionally retires `cmd/krayt-vsock-forward` (368 LOC).

Two facts make this much cheaper than it looks:

- **The wire protocol already exists and does not change.** `internal/guest/ask/ask.go` defines a
  newline-delimited JSON request/response — `{"prompt","choices"}` in, `{"response","no_answer"}`
  out, one exchange per connection. Only the transport underneath it moves.
- **`krayt-ask` is already not baked into the agent images.** The Claude Code image's entrypoint says
  so in as many words: "krayt-ask itself is bind-mounted by the guest onto /usr/local/bin/krayt-ask
  — never baked into the image" (`images/agents/claude-code/entrypoint.sh:113`). Under msb it becomes
  a second `msb copy`, exactly like the helper. **So no agent image needs rebuilding for this task.**

## Decisions already made (do not re-litigate)

1. **No listener inside the guest, ever.** Not in the helper (`add-krayt-guest-helper.md` decision 1),
   not as a separate process. `krayt-ask` is a short-lived client that dials out and exits.
2. **`KRAYT_ASK_SOCKET` keeps its name and gains a URL form.** Accept `vsock://<cid>:<port>`
   alongside today's bare filesystem path. Keeping the variable name means the adapters
   (`internal/adapter/adapter.go`'s `askEnv`), the MCP registration block in every agent image's
   entrypoint, and §8.2's container contract all keep working untouched — the value changes, the
   contract does not. A bare path still means a unix socket, so nothing that exists today breaks.
3. **CID 2 is the host, per msb's contract** — do not make it configurable. The port is krayt's to
   choose per run; pick it from the same range discipline `provider.ControlPort`/`EgressPort` used
   and name the constant in the same place, so ports stay enumerable in one file. Port `123` is
   reserved by msb and `0`/`u32::MAX` are invalid; assert the chosen port is none of those.
4. **The host socket lives in the run's own private state directory**, created `0700` and owned by
   the invoking user, and is removed on teardown. `harden-vfkit-socket-dir.md` established exactly
   this property for `/tmp/krayt` and it fails closed on a hostile pre-existing directory — reuse
   that check rather than writing a second one. msb's own docs warn that "a route gives sandbox
   processes access to whatever the host service allows"; krayt's service here answers exactly one
   question shape and nothing else, which is the mitigation, but the socket must still not be
   world-reachable.
5. **`internal/guest/ask` moves to `internal/askbridge`**, host-side. `internal/guest` is deleted at
   cut-over and this code has no business being under a package named for a thing that no longer
   exists. The `Bridge` type, the wire structs and the pending-question map are unchanged — this is
   a move plus a new `net.Listener` in front of it, not a rewrite. `internal/orchestrator/questions.go`,
   `RecordAnswer`, `krayt answer` and the `--on-question=wait` semantics of §6.13 are all untouched.
6. **`krayt-ask` joins `guestbin`.** It is built by the existing `make guest-bins` target for
   `linux/{amd64,arm64}`, embedded from the same gitignored directory,
   and `msb copy`'d to `/usr/local/bin/krayt-ask` per run. It is already `//go:build`-free and
   already builds for linux; the only change is the dialer.
7. **The `--mcp` front-end is unchanged.** It bridges to whatever `KRAYT_ASK_SOCKET` names; it does
   not need to know the transport.

## What to build

- `internal/askbridge/` — the moved package, plus:
  ```go
  // Serve accepts connections on lis and answers each with one question exchange
  // against the Bridge. One exchange per connection; it never keeps state between them.
  func Serve(ctx context.Context, lis net.Listener, b *Bridge) error
  ```
  The host listener is a plain `net.Listen("unix", path)` — msb owns the vsock side.
- `internal/guest/ask/ask.go`'s dialer half → `krayt-ask`'s side: teach `ask.OverSocket` (or a new
  `ask.Dial`) to parse `KRAYT_ASK_SOCKET` and return either a unix or a vsock connection. Use
  `github.com/mdlayher/vsock` — already pinned (§9.1), already used by `cmd/krayt-vsock-forward`.
  Note that this makes `cmd/krayt-ask` linux-only in practice; keep the parsing and the wire logic
  OS-agnostic and testable, and confine the vsock dial to a build-tagged file so
  `go build ./...` on darwin still covers everything else.
- `internal/sandbox`: the `--vsock` route on `CreateSpec`, and the copy of `krayt-ask` into the
  sandbox (both used by the cut-over, wired here only as functions).
- `KRAYT_SPEC.md` §6.13 and §8.2: the new transport, the `vsock://cid:port` form of
  `KRAYT_ASK_SOCKET`, and the fact that no guest process listens. Additive — the vsock-forward
  description stays true until cut-over.

## Done when

- `go build ./...` (both `GOOS`), `go test -race ./...` and `golangci-lint run` are green, with the
  existing question tests (`internal/orchestrator/question_test.go`, `internal/guest/ask/ask_test.go`,
  `cmd/krayt-ask/main_test.go`, `internal/cli/questions_test.go`) **still passing**.
- An offline test drives the full loop over a plain unix socket — `Serve` + `Bridge` + a real
  `krayt-ask` invocation via the re-exec'd-test-binary pattern — asserting the answer reaches
  stdout with exit 0, and that a no-answer sentinel exits 2 with empty stdout. The transport is the
  only thing vsock changes, so a unix-socket test covers the logic; do not require a VM.
- A test asserts `KRAYT_ASK_SOCKET` parsing: a bare path is unix, `vsock://2:5000` is vsock, and a
  malformed value is a usage error rather than a silent no-answer. A silent fallback here would turn
  a misconfiguration into "the agent quietly never asks", which is the failure mode §6.13 is
  designed to avoid.
- The chosen vsock port is a named constant, asserted not to be `123`, `0`, or `math.MaxUint32`.
- The host socket's parent directory check refuses a pre-existing hostile directory, reusing
  `harden-vfkit-socket-dir.md`'s existing check rather than a new one.

## Out of scope

- Deleting `cmd/krayt-vsock-forward` or `internal/guest` — cut-over.
- Rebuilding any agent image. `krayt-ask` is copied in, not baked in (see Background); if you find
  yourself editing a Dockerfile, re-read that paragraph.
- Changing the question/answer semantics, the `--on-question` modes, the timeout behaviour or the
  MCP front-end.
