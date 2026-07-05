# Task: read the task prompt from stdin (`krayt run --task -`)

**Read `CLAUDE.md` and `KRAYT_SPEC.md` (§13 CLI, §8.1 config, §6.2 detach) first. Proceed
autonomously — this is a self-contained task run inside a krayt sandbox; there is no interactive
human to approve a plan (use the `ask_human` tool only if genuinely blocked).**

## Goal

Let `krayt run --task -` read the task prompt from **stdin**, so a prompt can be supplied headlessly
without a file on disk:

```sh
echo "fix the flaky test in internal/foo" | krayt run --image … --repo . --task -
krayt run --image … --repo . --task - <<'EOF'
a multi-line
prompt
EOF
```

This is a **host-side CLI change only** — no protobuf, no guest, no VM-image rebuild. The prompt is
already just `TaskPrompt []byte` in the run spec; today it is only sourced from a file
(`os.ReadFile`). `-` should source it from **stdin** instead.

**Out of scope:** a `--prompt "…"` inline flag (deliberately deferred), and the `task_text:`
krayt.yaml key (a separate, pre-existing unimplemented item) — leave both alone.

## Current behavior (grounding)

- `internal/cli/run.go:86` registers `--task` as a file path.
- `internal/cli/run.go:111` requires `--task` non-empty; `:114` does `os.ReadFile(f.taskFile)`, which
  becomes `spec.TaskPrompt` (`:163`).
- The guest writes those bytes to `/task/prompt.md` (§8.2) regardless of source, so nothing
  downstream of `TaskPrompt` changes.

## Implement (`internal/cli/run.go`)

1. **Read stdin when `--task -`.** In `runRun`, branch the prompt read:
   - if `f.taskFile == "-"` → read all of **`cmd.InOrStdin()`** (use cobra's reader, *not* `os.Stdin`
     directly, so it is testable);
   - else → `os.ReadFile(f.taskFile)` exactly as today.
2. **Reject an empty prompt.** After reading (file *or* stdin), if the prompt is empty
   (`len(bytes.TrimSpace(prompt)) == 0`), return a clear error (e.g. `task prompt is empty`). This
   catches both an empty file and an empty/closed stdin.
3. **`--task -` + `--detach` — make it work by spooling (do NOT block it).** The prompt read
   (step 1) happens at ~`run.go:114`, *before* the detach fork at ~`run.go:184`, so the parent
   already has the bytes. The detached supervisor re-execs `krayt run` and re-reads the task, but its
   stdin is gone — so hand it the bytes via a **file**, mirroring how the run id is handed over
   today. `spawnDetachedRun` (`run.go:292`) appends env vars for the child
   (`run.go:301`: `envDetachChild`, `envRunID`); add one more in that same pattern:
   - when detaching with a stdin prompt, write the read bytes to a file — the run dir is the natural
     home (`orchestrator.RunDir(stateDir, id)` + `/prompt.md`, creating the dir if needed) — and
     append `envTaskFile=<path>` (a new `KRAYT_TASK_FILE` const) to the child's environment;
   - at the task-read point (~`run.go:114`), resolve the source **in this order**: (a) if
     `envTaskFile` is set (detached child) → read that file; (b) else if `f.taskFile == "-"` → read
     `cmd.InOrStdin()`; (c) else → `os.ReadFile(f.taskFile)`.
   Result: `echo "…" | krayt run --task - --detach` works — the supervisor reads the spooled prompt,
   which also lands in the run dir as a nice record of what was run.
4. **Help text** (`run.go:86`): `"path to the task prompt file, or - to read from stdin (required)"`.

## Tests (`internal/cli`)

- `--task -` reads from `cmd.InOrStdin()`: set a `strings.Reader`/`bytes.Buffer` as the command's
  input and assert the built `RunSpec.TaskPrompt` equals the piped bytes. (Bind the run flags to a
  test `*cobra.Command` and drive the spec build, mirroring the existing run-flag/config-precedence
  tests.)
- Empty stdin **and** empty file → the "empty prompt" error.
- `--task -` with `--detach` → the piped prompt is spooled to a file and handed to the child via
  `envTaskFile` (assert the spool file is written with the bytes and the child-side read resolves it
  from that env var) — i.e. the combination **works** rather than erroring.
- Existing `--task <file>` path still works (regression).

## Docs

- `KRAYT_SPEC.md` §13: document `--task -` (stdin) alongside the `--task` flag.
- Update the `--task` flag help and any README/usage text that says "task file" to mention stdin.

## Verify (all offline in the sandbox)

```sh
go build ./...
GOOS=linux GOARCH=arm64 go build ./...
go test -race ./...
golangci-lint run
```

No new dependency is introduced (stdlib `io`/`bytes`), so this runs offline via the pre-baked
module cache — no `--allow` beyond `api.anthropic.com` is needed for the agent.

## Constraints

- **Host-side only** — do not touch the protobuf, the guest, or `internal/vmimage/pinned.go`; there
  is no image rebuild involved.
- Keep `--task <file>` behavior byte-for-byte identical; `-` is purely an additional source.
- Small, focused diff.
