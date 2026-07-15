# Task: triage and fix a PR's review comments

You are running inside a krayt sandbox (the `krayt-dev` image) against a **local checkout of a pull
request's branch**, at `/workspace`. A PR on this repo has automated (and possibly human) **review
comments** — inline, per-line findings, e.g. from GitHub Copilot's automated reviewer. Your job is
to triage every one of them: **verify each against the actual current code, fix only what's genuinely
wrong, and for each false positive say specifically why it's wrong** — then hand the fixes back as
krayt's own patch for a human to review and apply.

This is a generic, repeatable procedure — it is not tied to any one PR. It assumes the PR's branch is
already checked out (that's what `--repo .` from a local checkout of the branch gives you).

## What you have

- `gh`, the GitHub CLI, is installed. It is authenticated **only if** a `GH_TOKEN` secret was
  supplied to this run. That token is **read-only** — a fine-grained PAT scoped to this repo with
  **Metadata / Contents / Pull requests: read** and nothing else.
- `api.github.com` must be in the run's `--allow` egress list for any `gh` call to work. If `gh`
  calls fail with a network/egress error, stop and report that `api.github.com` is missing from
  `--allow` (and, if auth also fails, that `GH_TOKEN` wasn't supplied) — don't guess around it.

## Hard constraints — the token is read-only

**Never attempt a GitHub *write* of any kind.** Do not reply to a review comment, resolve a
conversation, push a commit or branch, approve, request changes, merge, label, or edit the PR. The
token structurally cannot do these, and this task must never assume a differently-scoped token
later. **Every fix you make surfaces only as krayt's `changes.patch`** (the normal output of any
krayt run) — a human reviews and applies it themselves, and is the one who responds on GitHub. Your
only interaction with GitHub is **reading**.

## Step 1 — identify the PR

The PR is the one associated with the currently checked-out branch. `gh pr view` with **no PR number
argument** auto-detects it from the branch:

```sh
gh pr view                       # sanity-check: does it resolve to the expected PR?
number=$(gh pr view --json number -q .number)
```

If `gh pr view` can't find a PR for the branch, stop and report that — don't invent a PR number.

## Step 2 — fetch the *review* comments (not issue comments)

This distinction is critical and easy to get wrong **silently**:

- `gh pr view --comments` shows only **issue/PR-level** comments (the conversation timeline).
  Copilot's — and most automated reviewers' — inline, per-line findings are **NOT** here.
- The inline, per-line findings are **review comments**, a *different* API:

  ```sh
  gh api "repos/{owner}/{repo}/pulls/${number}/comments" --paginate
  ```

  (`gh api` substitutes `{owner}`/`{repo}` from the repo automatically; `--paginate` gets them all.)

Using the wrong one means you see **zero relevant comments** and wrongly conclude there's nothing to
do. Fetch the **review** comments. Each entry gives you at least `path`, `line` (or
`original_line`), `body`, and `user.login` — enough to locate exactly what each comment is about.
It's fine to also skim `gh pr view --comments` for any human summary-level notes, but the review
comments are the payload.

## Step 3 — triage each comment against the real code

For **every** review comment, before touching anything:

1. **Open the actual current code** at the comment's `path` and `line`. Read it — plus enough
   surrounding context to understand it. Do **not** take the comment's framing at face value;
   automated reviewers routinely flag things that are already correct, misread control flow, or
   propose diffs that are themselves buggy.
2. **Decide:**
   - **Real issue** → fix it properly. If the comment includes a suggested diff, treat it as a
     *hint*, not gospel — implement the *correct* fix, which may differ from what was suggested. (On
     this repo's own PRs, at least one Copilot suggestion would itself have introduced a bug — it
     double-logged — and was implemented correctly rather than pasted verbatim. Expect this.)
   - **False positive** → change **nothing** for it, and record a **specific** reason it's wrong
     (what the code actually does, why the concern doesn't apply) — not a bare "disagree." The bar
     is: a human reading your reason can see the comment was wrong without re-deriving it.
3. Keep edits minimal and scoped to the finding — don't opportunistically refactor unrelated code.

If a run's tasks warrant it (e.g. you changed Go code), you can build/test/lint to confirm your
fixes with the toolchain this image ships (`go build ./...`, `go test ./...`, `golangci-lint run`) —
see the `krayt-dev` README. Regenerate proto if and only if you changed `internal/protocol/krayt.proto`.

## Step 4 — report

When every comment is triaged, write a summary to `/output/report.md` (the `krayt-dev` entrypoint
tees your final summary there; it becomes the run report's Notes). Include a table with one row per
review comment:

| # | File:line | Reviewer | Verdict | Reason |
|---|---|---|---|---|
| 1 | `internal/foo/bar.go:42` | copilot | ✅ Fixed | Off-by-one was real; loop skipped the last element. |
| 2 | `internal/foo/baz.go:88` | copilot | ❌ False positive | Claims the mutex isn't released, but the `defer unlock()` on line 80 covers this path. |

Verdict is **✅ Fixed** or **❌ False positive** (use a third row only for something genuinely
deferred, and say why). Then, matching the `## Output` convention in
`docs/ai-tasks/automate-vmimage-releases.md`, **suggest** (don't create — no commits, that's a
write) a short Conventional Commits message covering the accumulated fixes as a set. Type it to
match what actually changed (`fix:` for real bug fixes, etc.); if nothing was real, say so plainly
and suggest no commit.

## Done when

- Every review comment on the PR has a verdict (fixed / false positive / deferred-with-reason).
- Real issues are fixed and present in `changes.patch`; false positives left the code untouched.
- `/output/report.md` has the summary table and a suggested commit message (or an explicit "no
  changes needed").
- **No GitHub write was attempted** — the only GitHub interaction was reading.
