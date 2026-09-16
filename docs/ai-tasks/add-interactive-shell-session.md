# Task: add `krayt shell` — an interactive terminal inside the sandbox

**Read `CLAUDE.md`, then `KRAYT_SPEC.md` §3, §6.15, §7, §8.2, §13 and §15 first.** Give a short
plan and proceed. Depends on `run-tasks-on-microsandbox.md` (the msb cut-over) having landed.

This task **reopens a decision the spec closed**, deliberately and with the repo owner's explicit
sign-off. §15 currently reads:

> **Mid-run human input** — *resolved:* async `ask_human` question channel (§6.13), not a
> terminal. Full interactive/attached pairing remains intentionally out of scope.

That stays true for *mid-run* pairing into an autonomous agent run — this task does not build
that. What it adds is a **separate, human-driven session**: `krayt shell` boots a sandbox from the
repo snapshot and hands the human a terminal inside it. The §15 bullet must be amended to say so
rather than silently contradicted (see "Spec amendments").

## Background

Everything krayt does today assumes nobody is watching: `krayt run` hands an agent a prompt, the
agent works autonomously, and a patch comes out. The value proposition of the sandbox — the agent
can run anything, install anything, and touch credentials, without any of it reaching the host —
is just as useful when a *human* is driving, and in one case more so: sitting down to plan or
discuss a change with an agent, in a tree with the real code and the real tools, where the agent
never has to ask permission because the blast radius is already bounded by the VM.

The alternative today is running the agent on the host in a git worktree, where every tool call is
either a permission prompt or a `--dangerously-skip-permissions` you talked yourself into. That is
the gap this closes.

The mechanism is mostly already there. `msb 0.6.16` ships `msb exec -t/--tty` —
*"Allocate a pseudo-terminal (enables colors, line editing)"*, and with no command after `--` it
*"attaches to the default shell"*. So there is no pty-over-pipe framing to build, no upstream
patch, and (see decision 9) most likely no terminal-handling code in krayt at all.

## Decisions already made (do not re-litigate)

1. **A new top-level `krayt shell` command**, a sibling of `run` — not `krayt run --interactive`.
   `run` is the headless autonomous path and stays exactly that; folding in an interactive mode
   would make `--task`, `--on-question`, `--detach`, `--timeout` and the report flow all
   conditionally meaningless.

2. **It attaches a bare login shell in `/workspace`. It does not launch an agent.** The human runs
   `claude`, `vim`, `pytest` or anything else themselves, from inside. This is §3 principle 2
   (agent-agnostic core) held intact: `krayt shell` must not grow a per-image table of interactive
   agent commands. A `--exec` convenience flag that runs an arbitrary command instead of the shell
   is acceptable if it falls out naturally; a per-agent launcher is not.

3. **Ephemeral by default; `--keep` opts into survival.** Without `--keep`, the sandbox is created
   for the session and torn down when the shell exits — §3 principle 3 unchanged, and a dropped
   terminal leaks nothing. With `--keep`, the sandbox survives the shell exiting and is re-entered
   with `krayt shell --attach <run-id>`; it dies on `krayt stop <run-id>`. The default is what
   preserves the invariant; `--keep` is the user asking for the leak risk by name.

4. **Re-entry is `krayt shell --attach <run-id>`.** Explicit and unambiguous. Do **not** overload
   `krayt attach <run-id>`, which means "stream logs" and must keep meaning that — one verb with
   two behaviours depending on run kind is a trap for later.

5. **No `--max-duration` and no wall-clock context timeout in shell mode.** `run`'s 30m default is
   wrong for a session a human is sitting in, and any fixed larger number is equally arbitrary. A
   shell session is bounded by the human closing it or `krayt stop`. This is a deliberate trade:
   see decision 6 for what pays for it.

6. **`krayt doctor` gains an orphan check.** Decision 5 means a crashed or `kill -9`'d krayt can
   leave an msb sandbox running with nothing to reap it. `doctor` cross-references the live
   sandbox list against `.krayt/runs/` and reports any `krayt-*` sandbox with no live run record,
   naming it and the command to stop it. **Report only — never reap.** krayt killing a sandbox it
   does not fully understand (one deliberately kept across a krayt upgrade, say) is a destructive
   default. Unlike the four existing msb checks this one is a **warning**, not a `[FAIL]`: an
   orphan is untidy, not a broken host.

