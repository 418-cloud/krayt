# Task: add `krayt code` — an SSH remote-dev session VS Code can open, without ever publishing a port

**Read `CLAUDE.md`, then `KRAYT_SPEC.md` §3, §6.6, §6.15, §7 (including "Shell lifecycle —
departures from the run lifecycle"), §8.2, §8.4, §10, §13 and §14 Phase 12 first**, plus
`add-interactive-shell-session.md` (whose decisions this task reuses wholesale) and
`docs/adr-microsandbox-sandbox-layer.md`'s "Scope boundary" section. State a short plan and proceed.

Depends on `add-interactive-shell-session.md` having landed — it has. This task is modelled on
`krayt shell` throughout and should reuse its code, not re-derive it.

## Background

krayt has two ways into a sandbox: a headless agent run (`krayt run`) and an interactive terminal
(`krayt shell`). There is no way to point a real editor at one. This task adds `krayt code`: boot a
sandbox from the repo snapshot exactly as `krayt shell` does, then let VS Code Remote-SSH — and, for
free, Cursor, JetBrains Gateway, `ssh`, `scp` and `rsync` — open `/workspace` inside it, with the
session's edits coming back out through krayt's normal `changes.patch` review contract.

The obvious implementation is "publish port 22 and open ingress". **That is not available, and the
evidence below is why.** The implementation is `sshd -i` over `msb exec --stream` instead.

### Evidence (verified while writing this task; cite it, don't re-derive it)

There is no verified host→guest TCP path into an msb sandbox:

- **`--vsock HOST_PATH:PORT` is guest→host only**, and create-time only. It publishes a *host* unix
  socket into the guest's CID-2 namespace; the guest dials, the host listens
  (`internal/sandbox/msb.go:263-268`, `internal/sandbox/ports.go:5-8`). Nothing in the repo shows it
  can be inverted.
- **Ingress is denied in every network mode.** `internal/task/netpolicy_msb.go:54-62` emits
  `--net-default-ingress deny` for `full`; `allowlist` uses the bidirectional `--net-default deny`;
  `none` uses `--no-net`. The comment is explicit: *"closes msb's own default-allow ingress posture
  before krayt ever publishes a port."*
- **`sandbox.CreateSpec` has no port/publish field** and `Args()` emits none
  (`internal/sandbox/msb.go:321-436`). Spec `:307-310` and `:2282-2285` both record "krayt publishes
  no ports today" as a deliberate standing position.
- **msb's published-port and SSH features are named only as concepts.** The ADR lists them among
  what is "opt-in and off by default" (`docs/adr-microsandbox-sandbox-layer.md:29`, `:168-170`) and
  **never names a flag, a syntax, or a semantic.** There is no probe for either.
- **`extra_conf` cannot be used to relax this.** Probe p8 (PASS, 2026-09-05) measured that krayt's
  own `--net-default*` flags *fully replace* an `extra_conf`'s network policy rather than merging
  with it, and that msb's config schema uses `deny_unknown_fields` — an unrecognised key makes msb
  reject the whole sandbox.
- **A guest daemon is the one thing the architecture forbids.** `KRAYT_SPEC.md:806-812`: *"no
  listener inside the sandbox, ever."* `cmd/krayt-helper/main.go`'s doc comment says a helper that
  grows a listener means *"krayt has re-created the guest agent inside someone else's sandbox while
  keeping none of B1's benefit."*

**The resolution.** OpenSSH's `sshd -i` (inetd mode) serves exactly one connection on stdin/stdout
and exits with it. `ssh` reaches the sandbox through a `ProxyCommand` that runs krayt, which pipes
bytes through `sandbox.Client.Exec` (`msb exec --stream`). That needs no published port, no ingress
change, and no network at all — it works under `--net none`. And it is **not** the forbidden guest
daemon: `sshd -i` is exec'd per connection, argv-in, exits when the connection closes — the same
stateless shape as `krayt-helper`. `ExecSpec`'s own doc already requires stdin to be a real pipe
rather than a terminal, which is exactly what a ProxyCommand provides.

## Decisions already made (do not re-litigate)

1. **The command is `krayt code`**, registered in `internal/cli/root.go` alongside `shell`. Not
   `krayt vscode` — the mechanism is SSH and every SSH client benefits, so the name must not promise
   VS Code-specific behavior krayt does not have.
2. **Transport is `sshd -i` over `msb exec --stream`.** Never `ExecTTY` — `--stream` and `--tty` are
   mutually exclusive by msb's own clap config (`internal/sandbox/msb.go:576`), and this path needs
   the separated, byte-exact pipes that `--stream` gives.
3. **`sshd -i` runs as root** (`ExecSpec.User = "root"`), and the SSH login user is the sandbox's own
   resolved user (the image's `USER`, via `orchestrator.resolveSandboxUser`). krayt already execs as
   root for `krayt-helper setup` (§7 step 2), so this adds no new privilege. Root gives OpenSSH its
   normal privilege separation, a correct login shell and env, and a working sftp subsystem.
4. **sshd's stderr must never reach the SSH byte stream.** `ExecSpec` has separate `Stdout` and
   `Stderr`, and msb separates them end to end. `Stdout` carries the SSH protocol; `Stderr` (sshd is
   invoked with `-e`) goes to a log file in the run dir. Getting this wrong corrupts every
   connection — assert it in a test.
5. **The ProxyCommand entry point is `krayt code --stdio <run-id>`, a hidden flag**
   (`MarkHidden("stdio")`). It resolves the run id to a sandbox name from the run record and pipes
   `os.Stdin`/`os.Stdout` through `Exec`. It is not documented in the CLI surface as a user-facing
   verb; it exists for `ssh` to call.
6. **`krayt code` creates its own sandbox**, mirroring `internal/cli/shell.go`'s `runShellFresh` →
   `orchestrator.Shell` path. It reuses `runFlags`, `applyConfig`, `printNetworkPolicy`,
   `hostFreeResources`/`checkHostResources`, `AcquireSlot` and `newRunID` rather than re-deriving
   §8.3's config precedence. `--keep` behaves exactly as it does for `krayt shell`.
7. **No agent auto-starts.** `add-interactive-shell-session.md` decision 2 applies unchanged: no
   launcher, no `krayt-ask` wiring, no transcript. The image's agent CLI is on `PATH` and the human
   starts it in a VS Code terminal if they want it. As in `krayt shell`, `agent.adapter`'s
   secret-scoping and config-seed contribution is still resolved host-side
   (`applyAdapterForShell`), for the reasons §13's 2026-09-16 amendment gives.
8. **Patch out on session end**, via the existing `finishAndCollect` — `changes.patch` +
   `commits.bundle`, then `krayt apply <run-id>`. Same contract as every other krayt session.
9. **New run kind `KindCode = "code"`**, beside `KindRun`/`KindShell` in
   `internal/orchestrator/state.go:33`. Every `EffectiveKind()` call site must be audited —
   `internal/cli/manage.go` (`ls`/`stop`/`patch`/`rm`), `internal/cli/complete.go`,
   `internal/cli/doctor_orphan.go` — so a `code` session is listable, stoppable, patchable and not
   mistaken for an orphan.
10. **Ephemeral per-session key material**, ed25519, generated by krayt into `<runDir>/ssh/`: a
    client keypair, a guest host key, a pinned `known_hosts`, `authorized_keys`, `sshd_config` and
    an `ssh_config` block. Nothing is reused across sessions and nothing outlives the run dir.
11. **The ssh alias is per-run (`krayt-<run-id>`) with `UserKnownHostsFile` pointed at the run
    dir.** A fresh host key each session would otherwise trigger host-key-changed warnings against a
    stable alias. Because the host key is pinned into the run dir's `known_hosts` before you ever
    connect, `StrictHostKeyChecking yes` works on the first connection with no prompt — strictly
    better than the usual TOFU dance.
12. **krayt writes only inside `.krayt/`, and prints the rest.** Per-run config at
    `<runDir>/ssh/config`, plus a stable aggregate at `<stateDir>/ssh/config` that `Include`s the
    live sessions. krayt **never** edits `~/.ssh/config`; it prints the one-time line the user adds
    themselves (`Include <stateDir>/ssh/config`), because VS Code Remote-SSH reads the user's
    config and has no `-F` equivalent on the command line.
13. **krayt prints connection info; it never launches an editor.** No dependency on a `code` binary
    being on `PATH`. Print the `ssh -F … krayt-<id>` command, the one-time `Include` line, and the
    `vscode-remote://ssh-remote+krayt-<id>/workspace` URI.
14. **A built-in editor egress allowlist, printed, opt-out via `--no-editor-allow`.** VS Code
    Remote-SSH downloads its own ~100MB server into `~/.vscode-server` on first connect; under the
    default `--net allowlist` with no `--allow`, that download fails and the feature is broken out
    of the box. `krayt code` therefore adds a small named set on top of the user's policy —
    `update.code.visualstudio.com`, `vscode.download.prss.microsoft.com`,
    `marketplace.visualstudio.com`, `*.vsassets.io`, `*.vscode-unpkg.net` — and the policy banner
    must show exactly what was added, so nothing is silently widened. Under `--net none` the
    allowlist is omitted (there is no policy to add to) and krayt warns that Remote-SSH cannot
    fetch its server.
15. **`openssh-server` and `procps` go into all three agent images** — `images/agents/claude-code`,
    `.../gemini-cli`, `.../opencode`. `procps` because VS Code's server shells out to `ps`. Add them
    to each image's existing `apt-get install` layer rather than adding a new one.
16. **Keygen is pure Go**: `crypto/ed25519` plus `golang.org/x/crypto/ssh` for OpenSSH marshalling
    (`ssh.MarshalPrivateKey`, `ssh.NewPublicKey`, `ssh.MarshalAuthorizedKey`). This is a new direct
    dependency — **amend `KRAYT_SPEC.md` §9.1 to pin it.** Chosen over shelling out to `ssh-keygen`
    so the whole path is unit-testable with no subprocess.
17. **Ingress stays closed.** This task must not add a port field, must not touch
    `--net-default-ingress`, and must not weaken `ValidateNetworkPolicyForMsb`. If you find yourself
    opening ingress, the design has gone wrong — stop and re-read decision 2.

## What to build

### `internal/sshsession` (new package — pure, no I/O beyond the run dir)

- `GenerateSession(user, runID string) (*Material, error)` — ed25519 client keypair + host key.
- Renderers, all pure and golden-testable: `RenderSSHDConfig`, `RenderSSHConfig`,
  `RenderKnownHosts`, `RenderAuthorizedKeys`, `VSCodeURI(alias, dir string) string`.
- `Write(dir string, m *Material) error` — lays `<runDir>/ssh/` out with correct modes: `0600` on
  both private keys, `0644` on the rest, `0700` on the directory.

The `sshd_config` it renders must set at minimum: `HostKey /.krayt/ssh/host_ed25519`,
`AuthorizedKeysFile /.krayt/ssh/authorized_keys`, `AllowUsers <sandbox user>`, `PermitRootLogin no`,
`PasswordAuthentication no`, `KbdInteractiveAuthentication no`, `UsePAM no`, `StrictModes no` (the
keys live under `/.krayt`, not in the user's `$HOME`), `PidFile none`, `X11Forwarding no`,
`PrintMotd no`, `AllowTcpForwarding yes`, and the Debian sftp subsystem
(`Subsystem sftp /usr/lib/openssh/sftp-server`).

`AllowTcpForwarding yes` is load-bearing and worth a comment: it is how VS Code forwards a dev
server in the sandbox back to your browser, and it rides the existing SSH connection — so it needs
no ingress either.

### `internal/orchestrator/code.go`

`Code(ctx, deps, spec, runDir, keep) (*CodeResult, error)`, modelled on `Shell()` in
`internal/orchestrator/shell.go` and reusing the same `copyInputs` → `helperSetup` →
`applyConfigSeeds` → … → `finishAndCollect` spine. Where `Shell` attaches a tty, `Code`:

1. generates and writes the SSH material;
2. `msb copy`s `host_ed25519`, `authorized_keys` and `sshd_config` to `/.krayt/ssh/`
   (`guestbin.GuestRoot` — deliberately outside `/workspace` and `/output`, so none of it can land
   in `changes.patch`);
3. execs as root to `mkdir -p /run/sshd` (sshd's privilege-separation directory; `/run` may be a
   tmpfs, so creating it in the Dockerfile is not sufficient) and to `chown root` + `chmod 600` the
   host key — sshd refuses a group- or world-readable host key regardless of `StrictModes`;
4. writes `<runDir>/ssh/config`, updates the aggregate `<stateDir>/ssh/config`, prints the
   connection block;
5. **blocks** until interrupted, then collects and tears down (unless `keep`).

Teardown registration must match `Shell`'s discipline exactly: registered before `Create` is
attempted, firing on every path except a clean exit under `keep`, so a failed create, a failed
setup, or a killed terminal cannot leak a sandbox.

### `internal/cli/code.go` and `internal/cli/code_stdio.go`

`newCodeCmd`/`bindCodeFlags`/`runCode`/`resolveCodeSpec`/`printCodeResult`, mirroring
`internal/cli/shell.go`. Flags: the `krayt shell` set (`--config --image --repo --secrets
--include-dirty --net --allow --bundle-depth --cpus --memory --disk --max-concurrency
--skip-resource-check --keep`) plus `--no-editor-allow` and the hidden `--stdio`. No `--timeout`, no
`--detach`, no `--on-question*`, no `--agent` launcher — same reasoning as `krayt shell`.

`code_stdio.go` holds the ProxyCommand path: resolve run id → record → sandbox name, then one `Exec`
with `Stdin: os.Stdin`, `Stdout: os.Stdout`, `Stderr: <runDir>/logs/sshd.log`, running
`/usr/sbin/sshd -i -e -f /.krayt/ssh/sshd_config` as root. Exit with the child's status. Be
deliberate about EOF and close ordering: probe p1 measured msb 0.6.16 dropping 16–20 of 25 round
trips on non-lingering socket shapes, and only a linger-until-peer-closes discipline hit 25/25.

### Images

Add `openssh-server` and `procps` to the existing `apt-get install --no-install-recommends` line in
all three `images/agents/*/Dockerfile`, with a comment naming this task. Do **not** add an sshd
service, an entrypoint change, or a `CMD` — nothing about sshd starts with the image. The non-root
`USER agent` contract and the wrap-don't-fork entrypoint contract
(`images/agents/claude-code/entrypoint.sh:8-12`) are both unchanged.

## Tests (the "Done when" for this task, all runnable without msb)

Use the existing scriptable fake `msb` seam — the test binary re-execs itself and dispatches on the
argv verb (`internal/sandbox/fakemsb_test.go`, `internal/cli/fakemsb_test.go`). Required:

- `TestSSHDExecArgsUseStreamAsRoot` — golden argv: `--user root`, `--stream`, never `--tty`.
- `TestStdioRoutesSSHDStderrAwayFromStdout` — the decision-4 regression test. Assert a byte written
  to the fake sshd's stderr never appears on stdout.
- `TestGenerateSessionKeysAreEd25519` / `TestSSHMaterialPermissions` — `0600` private keys, `0700`
  directory.
- `TestRenderSSHDConfigGolden`, `TestRenderSSHConfigGolden`, `TestRenderKnownHostsPinsHostKey`,
  `TestVSCodeRemoteURI`.
- `TestEditorAllowlistOrderedAfterDNSBeforeDenyGroups` — msb is first-match-wins and `allow@dns`
  must precede the deny groups (`internal/task/netpolicy_msb.go:123-138`); getting that order wrong
  once made every agent request `ENOTFOUND`.
- `TestEditorAllowlistOmittedWithFlag` and `TestEditorAllowlistOmittedUnderNetNone`.
- `TestCodeSessionCreatesCopiesSetsUpAndCollects` — full fake-msb lifecycle, asserting a
  `changes.patch` lands and the SSH material is copied under `/.krayt`, not `/workspace`.
- `TestKindCodeRoundTripsThroughState`, `TestLsShowsCodeSessions`, `TestStopDestroysKeptCodeSession`,
  `TestDoctorDoesNotReportCodeSessionAsOrphan`.
- `TestNoIngressFlagsEmittedByCode` — decision 17's guard: assert `krayt code`'s create argv
  contains no port/publish flag and does not alter `--net-default-ingress`.
- `GOOS=darwin go build ./...`, `GOOS=linux go build ./...`, `go vet ./...`, `go test -race ./...`
  and `golangci-lint run` all green.

## Done when

1. `krayt code --image <ref>` boots a sandbox, prints a connection block, and blocks.
2. The printed block contains a working `ssh -F <runDir>/ssh/config krayt-<id>` command, the
   one-time `Include` line, and the `vscode-remote://` URI.
3. `krayt ls` shows the session as `kind: code`; `krayt stop`, `krayt patch` and `krayt rm` all work
   on it; `krayt doctor --repo` does not report it as an orphan.
4. Ending the session produces `changes.patch` + `commits.bundle` and prints `krayt apply <run-id>`.
5. `--keep` leaves the sandbox running; `krayt stop <id>` destroys it.
6. The policy banner names every host the editor allowlist added; `--no-editor-allow` removes them.
7. `krayt code` emits no port, publish, or ingress-relaxing flag in any mode (test, not inspection).
8. All three agent images build with `openssh-server` + `procps` and still run as uid 1000.
9. The full test list above is green.

**Hardware-gated (§14 handoff, `HUMAN_TODO.md`):** criteria 1–5 need a real Apple-Silicon Mac with
`msb` ≥ 0.6.16 installed; `msb` is not available in this environment and **must not be faked**. Do
everything that is not gated: the code, the tests against the fake `msb`, the image changes, the
spec edits. Criteria 6–9 are fully verifiable offline — there is no excuse for leaving those.

## Docs and spec

- **§13** — add `krayt code` to the CLI surface with its flag set. Do not document `--stdio` as a
  user-facing verb (decision 5).
- **§6.x** — a new component section for the SSH session layer: the `sshd -i` over `msb exec`
  mechanism, why it is not the guest daemon §6.13 forbids, and the stdout/stderr separation rule.
- **§6.6** — a paragraph stating that `krayt code` reaches into the sandbox **without** opening
  ingress, and that the standing "krayt publishes no ports" position at `:307-310` is unchanged.
  This is the paragraph that keeps the security model honest; write it carefully.
- **§8.2** — the image contract gains `openssh-server` + `procps` for the published agent images.
- **§8.4** — the new `<runDir>/ssh/` artifacts and their modes.
- **§9.1** — pin `golang.org/x/crypto`.
- **§10** — the security model: ephemeral per-session keys, no persistent credential, no ingress,
  and the fact that the SSH channel inherits msb's exec authorization rather than adding a new one.
- **§11** — the image contract change.
- **§14** — a new phase with its offline and hardware "Done when" checkboxes.
- **`README.md`** — a short `krayt code` section with the VS Code setup steps.
- **`docs/ai-tasks/README.md`** — update this task's row with the outcome.

## Handoff (`HUMAN_TODO.md`, non-blocking)

Add one `[HUMAN]` entry, following §14's template, covering the checks that need a real Mac with
`msb` and a real VS Code:

1. **Does `sshd -i` complete an SSH handshake over `msb exec --stream`?** The whole design rests on
   this. Run `ssh -F <runDir>/ssh/config -v krayt-<id> true` and record the handshake.
2. **Does VS Code Remote-SSH connect, download its server, and open `/workspace`?** Record how long
   the first connect takes — the server download over a piped `msb exec` may be slow enough to need
   its own follow-up.
3. **Does the editor allowlist cover every host the download really touches?** Capture the denied
   destinations from a run with `--net allowlist` and no extra `--allow`, and correct decision 14's
   list against what was actually observed.
4. **Do `sftp`, `scp` and VS Code's port forwarding work** over the same channel?
5. **Publish and verify the three rebuilt agent images.**

Also log, as a separate unverified entry: **Windows.** The ProxyCommand path is the same code, but
nobody can confirm Windows' OpenSSH client invoking `krayt.exe` as a ProxyCommand without a Windows
box — **do not claim Windows works.**

## Out of scope (log, don't fix)

- **A published-port fast path.** Once someone with hardware establishes msb's actual port and SSH
  flags, a direct TCP path would be faster than piping through `msb exec`. That is a follow-up task
  and needs the ingress question reopened deliberately — note it in `HUMAN_TODO.md` as a question to
  answer, not work to do here.
- **`krayt code --attach <run-id>`.** The ProxyCommand already reconnects to a live session, so
  attach adds nothing until someone wants a second sandbox shape.
- **Pre-baking the VS Code server into the images.** It would make `--net none` viable but pins a
  server version against the user's local VS Code; decision 14 takes the allowlist instead.
- **Attaching an editor to a live `krayt run`.** `KRAYT_SPEC.md` §15 rules out two actors mutating
  one `/workspace` concurrently. `krayt code` is its own session, like `krayt shell`; it does not
  reopen that.
- **JetBrains Gateway / Cursor-specific UX.** They work because SSH works; krayt ships no
  editor-specific integration beyond the printed VS Code URI.

## Report

Write to **`/output/krayt-code-report.md`**, not `/output/report.md` — the agent entrypoints run
`<agent> … | tee "$OUTPUT_DIR/report.md"`, so `tee` owns that file for the whole run and an agent
writing it corrupts the output (`HUMAN_TODO.md`'s open `[BUG]` entry; writing to a different file
under `/output` is the proven workaround, since krayt collects the whole directory).

State:

- what landed, and what you left undone and why;
- the exact `sshd_config` you generated and any directive you had to add beyond decision 10's list;
- the test list and its results;
- what went to `HUMAN_TODO.md`;
- anything in "Decisions already made" you found hard evidence against.
