# Task: diagnose and fix a PR's failing GitHub Actions checks

You are running inside a krayt sandbox (the `krayt-dev` image) against a **local checkout of a pull
request's branch**, at `/workspace`. One or more **GitHub Actions checks are failing** on that PR.
Your job is to find out *which* checks failed and *why* — from the real run logs, not from
guesswork — **reproduce each failure locally where the sandbox can**, fix the underlying cause, and
hand the fixes back as krayt's own patch for a human to review and apply.

This is a generic, repeatable procedure — it is not tied to any one PR or any one workflow. It
assumes the PR's branch is already checked out (that's what `--repo .` from a local checkout of the
branch gives you).

## What you have

- `gh`, the GitHub CLI, is installed. It is authenticated **only if** a `GH_TOKEN` secret was
  supplied to this run. That token is **read-only** — a fine-grained PAT scoped to this repo with
  **Metadata / Contents / Pull requests / Actions: read** and nothing else. **`Actions: read` is
  the one this task needs that `fix-pr-review-comments.md` doesn't**: without it every `gh run`
  call 403s while `gh pr view` keeps working, so a token that looks fine can still be unable to
  read a single log line. If that's what you see, report it as a token-scope problem and stop —
  don't infer the failure from the diff.
- `api.github.com` must be in the run's `--allow` egress list for any `gh` call to work.
  **Log downloads are a second host**: `gh run view --log` / `--log-failed` fetch the archived log
  from a redirect to `objects.githubusercontent.com` (and, for some runs, an Azure blob host), so
  that must be allowed too, or log fetches fail with a network/egress error while `gh api` calls
  succeed. If `gh` calls fail on the network, stop and report **which host** was blocked (and, if
  auth also fails, that `GH_TOKEN` wasn't supplied) — don't guess around it. Step 3 gives a
  fallback that stays on `api.github.com`.
- **`$GH_TOKEN` holds a placeholder, not the real token, and that is correct.** No credential is
  ever placed inside this sandbox. The sandbox runtime substitutes the real value into your
  outbound requests at its own TLS boundary, and only for the hosts that secret is scoped to — so
  `gh` works normally while nothing in here can read the secret. Judge auth by whether a `gh` call
  actually succeeds, never by reading the token, and never try to "fix" it with `gh auth login`,
  which would replace a working credential with the placeholder you can see.
- **There is no git remote, by design — `gh` needs `GH_REPO`.** `/workspace` is cloned from a git
  bundle and krayt drops the `origin` that pointed at it, so `git remote -v` is empty. `gh`
  normally resolves *which repository* from that remote: without help, `gh pr view` and `gh run`
  fail with `no git remotes found` and `gh api "repos/{owner}/{repo}/..."` has nothing to
  substitute. The run's config supplies `GH_REPO=<owner>/<name>` in its `env:` block; every `gh`
  command below assumes it is set. If it is unset, **that** is the fix — report it and stop.
- **Do not go hunting in the environment.** If `gh` misbehaves, the cause is one of the things
  above — the token, an egress host, or `GH_REPO` — all of which you can check directly. Dumping
  `env` tells you nothing those do not, and it is the one place the credential placeholder appears
  in output.
- The `krayt-dev` toolchain: Go, `golangci-lint`, `buf`, `make`. That's enough to reproduce the
  `test`, `lint`, and `cross-compile-guest` jobs locally. It is **not** enough for the real-VM,
  Docker, or Nix jobs — see Step 4.

## Hard constraints — the token is read-only

**Never attempt a GitHub *write* of any kind.** In particular, **do not re-run a workflow**
(`gh run rerun`, `gh workflow run`, `gh run cancel`) — those are writes needing `Actions: write`,
and "just re-run it" is not a diagnosis. Do not push a commit or branch, comment, approve, request
changes, merge, label, or edit the PR. **Every fix you make surfaces only as krayt's
`changes.patch`** (the normal output of any krayt run) — a human reviews it, applies it, pushes,
and is the one who makes CI green. Your only interaction with GitHub is **reading**.

## Step 1 — identify the PR and its head commit

The PR is the one associated with the currently checked-out branch. `gh pr view` with **no PR number
argument** auto-detects it from the branch:

```sh
gh pr view                                        # sanity-check: does it resolve to the expected PR?
number=$(gh pr view --json number -q .number)
head=$(gh pr view --json headRefOid -q .headRefOid)
branch=$(git branch --show-current)
git log --oneline -1                              # does HEAD match $head?
```

If `gh pr view` can't find a PR for the branch, stop and report that — don't invent a PR number.

If your local `HEAD` is **not** `$head`, say so in the report and keep going: the logs describe a
commit you don't have, so anything you can't reproduce locally may already be fixed (or may be a
failure your checkout can't show). Reproduce what you can and flag the mismatch.

## Step 2 — list the checks and find the failing ones

```sh
gh pr checks                                      # human-readable: name, state, link per check
gh pr checks --json name,state,bucket,link,workflow,description
```

Note `gh pr checks` **exits non-zero when any check is failing** — that's its normal signal, not an
error. Don't run it under `set -e` without guarding it (`gh pr checks || true`).

Then get the workflow runs for this branch, which is what carries the run IDs you need:

```sh
gh run list --branch "$branch" --json databaseId,name,workflowName,headSha,status,conclusion,event
```

Pick the runs whose `headSha` is `$head` (older runs describe superseded commits) and whose
`conclusion` is `failure`. Then list that run's jobs:

```sh
gh run view <run-id> --json jobs \
  -q '.jobs[] | {name, conclusion, databaseId}'
```

A run can be red because of **one** matrix leg — `build + test (macos-latest)` failing while
`ubuntu-latest` passes is a completely different bug from both failing. Record which legs failed;
that distinction usually *is* the diagnosis (see Step 4).

Also distinguish these, because two of them are not yours to fix:

- **`failure`** — a step genuinely failed. This is the payload.
- **`cancelled`** — usually a newer push superseding the run via `cancel-in-progress`. Not a defect;
  say so and move on.
- **`skipped`** — path filters, or a gate job whose condition was false. Every image workflow here
  is path-scoped, so most PRs legitimately skip most of them. Not a failure at all. Do **not**
  "fix" a skipped check.
- **`startup_failure`** — the workflow file itself doesn't parse, or a referenced action/secret is
  missing. That's a workflow bug, and it's in the diff you can edit.

## Step 3 — read the actual failure output

Get the failing steps' log, not the whole run:

```sh
gh run view <run-id> --log-failed                    # only the failed steps' lines
gh run view <run-id> --job <job-id> --log            # one job's full log, when you need context
```

`--log-failed` is the right default: a full CI log is mostly setup noise and will bury the four
lines that matter. Read far enough to get the **first** real error — a Go build failure cascades
into dozens of downstream errors, and fixing the last one on screen fixes nothing.

**If the log download is blocked by egress** (see "What you have"), fall back to endpoints that
stay on `api.github.com`:

```sh
gh api "repos/{owner}/{repo}/commits/${head}/check-runs" --paginate \
  -q '.check_runs[] | select(.conclusion=="failure") | {name, id, output: .output.summary}'
gh api "repos/{owner}/{repo}/check-runs/<check-run-id>/annotations" \
  -q '.[] | {path, start_line, annotation_level, message}'
```

Annotations carry `golangci-lint` findings and compiler errors with file and line, so they're often
enough on their own. They are **not** enough for a `go test` assertion failure or a shell script's
output — say plainly in the report when annotations were all you could get.

Quote the actual error text in your report. A claim about why CI failed that you can't back with a
log line is worse than saying you couldn't read the log.

## Step 4 — reproduce locally, then fix

For **every** failing job, before touching anything: work out what it actually runs, and run that
same thing here. The mapping for this repo's `ci.yml`:

| Failing job | Reproduce with |
|---|---|
| `build + test (ubuntu-latest)` | `make guest-bins && go build ./... && go vet ./... && go test -race ./...` |
| the entrypoint step of that job | `bash hack/test-entrypoint-credentials.sh` |
| `cross-compile guest (linux/arm64)` | `GOOS=linux GOARCH=arm64 go build ./...` |
| `golangci-lint (ubuntu-latest)` | `golangci-lint run` |

`make guest-bins` matters: `internal/sandbox/guestbin`'s embed is empty without it, and
`guestbin_test.go`'s `TestEmbeddedBinariesPresentInCI` is gated on the `CI` env var — so a failure
there in CI can be invisible locally until you build the guest binaries first.

**What this sandbox cannot reproduce** — these are §14 "needs real hardware / CI" categories, and
being unable to run them is a correct outcome, not a failure of yours:

- **Anything `(macos-latest)`** — you're on Linux. The recurring macOS-only cause in this repo is
  **bash 3.2**: `hack/*.sh` and the agent entrypoints must avoid `${arr[@]}` on empty arrays under
  `set -u`, `declare -A`, `mapfile`/`readarray`, `${var^^}`, and `&>>`. If exactly the macOS leg is
  red and the Linux one is green on a shell change, that's your first suspect — inspect the script,
  fix it to bash 3.2 syntax, and say the fix is unverified on macOS.
- **`vm-image`** — a Nix build on a Linux builder. Note this one is **known-broken and being
  retired**: `images/flake.nix` still builds the deleted `cmd/krayt-agent`/`cmd/krayt-vsock-forward`
  (`KRAYT_SPEC.md` §14, task 7), and `docs/ai-tasks/retire-vm-image-pipeline.md` deletes the whole
  pipeline. Don't try to repair it — record it as known-broken and move on.
- **`dev-image` / `agent-images` / `probe-images`** — Docker buildx image builds.

For those: read the log, identify the cause as precisely as the log supports, fix it if the cause is
in code you can see (a Dockerfile typo, a wrong path in a workflow, a bad `nix` attribute), and mark
the fix **unverified** with the exact command a human should run to confirm. Do not claim a fix you
couldn't execute.

Then, for each failure:

1. **Decide what kind of failure it is.**
   - **A real defect in the PR's code** → fix it properly, minimally, at the root cause.
   - **A real defect in a test** (the test is wrong, not the code) → fix the test, and say
     specifically why the test was wrong.
   - **Infrastructure or flake** — a runner network hiccup, a GHCR rate limit, a registry 5xx, a
     timeout under load, a race that passes on re-run. Change **nothing**, and record a **specific**
     reason it's a flake (which step, what the error was, why it's environmental). The bar is: a
     human reading your reason can see it wasn't the PR's fault without re-deriving it.
   - **Pre-existing on `main`** → check whether the failure also reproduces at the merge base
     (`git merge-base HEAD origin/main`); if it does, it isn't this PR's doing. Say so, and don't
     expand the PR to fix it unless it's trivially in scope.
2. **Never make CI green by hiding the failure.** Do not skip, `t.Skip`, delete, or weaken a test;
   do not loosen an assertion; do not remove a step, add `continue-on-error`, or narrow a path
   filter in a workflow so the job stops running. If you believe a check is genuinely wrong to
   exist, say that in the report and leave it running.
3. **Re-run the local command** after your fix, and paste the result. A fix you didn't re-verify
   locally, when you *could* have, isn't done.
4. Keep edits scoped to the failure — don't opportunistically refactor unrelated code.

Regenerate proto (`make proto`) if and only if you changed `internal/protocol/krayt.proto`.

## Step 5 — report

When every failing check is accounted for, put your summary in your **final response** (plain
stdout) — do **not** use a file-write tool to create or edit `/output/report.md` yourself. The
`krayt-dev` entrypoint already pipes your entire session's stdout through `tee /output/report.md`;
writing the file directly races that pipe (two independent writers on the same path) and can
truncate or corrupt it. Your final response becomes the run report's Notes automatically.

Include a table with one row per failing check:

| # | Check (job / matrix leg) | Root cause | Verdict | Evidence |
|---|---|---|---|---|
| 1 | `build + test (ubuntu-latest)` | `internal/foo` didn't handle an empty slice; `TestBar` panicked | ✅ Fixed | Log: `panic: runtime error: index out of range [0] with length 0`; `go test -race ./internal/foo` now passes locally. |
| 2 | `build + test (macos-latest)` | `hack/foo.sh` uses `mapfile`, absent in bash 3.2 | ⚠️ Fixed, unverified | Log: `foo.sh: line 12: mapfile: command not found`. Rewritten with a `while read` loop; needs a macOS runner to confirm. |
| 3 | `vm-image (aarch64-linux)` | `images/flake.nix` builds `cmd/krayt-agent`, deleted by the msb cutover | ❌ Known-broken, being retired | Log: `path 'cmd/krayt-agent' does not exist`. Pipeline is deleted wholesale by `retire-vm-image-pipeline.md`; no fix here. |

Verdict is **✅ Fixed** (reproduced locally and re-verified), **⚠️ Fixed, unverified** (cause
identified and fixed, but the job can't run in this sandbox — name the command a human must run),
**❌ Flake / infra**, or **❌ Pre-existing on main**. Use another row only for something genuinely
deferred, and say why.

Then, matching the `## Output` convention in `docs/ai-tasks/automate-vmimage-releases.md`,
**suggest** (don't create — no commits, that's a write) a short Conventional Commits message
covering the accumulated fixes as a set. Type it to match what actually changed (`fix:` for a real
bug fix, `test:` if only tests changed, `ci:` for workflow files); if nothing was a real defect,
say so plainly and suggest no commit.

## Done when

- Every failing check on the PR's head commit has a root cause and a verdict, each backed by quoted
  log output.
- Failures reproducible in this sandbox are fixed **and re-verified locally**, and the fixes are in
  `changes.patch`; flakes and pre-existing-on-`main` failures left the code untouched.
- Failures needing hardware/CI are diagnosed as far as the log allows and marked unverified, with
  the exact command a human should run.
- No test was skipped, weakened, or deleted, and no workflow step was disabled, to make a check pass.
- `/output/report.md` has the summary table and a suggested commit message (or an explicit "no
  changes needed").
- **No GitHub write was attempted** — no re-runs, no comments, no pushes; the only GitHub
  interaction was reading.
