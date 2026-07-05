# AI tasks

Self-contained task/prompt markdown files written for an AI coding agent — hand one to Claude
directly, or run it in a krayt sandbox with `krayt run --task docs/ai-tasks/<file>.md` (dogfooding).

Each file should be self-contained: enough context that a fresh agent with no prior conversation
can act on it. Name them descriptively in kebab-case after the outcome (e.g.
`build-krayt-dev-image.md`).

| Task | What it does |
|---|---|
| [`build-krayt-dev-image.md`](./build-krayt-dev-image.md) | Build the multi-arch `krayt-dev` agent image (Claude Code + the krayt dev toolchain) and its GHCR publish workflow, for dogfooding krayt on krayt. |
| [`task-prompt-from-stdin.md`](./task-prompt-from-stdin.md) | Add `krayt run --task -` to read the task prompt from stdin (host-side CLI only; no image rebuild). |
