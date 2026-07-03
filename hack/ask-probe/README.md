# ask-probe — hardware confirmation for the §6.13 question channel

A throwaway "agent" image that drives the Phase-4 agent→human question channel end to end on
real hardware — the confirmation logged in `HUMAN_TODO.md`. It connects to the in-VM ask
bridge (`/run/krayt/ask.sock`), asks one question, blocks for your answer, and writes the
answer into `/workspace` so it appears in `changes.patch`.

It speaks the raw wire protocol (no krayt imports), logs every hop, and returns a distinct
exit code per failure, so a break is obvious from `krayt ls` (EXIT column) or the logs.

## Prerequisites
- Apple-Silicon Mac with the `krayt` binary built (`go build -o bin/krayt ./cmd/krayt`).
- The base micro-VM image already built + pinned (same one Phase 2/3 used).
- A container registry the Mac can pull from (Docker Hub, GHCR, …).

## 1. Build + push the probe image (linux/arm64)
```sh
cd hack/ask-probe
docker buildx build --platform linux/arm64 -t <your-registry>/krayt-ask-probe:latest --push .
```
`--platform linux/arm64` matters: the VM is arm64. (The binary is already `GOARCH=arm64`; this
tags the manifest so the pull picks the right one.)

## 2. A throwaway repo + task for the run
`krayt run` needs a git repo and a task file; the probe ignores the task's contents.
```sh
mkdir /tmp/ask-demo && cd /tmp/ask-demo && git init -q && echo hi > seed.txt && git add -A && git commit -qm init
echo "just call the ask bridge" > task.md
```

## 3. Run in wait mode, then answer from a second terminal
Terminal A (blocks in `waiting`, streams the probe's `[ask-probe]` logs):
```sh
krayt run --image <your-registry>/krayt-ask-probe:latest --task task.md --repo . --on-question=wait
```
You should see `[ask-probe] ok: question sent — the run is now waiting …`, and a **macOS
notification** should fire.

Terminal B:
```sh
krayt ls                       # the run shows STATE=waiting
krayt answer <run-id> yes      # dials the recorded ctrl_socket and resolves it
```

## Success looks like
- A desktop notification fired when the run entered `waiting`.
- `krayt ls` went `waiting` → `running` → `done` (EXIT `0`).
- `.krayt/runs/<id>/questions/<qid>.json` exists with `prompt: "ask-probe: proceed?"` and your answer.
- `krayt patch <id>` → the patch adds `ask-probe-decision.txt` containing `yes`.

## Bonus: prove `fail` mode is inert (one terminal, no answer)
```sh
krayt run --image <ref> --task task.md --repo .        # default --on-question=fail
```
It must finish on its own (exit 0) with `ask-probe-decision.txt` = `no-answer-sentinel` —
i.e. the agent got the sentinel immediately and never blocked.

## Exit codes (what broke, if it isn't 0)
| exit | meaning | likely cause |
|------|---------|--------------|
| 0  | success | — |
| 10 | `/run/krayt/ask.sock` absent | runner didn't bind-mount it, or the guest couldn't `net.Listen("unix", …)` — the logs dump `/run` + `/run/krayt` |
| 11 | dial failed | socket exists but no one is serving (guest bridge not wired) |
| 12 | send failed | connection dropped mid-write |
| 13 | receive failed | no answer arrived / stream torn down before an answer |
| 14 | write `/workspace/…` failed | workspace not writable (uid/mount issue) |

`krayt logs <id>` shows the full `[ask-probe]` trace; the exit code is in `krayt ls`.

## Cleanup
```sh
krayt rm <run-id>
docker rmi <your-registry>/krayt-ask-probe:latest   # optional
```

This whole directory is a nested Go module, isolated from the krayt build — delete it once the
Phase-5 `krayt-ask` front-end supersedes it.
