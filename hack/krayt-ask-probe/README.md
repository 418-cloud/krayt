# krayt-ask-probe — hardware confirmation for the `krayt-ask` CLI front-end (§6.13)

A throwaway **non-root** image that exercises the `krayt-ask` binary front-end end to end — the
last Phase-5 "Done when" clause. It shells out to `krayt-ask` (which krayt bind-mounts onto the
container PATH at `/usr/local/bin/krayt-ask`), submits one question, blocks for your answer, and
writes it into `/workspace` so it appears in `changes.patch`.

Because it runs as uid 1000, a success also confirms the non-root fixes (socket connect +
workspace write). It's the `krayt-ask`-binary analogue of `ask-probe` (which used its own socket
client); use whichever you like.

> **Published by CI.** `.github/workflows/probe-images.yml` builds every probe multi-arch
> (`linux/amd64` + `linux/arm64`) into one package, with the probe type as the tag:
> `ghcr.io/<owner>/krayt-probe:{probe}`. Use that rather than building by hand — the manual steps
> below remain valid for iterating on the probe itself. Note the arch: the Linux/firecracker
> backend needs `amd64`, the macOS/vfkit backend `arm64`, and CI publishes both.

## Prerequisites
- Apple-Silicon Mac with `krayt` built (`go build -o bin/krayt ./cmd/krayt`).
- The base micro-VM image **rebuilt + re-pinned** with the krayt-ask mount + the non-root fixes
  (see `HUMAN_TODO.md` "[Phase 5] Rebuild VM image to ship the krayt-ask CLI front-end").
- A container registry the Mac can pull from.

## 1. Build + push the probe image (linux/arm64)
```sh
cd hack/krayt-ask-probe
docker buildx build --platform linux/arm64 -t <your-registry>/krayt-ask-probe:latest --push .
```

## 2. A throwaway repo + task
```sh
mkdir /tmp/ka-demo && cd /tmp/ka-demo && git init -q && echo hi > seed.txt && git add -A && git commit -qm init
echo "just call krayt-ask" > task.md
```

## 3. Run in wait mode, answer from a second terminal
Terminal A (blocks in `waiting`, streams the `[krayt-ask-probe]` logs):
```sh
krayt run --image <your-registry>/krayt-ask-probe:latest --task task.md --repo . --on-question=wait
```
Terminal B:
```sh
krayt ls                       # STATE=waiting
krayt answer <run-id> yes      # resolves it
```

## Success looks like
- `krayt ls` goes `waiting` → `done` (EXIT `0`).
- The logs show `[krayt-ask-probe] got answer: yes`.
- `.krayt/runs/<id>/questions/<qid>.json` has the Q&A; `changes.patch` adds
  `krayt-ask-decision.txt` containing `yes`.

## Bonus: prove `fail` mode returns the sentinel immediately
```sh
krayt run --image <ref> --task task.md --repo .        # default --on-question=fail
```
Finishes on its own (exit 0) with `krayt-ask-decision.txt` = `no-answer-sentinel` — the CLI got
the sentinel from an unanswered/immediately-sentineled question and the agent fell back.

## Exit codes
| exit | meaning |
|------|---------|
| 0  | success (answered, or sentinel in fail mode) |
| 10 | `krayt-ask` not on PATH — the runner didn't mount it (base image missing the fix) or the image has no `/usr/local/bin` on PATH |

## Cleanup
```sh
krayt rm <run-id>
docker rmi <your-registry>/krayt-ask-probe:latest   # optional
```