7. **A shell session is a `RunRecord`**, in `.krayt/runs/<id>/` like any run, with a new
   `Kind` field (`"run"` | `"shell"`; absent means `"run"` for records written before this task).
   `krayt ls`, `logs`, `stop`, `rm`, `patch` and `apply` then keep working on it with no new code,
   and the orphan check in decision 6 has something to cross-reference. Do not invent a parallel
   "sessions" namespace.

8. **The session produces a patch, same as a run.** On exit — before teardown — run `krayt-helper
   finish` and collect `/output` exactly as §7 steps 8–10 do, so `changes.patch`, `report.md` and
   `meta.json` land in the run dir and `krayt apply <run-id>` works unchanged. A shell session
   whose deliverable evaporated with the VM would not be worth booting a VM for.

9. **Mid-session snapshots are host-side: `krayt patch <run-id>` against a live shell session.**
   From a second terminal, when the record is a live `kind: shell`, `patch` runs `krayt-helper
   finish` + copy-out on demand and prints the path, instead of just `stat`-ing a file that isn't
   there yet. Repeatable and idempotent — each call re-derives the patch from the current
   workspace. Copy-out is inherently a host operation, so this needs **no** new guest binary and
   **no** second guest→host channel. Explicitly **not** an askbridge-style vsock signal from
   inside the shell; that is a whole second protocol for a gesture that a second terminal already
   serves. On a *finished* run, `krayt patch` keeps its current behaviour byte-for-byte.

10. **msb owns the pty; krayt inherits stdio.** Spawn `msb exec -t` with `Stdin`/`Stdout`/`Stderr`
    inherited from krayt's own terminal and let msb do raw mode, echo, CRLF and `SIGWINCH`. This
    keeps `golang.org/x/term` (not currently in `go.mod`) out of §9.1 entirely and leaves krayt
    with no terminal code. **Verify this before building on it** — see "Verify first". If msb's
    tty handling turns out to be insufficient, the fallback is hand-rolled raw mode via the
    already-pinned `golang.org/x/sys`; adopt it only with evidence, and record the evidence.

11. **Network policy is `run`'s, unchanged**: default `allowlist`, resolved from `krayt.yaml` and
    `--net`/`--allow` by the same code path. Default-deny (§3 principle 4) is not relaxed because
    a human is at the keyboard. No interactive "allow this host?" prompt in this task — the proxy
    has no such path and a per-session mutable allowlist is new state; it is a fine follow-up.

12. **`ask_human` is off in shell mode.** No `--vsock` route, no bridge, no `KRAYT_ASK_SOCKET`, no
    `waiting` state. The human is already in the room.

13. **`--task` is optional.** If given, copy it to `/task/prompt.md` like a run does, so an agent
    started from inside the shell can read it. If omitted, no task file is copied and nothing
    fails — the prompt is the human's to type.

14. **One task doc, two clearly separated halves.** The host-side command and the image-side
    `krayt-agent-shellenv` ship together: `krayt shell` without the env wiring is a shell where the
    agent may not authenticate, which is the whole point of the feature. The image half is its own
    section below; the rebuild is CI on merge to `main`, not a manual blocker.

## Verify first — do these before writing the implementation

Each of these decides how much code gets written. Run them against a real sandbox and **record the
answers in your report**; a wrong guess here is a rewrite.

1. **Does `msb exec -t` give a usable terminal when krayt inherits stdio?** Boot any agent image
   by hand and check: window resize reflows (`SIGWINCH`), `Ctrl-C` interrupts the foreground
   command rather than killing the session, a full-screen TUI renders and exits cleanly, and 256
   colour / mouse reporting survive. This is decision 10's premise.

2. **Does `msb exec` inherit the sandbox's create-time environment?** `CreateSpec.Env` sets the
   sandbox environment; `ExecSpec` has no `Env` field at all (§6.15, deliberately — it is what
   makes the secret Timing rule structural). If an exec'd shell already sees the create-time env,
   the credential is *already present* in the shell and decision 15's script has almost nothing to
   carry. If it does not, the script carries more. **Find out; do not assume either way.**

3. **What does the `--secret` placeholder actually look like inside an exec'd shell?** §8.2: msb
   sets the credential env var to its own placeholder at create time and substitutes the real
   value host-side on requests to the allowed host. Confirm an interactively-started `claude`
   inside the sandbox reaches `api.anthropic.com` on that basis.

