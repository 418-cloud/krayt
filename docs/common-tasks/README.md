# Common tasks

Self-contained, **repeatedly-runnable** operating procedures written for an AI coding agent — hand
one to Claude directly, or run it in a krayt sandbox with
`krayt run --task docs/common-tasks/<file>.md --repo .` (dogfooding).

How this differs from [`docs/ai-tasks/`](../ai-tasks/README.md): those files are **one-off** tasks
that build a specific krayt feature (done once, then marked ✅). The files here are **generic
operating procedures** you run again and again against whatever state the repo is in at the time —
they're invoked the same way, but aren't tied to a single change and don't get "done".

Each file should be self-contained: enough context that a fresh agent with no prior conversation can
act on it. Name them descriptively in kebab-case after the outcome (e.g.
`fix-pr-review-comments.md`).

| Task | What it does |
|---|---|
| [`verify-rtk-integration.md`](./verify-rtk-integration.md) | Prove — from inside the sandbox — that the running image actually has `rtk`, that Claude Code's `PreToolUse` hook resolves through the `KRAYT_RTK=off` wrapper rather than around it, and that rewriting measurably shrinks command output. Re-run it after any agent-image or `krayt-dev` base bump; run it a second time with `KRAYT_RTK=off` as the negative control. Reports to `/output/rtk-verify-report.md`, changes no files. |
| [`fix-pr-review-comments.md`](./fix-pr-review-comments.md) | Triage a PR's inline **review** comments (e.g. GitHub Copilot's automated review) from the checked-out branch: verify each against the actual current code, fix only what's real, state plainly why a false positive is wrong — then surface every fix as krayt's `changes.patch` for a human to apply. Read-only against GitHub; never comments/pushes/approves/merges. |
| [`fix-pr-ci-failures.md`](./fix-pr-ci-failures.md) | Diagnose a PR's **failing GitHub Actions checks** from the checked-out branch: find the failing runs/jobs for the head commit, read the real `--log-failed` output, reproduce each locally where the sandbox can (`go test`, `golangci-lint`, the arm64 cross-build), fix the root cause — and mark the macOS/VM/Docker/Nix jobs it can't run as unverified rather than guessing. Never greens a check by skipping a test or disabling a step. Read-only against GitHub; never re-runs, comments, or pushes. |
