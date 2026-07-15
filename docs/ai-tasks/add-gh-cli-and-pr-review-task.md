# Task: add `gh` CLI + a read-only PR token to `krayt-dev`, and a reusable PR-review-comment task

**Read `CLAUDE.md`, `hack/krayt-dev/README.md`, `hack/krayt-dev/Dockerfile`/`entrypoint.sh`, and
`KRAYT_SPEC.md` §6.8 (secrets) + §8.2 (container contract) first.** Medium-sized: one image change
plus one new reusable prompt file. Give a short plan and proceed; stop and ask if something here
conflicts with what you find in those files.

## Background

`krayt-dev` (the dogfooding agent image, `hack/krayt-dev/`) already gives an in-sandbox Claude Code
agent krayt's own dev toolchain (Go, `golangci-lint`, `protoc`/`buf`, `oras`) so it can build/test/
lint/regenerate proto before handing back a patch. It has no way to read a GitHub PR's review
comments today. This task adds that, plus a reusable task prompt (not a one-off — see
`docs/common-tasks/`, a new sibling to `docs/ai-tasks/` for exactly this kind of thing) that
instructs an agent to triage and fix real PR review-comment findings (e.g. from GitHub Copilot's
automated review), modeled on the same discipline used to triage the Copilot findings on this
repo's own PRs: verify each comment against the actual code before touching anything, fix only
what's real, state plainly why a comment is wrong when it's a false positive, never blindly comply.

## Decisions already made