4. **Confirm what is genuinely missing from a bare shell.** Note carefully: the credential
   materialization from `/run/secrets` and the `KRAYT_CA_CERT` / CA-bundle blocks in all three
   image entrypoints are **dead code under msb** — §8.2 states plainly *"There is no
   `/run/secrets` under msb"*, `internal/task/netpolicy_msb.go` rejects `network.mitm` outright,
   `internal/proxy` is deleted, and no Go file in the repo sets `KRAYT_CA_CERT`. **Do not port
   dead code into a new script.** The one gap known for certain today is `git config --global
   --add safe.directory /workspace` (the workspace `.git` is root-owned; a non-root shell's git
   refuses it with "dubious ownership"). Whatever else step 2 turns up joins it.

## What to build — host side

- **`internal/cli/shell.go`** — the `krayt shell` command: `--image`, `--repo`, `--config`,
  `--secrets`, `--net`/`--allow`, `--cpus`/`--memory`/`--disk`, `--include-dirty`,
  `--bundle-depth`, `--task` (optional), `--keep`, `--attach <run-id>`, `--max-concurrency`,
  `--skip-resource-check`. Reuse `bindRunFlags`' config-precedence path (§8.3) rather than
  re-deriving it. Deliberately absent: `--timeout` (decision 5), `--detach`, `--on-question*`,
  `--agent`, `--transcript`. Add `--attach`'s dynamic run-id completion, filtered to live
  `kind: shell` records, alongside the existing `completeRunIDs` helpers (§13).

- **`internal/sandbox`** — one new method beside `Exec`, for the tty path. `Exec` hardcodes
  `--stream` and forces stdin to an explicit pipe, with the comment *"never the terminal/this
  process's own stdin"* (`msb.go:535`); `--stream` and `--tty` are mutually exclusive by msb's own
  clap config. So this is a **separate method with its own spec type**, not a flag on `ExecSpec` —
  and the new method's comment must say explicitly that inheriting the caller's terminal is the
  deliberate, reviewed exception to that rule, and why. Render its argv through a pure `Args()`
  function like `CreateSpec`/`ExecSpec` do, so the surface is unit-testable against the fake `msb`.
  Add a sandbox-list method (`msb ls --format json`) for decision 6's orphan check — nothing
  outside `internal/sandbox` may construct an msb argv (§6.15).

- **`internal/orchestrator`** — a `Shell` entry point beside `Run`, reusing §7 steps 1, 2, 4, 5
  and 6 verbatim (resolve spec, register teardown, create, copy in, helper `setup`), then the tty
  attach in place of step 7's agent exec, then steps 8–10 on exit. Factor the shared prologue
  rather than copying it. Two departures from `Run`, both deliberate: no `context.WithTimeout` and
  no `--max-duration` (decision 5), and the deferred teardown from step 2 is **skipped when
  `--keep` is set and the shell exited cleanly** — it must still fire on every error path, which
  is the part to get right and test.

- **`RunRecord.Kind`** in `internal/orchestrator/state.go`, `omitempty`, absent == `"run"`.
  Surface it in `krayt ls`. Extend the §8.4 `meta.json` schema note.

- **`krayt patch <run-id>`** — decision 9's live branch.

- **`krayt doctor`** — decision 6's orphan check, as a fifth `checkResult` with `optional: true`.
  Note it is the first check needing repo state (`.krayt/`), so `doctor` may need a `--repo` flag;
  degrade to reporting nothing rather than failing when there is no `.krayt/` to compare against.

Test everything against the scriptable fake `msb` the orchestrator tests already re-exec
themselves as (§14 test strategy) — including the teardown-on-error-with-`--keep` matrix, which is
where a leaked sandbox would come from.

## What to build — image side (delimited)

