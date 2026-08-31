# Task: dial the `ask_human` channel over `AF_VSOCK`, with no guest daemon

**Read `CLAUDE.md`, `docs/adr-microsandbox-sandbox-layer.md` ("The guest helper" → "`ask_human` must
not go through it"), and `KRAYT_SPEC.md` §6.13, §6.12, §8.2 first.** Give a short plan (where the
bridge moves, the dial-address form, how `krayt-ask` ships) and proceed. Depends on
`add-msb-sandbox-driver.md` and `add-krayt-guest-helper.md` (it extends that task's `guestbin`
package).

**Unblocked 2026-08-29 — `probe-microsandbox-feasibility.md` P1 passed on msb 0.6.16.** A guest
process running as `agent` (uid 1000) opened `AF_VSOCK` to host CID 2 and completed a
write-then-read-back round trip, in the real `ghcr.io/418-cloud/krayt-agent-claude-code` image,
unmodified (`hack/msb-probes/p1-vsock-nonroot.sh`). **This design works: `krayt-ask` dials the host
directly.** The fallback the task was told to stop and ask about — a root-owned in-guest forwarder,
which is the guest daemon B1 exists to delete — is now dead. Do not build it, and do not add a
privilege-dropping wrapper "just in case": a direct dial as `agent` is the verified path.

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
8. **The bridge parses hostile input on the *host* side now — bound it there.** This is the one
   thing decision 5's "a move plus a `net.Listener`, not a rewrite" framing gets wrong, so it is
   settled here rather than left to the implementer's judgement. Today the decoder runs *inside
   the VM* (`internal/guest/ask/ask.go:128`) and the host sees only a `RunEvent.Question` over
   gRPC, where `grpc.NewClient`'s default 4 MiB receive cap (`internal/controlclient/client.go:38`
   sets no `MaxCallRecvMsgSize` override) and generated protobuf code stand between the sandbox
   and the host process. Both disappear the moment the sandbox talks straight to `encoding/json`
   in krayt, and §10's "resource exhaustion — bounded by per-VM CPU/mem/disk" does not bound a
   host-side decoder. Three bounds, all in `Serve`/`handleConn`:
   - `io.LimitReader(conn, maxAskRequestBytes)`, a named constant (64 KiB is generous for a
     question) — the same idiom as `maxCACertPEMBytes` in
     `internal/orchestrator/egressproxy.go:200`. Over-long input is a refusal, not a truncation.
   - A **read** deadline around `Decode` only, cleared before `Bridge.Ask`. The ordering is
     non-negotiable: `Ask` legitimately blocks for `--question-timeout` (minutes), so a
     connection-wide `SetDeadline` would break the feature it is meant to protect.
   - A bound on in-flight questions, past which the answer is the no-answer sentinel rather than
     a queue slot. Unbounded `Bridge.pending` growth is not only memory: every `Ask` calls `push`,
     whose host end writes `questions/<id>.json` to disk (`internal/orchestrator/questions.go`),
     moves the run to `waiting`, and fires a desktop notification (§6.13) — all sandbox-driven.
9. **The `--vsock` route is created only under `--on-question=wait`.** `fail` is the default, and
   today it means the channel does not exist at all: the adapter sets `KRAYT_ASK_SOCKET` only in
   `wait` mode (`internal/adapter/adapter.go:98`, §14 Phase 5). Emitting the route unconditionally
   would hand every unattended default run a guest→host channel it does not have today, for no
   benefit — the inverse of §6.13's "off by default, so batch stays batch" and of design principle
   4's default-deny. Exactly one route per sandbox, and only in `wait`.
10. **The host socket is private to the invoking user, and the guest's `chmod 0777` does not come
    with it.** The code being moved sits two lines from `os.Chmod(askSocket, 0o777)`
    (`internal/guest/service.go:355`) — correct in-guest, where a non-root container has to connect
    to it, and a host exposure if carried across. Bind it `0600` inside decision 4's `0700`
    directory. **Verify which uid actually connects:** if msb's local backend opens the host socket
    as the invoking user this is free; if it opens it as root or a system daemon under another uid
    the route fails, and the tempting fix is exactly the `0777` above. Extend
    `hack/msb-probes/p1-vsock-nonroot.sh` to bind its listener inside a `0700` dir and record the
    answer rather than guessing it.
