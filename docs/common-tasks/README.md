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
| [`fix-pr-review-comments.md`](./fix-pr-review-comments.md) | Triage a PR's inline **review** comments (e.g. GitHub Copilot's automated review) from the checked-out branch: verify each against the actual current code, fix only what's real, state plainly why a false positive is wrong — then surface every fix as krayt's `changes.patch` for a human to apply. Read-only against GitHub; never comments/pushes/approves/merges. |