- **Extract the setup half of `images/agents/claude-code/entrypoint.sh` into a sourceable
  `/usr/local/bin/krayt-agent-shellenv`**, and have the entrypoint `source` it rather than
  duplicating it — the entrypoint's own header warns that a second copy is exactly what caused a
  past bug, so a copy-paste here would be a regression, not a shortcut. Do the same for
  `gemini-cli` and `opencode`. `hack/krayt-dev` ships no entrypoint (it inherits claude-code's)
  and needs no change.

- **Scope it to what "Verify first" found.** The script carries `safe.directory` plus whatever
  step 2 shows the exec'd shell genuinely lacks. If the credential env var is already inherited,
  say so in a comment and keep the script small — a script that re-exports what msb already set
  is noise that will rot. Leaving the entrypoint's dead `/run/secrets` and CA blocks where they
  are, untouched and out of the new script, is the correct call for this task; retiring them is a
  separate cleanup.

- **Source it from the interactive shell.** `/etc/profile.d/krayt-shellenv.sh` sourcing it is the
  conventional hook and works with `bash -l`. Whatever you choose, it must work for the shell
  `msb exec -t` actually starts — confirm which that is rather than assuming `bash`.

- **Extend `hack/test-entrypoint-credentials.sh`** to exercise the extracted script directly. It
  runs offline with no Docker and no VM, and it is the only seam that covers these shell scripts
  at all — so it is where the extraction's correctness gets proven, not the image build.

## Done when

1. `krayt shell --image <ref> --repo .` drops you at a shell in `/workspace` with the repo
   snapshot present, and `exit` tears the sandbox down and leaves `changes.patch` + `meta.json`
   (`kind: "shell"`) in `.krayt/runs/<id>/`.
2. Edits made in that shell appear in the collected patch, and `krayt apply <run-id>` applies them
   to the host repo.
3. `krayt shell --keep` leaves the sandbox running on exit; `krayt shell --attach <run-id>`
   re-enters the same workspace with the edits still there; `krayt stop <run-id>` destroys it.
4. Without `--keep`, the sandbox is gone after exit — and also after a `Ctrl-C`, a failed
   `krayt-helper setup`, and a killed terminal. Prove the error paths with the fake `msb`, not
   just the happy path.
5. `krayt patch <run-id>` against a live shell session writes and prints a fresh `changes.patch`
   without ending the session, and can be run repeatedly.
6. `krayt doctor` reports a `krayt-*` sandbox with no live run record as a `[warn]`, naming it and
   how to stop it, and stays silent when there are none.
7. An agent started by hand from inside the shell (`claude` in the claude-code image) authenticates
   and reaches its API host under the run's `allowlist` policy.
8. `hack/test-entrypoint-credentials.sh` passes, including new cases for `krayt-agent-shellenv`.
9. `go build ./... && go test ./... && golangci-lint run` clean on macOS **and** Linux; no new
   OS-tagged files (§9.1) and no new module in `go.mod` — if decision 10's fallback forced
   `golang.org/x/term` in, that is a §9.1 amendment and must be called out explicitly, with the
   evidence that forced it.
10. Spec amendments below are made in the same change.

**Hardware-gated (§14 handoff, `HUMAN_TODO.md`):** criteria 1–7 all need a real Apple-Silicon Mac
with `msb` installed, and the "Verify first" checks need one before implementation even starts.
Windows is the same code path (inherited stdio, no new seam) but nobody can confirm ConPTY through
msb without a Windows box — log that as a separate unverified entry and **do not claim Windows
works**. Do everything not gated: the code, the tests against the fake `msb`, the image scripts,
the spec edits.

## Spec amendments (same change)

- **§15** — rewrite the "Mid-run human input" bullet. Keep it resolved for pairing *into an
  autonomous run*, and record that a standalone human-driven session (`krayt shell`) was added
  deliberately, by this task, with the reasoning above.
- **§13 CLI surface** — add the `krayt shell` line and its flags; note `patch`'s live branch.
- **§6.15** — document the new tty method beside `Exec`, including why the inherited-stdio
  exception to the "never the terminal" rule exists, and the sandbox-list method.
- **§7** — a short subsection for the shell lifecycle's departures from the 12-step run lifecycle
  (no timeout, conditional teardown, tty attach in place of step 7).
- **§8.2** — `/usr/local/bin/krayt-agent-shellenv` as part of the container contract.
- **§8.4** — the `kind` field in `meta.json`.
- **§14** — a new phase with its "Done when", checked off only when the hardware run passes.
- **`docs/ai-tasks/README.md`** — a status row for this task.
- **`README.md`** — a short `krayt shell` section, which must state the snapshot-drift caveat:
  a long session works from a snapshot taken at boot while the host repo moves on, so its patch
  can conflict. That is inherent to krayt's model, but `krayt shell` makes it far likelier than a
  20-minute autonomous run does, and a user who hits it without warning will read it as a bug.

## Out of scope

- **Attaching a terminal into a running *agent* run** (`krayt run` + a second tty). Genuinely
  useful, genuinely different: two actors mutating one `/workspace` concurrently, with the run's
  own timeout and patch collection still live around them. Its own task.
- **Launching the agent TUI directly** (`krayt shell --agent claude-code`). Decision 2.
- **An interactive allow-this-host prompt.** Decision 11.
- **Automatic orphan reaping.** Decision 6.
- **Retiring the dead `/run/secrets` and `KRAYT_CA_CERT` blocks** from the image entrypoints.
  Real cleanup, unrelated to this feature, and touching them here would confuse the diff.
- **Warm sandbox pooling** to make `krayt shell` start faster (§15).