11. **Keep the secret redaction — move it host-side.** `internal/guest/service.go:317-327` redacts
    secret values out of the agent-authored prompt and choices before they leave the VM, for the
    reason its own comment gives: the host persists what it receives, so redaction belongs at the
    boundary, not on display. This task moves that boundary; the control moves with it rather than
    evaporating into "the package move is unchanged". B1's "secrets never enter the guest" is the
    general case, not a guarantee — `network.passthrough`, an env var in `krayt.yaml`, and a value
    the agent fetched from an allowed host each put one back in the sandbox. The host holds the
    values anyway, so applying the `Redactor` in `Serve` before `push` costs nothing.
12. **`ensureSockRoot` is extracted, not copied.** Decision 4's "reuse that check" is right and, as
    written, impossible: the check is duplicated in each OS-specific provider package —
    `internal/provider/vfkit/vfkit.go:185` behind `//go:build darwin` and
    `internal/provider/firecracker/firecracker.go:203` behind `//go:build linux` — and
    `internal/sandbox` is OS-agnostic and runs on both. Move it to a shared unix-tagged package,
    preserving the two properties that make it work — `os.Mkdir` (not
    `MkdirAll`, so an existing path is an error) and `os.Lstat` (so a pre-placed symlink is never
    followed into an attacker's target). Do not inherit vfkit's `/tmp/krayt-<uid>` root: the run's
    own private state dir is correct here. vfkit's fixed `/tmp` path exists only for macOS's
    104-byte `sun_path` limit (`vfkit.go:145`) — a limit this socket still has to fit under, so keep the
    path short by construction rather than "fixing" an overflow later by moving to a shared
    world-writable dir. A socket already present in a freshly created per-run dir means something
    is wrong: fail closed, never unlink-then-bind.

## What to build

- `internal/askbridge/` — the moved package, plus:
  ```go
  // Serve accepts connections on lis and answers each with one question exchange
  // against the Bridge. One exchange per connection; it never keeps state between them.
  func Serve(ctx context.Context, lis net.Listener, b *Bridge) error
  ```
  The host listener is a plain `net.Listen("unix", path)` — msb owns the vsock side. `Serve` is
  where decision 8's three bounds live; it is the only code in krayt that reads bytes the sandbox
  wrote, in the host process.
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
  description stays true until cut-over. **Also §6.8 and §10**: the redaction point moves host-side
  (decision 11), and §10's table gains the ask bridge as host-side attack surface. Its host-egress
  residual currently argues that the whole adversarial-input surface concentrates in one host
  process that "parses nothing it does not have to" — this task adds a second parser to the host
  that the paragraph does not know about yet, bounded by decision 8.

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
- Decision 8's bounds are tested without a VM: a request over `maxAskRequestBytes` is refused
  rather than allocated; a connection that opens and never writes is dropped on the read deadline
  **while** an accepted question still survives a wait longer than that deadline (the ordering is
  the point); and an in-flight question past the bound gets the no-answer sentinel.
- `CreateSpec.Args()` emits no `--vsock` flag for an `--on-question=fail` run and exactly one for a
  `wait` run (decision 9).
- The host socket is created `0600` inside the `0700` directory, asserted by a test that stats it
  (decision 10).
- A prompt carrying a secret value comes back redacted from the host-side `Serve` path — the
  guest-side test covering this today has a host-side equivalent (decision 11).
- The port constant is distinct from `provider.ControlPort`/`EgressPort` (1024/1025), not merely
  valid: both paths exist during the additive period, and two channels sharing a number invite the
  wrong one being reasoned about.

## Residuals to record, not solve here

Both belong in §10 as stated properties. Neither is new work; leaving them unstated is what makes
them look like oversights later.

- **Any process in the sandbox can dial the socket** — it is unauthenticated by construction, as
  today's in-guest socket already is. A hostile process can ask a plausible question and collect
  the human's answer meant for the agent. It does not reach the host, and the controls are
  unchanged (§6.13: label the prompt agent-originated, never auto-fill secrets into an answer) —
  say so rather than leaving it inferred from the absence of a claim.
- **Cross-run isolation is per-sandbox by construction** — vsock maps guest CID 2:port to the host
  path declared on *that* sandbox's `create`, so one fixed port constant is safe across concurrent
  runs. §10 makes exactly this claim for the vfkit/Firecracker egress channel and proves it on
  hardware (`TestConcurrentRealVMs`); a two-sandbox extension of P1 buys the same claim for msb
  for the cost of one more `msb create`.

## Out of scope

- Deleting `cmd/krayt-vsock-forward` or `internal/guest` — cut-over.
- Rebuilding any agent image. `krayt-ask` is copied in, not baked in (see Background); if you find
  yourself editing a Dockerfile, re-read that paragraph.
- Changing the question/answer semantics, the `--on-question` modes, the timeout behaviour or the
  MCP front-end.