1. **`gh` CLI, installed the same way `protoc` is** (`hack/krayt-dev/Dockerfile` already has this
   exception pattern for a tool with no good `go install` story): fetched as a prebuilt release
   tarball, `TARGETARCH`-aware. Verified against the real release assets (`api.github.com` reachable
   from this sandbox, not guessed) — asset names are `gh_<version>_linux_<arch>.tar.gz` where
   `<arch>` is `amd64`/`arm64` **matching Docker's `TARGETARCH` values exactly**, no translation
   needed (unlike `protoc`'s `x86_64`/`aarch_64` mapping). The binary is at
   `gh_<version>_linux_<arch>/bin/gh` inside the tarball. Add `ARG GH_CLI_VERSION=2.96.0` (or
   whatever's current when you implement this — verify against
   `https://api.github.com/repos/cli/cli/releases/latest` rather than trust this number) next to
   the other tool `ARG`s, following `protoc`'s exact install-step shape:
   ```dockerfile
   RUN set -eu; \
       curl -fsSL -o /tmp/gh.tar.gz \
         "https://github.com/cli/cli/releases/download/v${GH_CLI_VERSION}/gh_${GH_CLI_VERSION}_linux_${TARGETARCH}.tar.gz" \
    && tar -xzf /tmp/gh.tar.gz -C /tmp \
    && install -m 0755 "/tmp/gh_${GH_CLI_VERSION}_linux_${TARGETARCH}/bin/gh" /usr/local/bin/gh \
    && rm -rf /tmp/gh.tar.gz "/tmp/gh_${GH_CLI_VERSION}_linux_${TARGETARCH}"
   ```
   Add a matching `renovate.json` custom manager entry (same shape as the existing `PROTOC_VERSION`
   one, since `gh` is also not `go install`-able and needs its own tracker):
   ```jsonc
   {
     "customType": "regex",
     "description": "hack/krayt-dev/Dockerfile: gh CLI, fetched as a release tarball (no go install story worth relying on)",
     "managerFilePatterns": ["^hack/krayt-dev/Dockerfile$"],
     "matchStrings": ["ARG GH_CLI_VERSION=(?<currentValue>.*?)\\n"],
     "datasourceTemplate": "github-releases",
     "packageNameTemplate": "cli/cli",
     "extractVersionTemplate": "^v(?<version>.*)$"
   }
   ```

2. **A new secret, `GH_TOKEN`, read from `/run/secrets/GH_TOKEN` exactly like the existing model
   credentials — but optional, not required.** Unlike `ANTHROPIC_API_KEY`/etc. (the entrypoint
   exits `78` if none of those are present, since Claude Code cannot run at all without one),
   `krayt-dev` is used for lots of things that have nothing to do with GitHub — the entrypoint must
   NOT fail if `GH_TOKEN` is absent. Mirror the existing credential-export loop's `if [ -f ... ]`
   shape, but make it its own independent, non-fatal step:
   ```sh
   if [ -f "$SECRETS_DIR/GH_TOKEN" ]; then
     gh auth login --with-token < "$SECRETS_DIR/GH_TOKEN"
     echo "[krayt-dev] authenticated gh via GH_TOKEN"
   else
     echo "[krayt-dev] no GH_TOKEN in $SECRETS_DIR — gh commands will be unauthenticated"
   fi
   ```
   `gh auth login --with-token` is `gh`'s own documented non-interactive auth path — it writes the
   token into `~/.config/gh/hosts.yml` (the agent user's home, uid 1000), which is outside
   `/workspace`, so it cannot leak into `changes.patch` (which only ever diffs `/workspace`) —
   confirm this rather than assume it (`echo $HOME`, confirm it's `/home/agent`, confirm
   `/workspace` is a different mount). No new redaction code is needed either: `secrets.Redactor`
   (`internal/secrets`) already redacts every value from the secrets file uniformly, not by an
   allowlisted set of key names, so `GH_TOKEN`'s value gets the same log/report.md/question
   redaction coverage as `ANTHROPIC_API_KEY` for free — verify this is still true by reading
   `internal/secrets/secrets.go`'s `NewRedactor`, don't just take this doc's word for it.

3. **Token scope: metadata + contents + pull-requests, read-only, on the krayt repo specifically —
   document this in `hack/krayt-dev/README.md`** (same place the other secrets' contracts already
   live), **and state it again, operationally, in the reusable task prompt itself** (deliverable
   below) — an agent running with this token needs to know at the moment it matters that it
   *cannot* comment, push, approve, or merge via `gh`/the GitHub API, only read. Not `CLAUDE.md` —
   that file is the working agreement for developing krayt itself; this is a downstream image's
   runtime secret contract, a different audience.

4. **The reusable task lives at `docs/common-tasks/fix-pr-review-comments.md`**, a new sibling
   directory to `docs/ai-tasks/`. Give it its own `docs/common-tasks/README.md` (mirroring
   `docs/ai-tasks/README.md`'s own shape) explaining the distinction: `docs/ai-tasks/*.md` are
   one-off tasks that build a specific krayt feature; `docs/common-tasks/*.md` are generic,
   repeatedly-runnable operating procedures, invoked the same way
   (`krayt run --task docs/common-tasks/<file>.md --repo .`) but not tied to a single change.

5. **Manual/on-demand only — no CI automation.** This task builds the reusable prompt file, not a
   trigger for it. Auto-running it whenever Copilot posts a review would need a persistently
   reachable krayt host from GitHub Actions, webhook handling, and its own credential/security
   design — a distinct, much bigger task, explicitly out of scope here. The expected usage is a
   human checking out the PR's branch locally and running the task by hand.

## Deliverables

1. `hack/krayt-dev/Dockerfile` — `gh` CLI per decision 1.
2. `renovate.json` — the new custom manager entry per decision 1.
3. `hack/krayt-dev/entrypoint.sh` — optional `GH_TOKEN` → `gh auth login` per decision 2.
4. `hack/krayt-dev/README.md` — document `gh` in "What's in the image", `GH_TOKEN` alongside the
   existing secrets section (name, optional, scope per decision 3), and an egress note matching the
   existing style (`api.github.com` needs to be in a run's `--allow` list for any `gh` command to
   work — same pattern as the existing Nix/proto egress callouts).
5. `docs/common-tasks/README.md` — new index page per decision 4.
6. `docs/common-tasks/fix-pr-review-comments.md` — the reusable task prompt. Self-contained (a
   fresh agent with no other context must be able to act on it, same bar `docs/ai-tasks/README.md`
   sets). Cover, at minimum:
   - **Identify the PR from the checked-out branch** — `gh pr view` with no arguments auto-detects
     the PR associated with the current branch; that's the intended invocation (`--repo .` from a
     local checkout of the PR's branch), not a PR number passed in some other way.
   - **Fetch *review* comments specifically, not issue comments.** `gh pr view --comments` only
     shows PR-level/issue comments — Copilot's (and most automated reviewers') inline, per-line
     findings are *review* comments, a different API:
     `gh api repos/{owner}/{repo}/pulls/{number}/comments`. Getting this wrong means silently
     seeing zero relevant comments. State this distinction explicitly in the prompt; it's the kind
     of thing that's easy to get wrong silently.
   - **For each comment: read the actual current code at the referenced file/line before deciding
     anything** — don't take the comment's framing at face value. Fix it only if it's a real issue;
     if it's a false positive, say specifically why (not just "disagree") and move on without
     changing anything for that one. This mirrors exactly how Copilot review comments on this
     repo's own PRs have been triaged — most were real and fixed, at least one suggested diff had a
     bug of its own (would have double-logged) and was implemented correctly instead of copied
     verbatim.
   - **The token is read-only.** Never attempt to reply to a comment, push, approve, or merge via
     `gh`. All fixes surface only as krayt's own `changes.patch` — same as every other krayt run —
     for a human to review and apply themselves.
   - **When every comment is triaged**, output (to `/output/report.md`, same mechanism
     `krayt-dev`'s entrypoint already tees Claude's summary into) a summary table: each comment,
     verdict (fixed / false positive), and a one-line reason — plus a suggested short commit
     message covering the accumulated fixes as a set, matching the `## Output` convention already
     used in `docs/ai-tasks/automate-vmimage-releases.md` (suggest, don't create, unless separately
     asked).
7. `README.md` (repo root) — only if you find the repo-orientation table already lists
   `docs/ai-tasks`; if it doesn't (it may not), don't add `docs/common-tasks` there either — stay
   consistent with whatever's already true rather than introducing a new inconsistency.

## Verify

What you can do yourself:
```sh
bash -n hack/krayt-dev/entrypoint.sh
python3 -c "import json; json.load(open('renovate.json'))"   # confirm renovate.json still parses
```
Attempt a local single-arch build if this sandbox has Docker/buildx (per `hack/krayt-dev/README.md`'s
own "Build + publish" section — `docker buildx build --platform linux/arm64 -f hack/krayt-dev/Dockerfile -t krayt-dev:local .`
from the repo root). If it doesn't, say so and hand off rather than assuming the Dockerfile is
correct.

What you cannot verify yourself, and must log to `HUMAN_TODO.md` rather than assume:
- **The image actually builds** (if Docker isn't available in your sandbox).
- **A real fine-grained PAT with exactly metadata+contents+pull-requests read scope actually
  authenticates `gh` and can read PR review comments** — needs a real token and a real PR.
- **A real run of `docs/common-tasks/fix-pr-review-comments.md` against a real PR** — triages
  comments sensibly, fixes only real issues, produces a correct patch and summary. This needs an
  actual krayt run with live credentials, not something provable statically.
- Never fabricate any of the above.

## Done when

- `hack/krayt-dev/Dockerfile` builds `gh` for both `linux/amd64` and `linux/arm64` (confirmed, or
  handed off per Verify).
- `entrypoint.sh` authenticates `gh` when `GH_TOKEN` is present and does not fail when it's absent.
- `hack/krayt-dev/README.md` documents `gh`, the `GH_TOKEN` secret (name, optional, scope), and the
  `api.github.com` egress requirement.
- `docs/common-tasks/README.md` and `docs/common-tasks/fix-pr-review-comments.md` exist, and the
  latter is self-contained per decision 4/deliverable 6.
- `renovate.json` still parses as valid JSON and includes the new `GH_CLI_VERSION` manager.
- `HUMAN_TODO.md` has honest entries for whatever in Verify needs a real build/token/run to confirm.

## Constraints

- `GH_TOKEN` must stay optional. Never make the entrypoint exit non-zero because it's absent.
- Never let the reusable task (or anything else added here) attempt a GitHub *write* operation —
  comment, push, approve, merge, label, anything — the token this is designed around structurally
  cannot do these, and the design should not assume a differently-scoped token later either.
- Don't wire any CI automation to auto-trigger the reusable task (decision 5) — manual invocation
  only, for this task.
- Don't add `GH_TOKEN` handling to any *other* image (`hack/claude-code`, the probes, etc.) — this
  is `krayt-dev`-specific.

## Output

When this task is done, output a suggested branch name and commit message (don't create the branch
or commit yourself unless separately asked to) — kebab-case branch name describing the outcome, and
a Conventional Commits message for the change set as a whole, typed `chore:` (this is dev tooling,
not a CLI-facing `feat:`/`fix:` — `krayt-dev` ships as its own image via `dev-image.yml`, entirely
separate from both the `krayt` binary's own release-please package and the pinned vmimage).
