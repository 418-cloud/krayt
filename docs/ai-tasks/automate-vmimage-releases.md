# Task: automate vmimage release-candidate publishing + a deliberate graduation step

**Read `CLAUDE.md`, `RELEASING.md`, and `KRAYT_SPEC.md` §11 (image/release) first.** Give a short
plan (the tag-computation script's exact logic, the two workflow files) and proceed — this is a
contained addition (one small script, two workflow files, a `RELEASING.md` rewrite), not a
repo-wide change. Still stop and ask if you hit something this doc doesn't cover.

## Background — why this ended up NOT being a release-please extension

The original idea was to extend release-please (already used for the CLI) to also manage vmimage
releases. That was explored in depth and abandoned — recorded here so the reasoning isn't lost and
nobody re-proposes it without knowing why:

- release-please's manifest mode scopes each tracked package to exactly **one folder**, and only
  attributes commits to a package if they touch files under that folder (verified against
  release-please's own docs). "Files that require a vmimage rebuild" span `images/**`,
  `internal/guest/**`, `cmd/krayt-agent/**`, `cmd/krayt-proxy/**`, `cmd/krayt-ask/**` — five
  non-contiguous locations.
- Consolidating them under one folder (e.g. `images/guest/`) was the first plan, but it doesn't
  work: `internal/guest`'s root package (`service.go`, `network.go`, `runner.go` —
  `guest.NewService`, `guest.Service`, `guest.RunConfig`, `guest.ContainerAskSocket`, etc.) is
  imported **outside** the guest tree — most importantly by `internal/provider/fake`, which runs
  the real `guest.NewService()` in-process as the host-side test fixture for the entire
  fake-provider test strategy (`CLAUDE.md`: "Test the core against the `fakeProvider`; don't
  require a real VM for unit tests"), plus several `internal/orchestrator`/`internal/cli` test
  files and one production file (`internal/cli/run.go`, for `guest.ContainerAskSocket`). Moving
  `internal/guest` under a path Go's `internal/` visibility rule would lock down to
  `images/guest/` would break all of those — verified by grep, not assumed. (The *subpackages* —
  `internal/guest/proxy`, `/ask`, `/runner` — are cleanly guest-only and could move on their own,
  but moving only those still leaves the actively-changed root package outside any single-folder
  scope, so it doesn't actually solve the problem either.)
- Without a full move, there's no way to make release-please's own commit-scanning see commits
  under `internal/guest/**` etc. as relevant to an `images`-path-scoped package, short of
  reimplementing chunks of its version-computation engine (forcing versions via `release-as`,
  faking relevant commits, etc.) — which throws away the part of release-please that's actually
  valuable here (conventional-commit-driven semver inference) while keeping all the fragility.

**Decision: don't use release-please for the vmimage at all.** It stays exactly as it is today,
exclusively for the CLI (`.` package, unchanged). No file moves, no `internal/`-visibility
restructuring, no multi-package `release-please-config.json`. Instead, build a small, independent
tagging mechanism purpose-built for the RC → graduate lifecycle this actually needs.

## Decisions already made (do not re-litigate)

1. **Nothing moves.** `internal/guest/*`, `cmd/krayt-agent`, `cmd/krayt-proxy`, `cmd/krayt-ask`,
   and `images/*.nix` all stay exactly where they are.

2. **RC publishing triggers on both PR pushes and pushes to `main`** — deliberately, not just one
   or the other. A new workflow (suggest `.github/workflows/vmimage-rc.yml`) triggers on:
   - `pull_request` (any type that updates the head — default `opened`/`synchronize` is fine),
     path-filtered to the broad set: `images/**`, `internal/guest/**`, `cmd/krayt-agent/**`,
     `cmd/krayt-proxy/**`, `cmd/krayt-ask/**`.
   - `push: branches: [main]`, same path filter.

   Both cases do exactly the same thing: compute and push the next `vmimage-vX.Y.Z-rc.N` tag from
   the current commit. There's no special "this one's from main" behavior — a push to `main` just
   means the RC candidate is now also reachable from mainline history, nothing more. This is a
   deliberate choice, not an oversight: the tempting alternative — auto-publishing a *clean*,
   non-RC release the moment something lands on `main` — was considered and rejected. Merging a PR
   is a code-review event, not "a human ran a real boot test on hardware," and collapsing those two
   would silently reopen the exact risk the RC/graduate split exists to prevent (an unverified image
   becoming pin-eligible-looking). See decision 4.

   Building from PR branches (as opposed to only post-merge) is the actual improvement over the
   earlier plan: a reviewer can pull and boot-test the artifact for the *specific proposed change*
   before approving, not only after it's already on `main`.

   One GitHub Actions behavior worth knowing, not necessarily worth solving now: `pull_request`-
   triggered workflows from **forks** don't get repo secrets by default (a deliberate GitHub
   safeguard), so the GHCR-push step in this workflow will only work for branches within this repo.
   If this project ever takes fork PRs that touch these paths, that needs separate handling
   (e.g. gate the actual publish on `github.event.pull_request.head.repo.fork == false`, or require
   a maintainer to re-run after review). Note it in `RELEASING.md`; don't build for it now unless
   you find it's already needed.

3. **Tag computation logic** (suggest a dedicated script, `hack/next-vmimage-tag.sh`, called from
   the workflow — this repo's convention for anything more than a one-liner, per
   `hack/linux-net-setup.sh`):
   - Read the latest `vmimage-v*` tag reachable in history (`git tag -l 'vmimage-v*' --sort=-v:refname | head -1`).
   - If it's a release candidate (`vmimage-vX.Y.Z-rc.N`): bump to `vmimage-vX.Y.Z-rc.(N+1)`,
     targeting the *same* `X.Y.Z`.
   - If it's a clean, graduated tag (`vmimage-vX.Y.Z`, no `-rc` suffix) or there is no prior tag:
     start a new series at the next patch — `vmimage-vX.Y.(Z+1)-rc.1`. This is deliberately the
     simplest possible default (no conventional-commit analysis, no attempt to infer feat/fix/
     breaking-change semver bumps from commits outside any single scoped path — that's exactly the
     thing release-please could do and this mechanism can't, per Background). A bigger bump than
     patch is always available as a free choice at graduation time (decision 4) — the RC series'
     own number is just a provisional label, not a commitment to a final version.
   - Push the computed tag. This alone triggers `.github/workflows/image.yml`'s existing
     `push: tags: ["vmimage-v*"]` publish job — **no changes needed to `image.yml`**; verify its
     glob matches an RC-suffixed tag (it should — it's a plain prefix match against the whole tag
     string) rather than assume it.

   **Concurrency**: give the tag-computation step (or the whole job) a GitHub Actions
   `concurrency:` group — e.g. `group: vmimage-rc-tag` with no `cancel-in-progress` (so overlapping
   triggers *queue* rather than race or cancel each other) — not scoped per-ref like `image.yml`'s
   own group, but global, since "read the last tag, then push the next one" is a read-then-write
   over shared, repo-wide state (the tag list) and two concurrent runs (e.g. a PR push and a `main`
   push landing seconds apart) computing the same "next" tag would collide. A failed/duplicate tag
   push is a safe failure (git refuses to overwrite an existing tag), but a global concurrency
   group avoids the wasted, confusing CI failure entirely.

   It's fine and expected for RC numbers to end up "orphaned" (a PR gets abandoned or rebased after
   its RC published) — these are throwaway candidate artifacts by design, not a bug to engineer
   around.

4. **Graduation is a separate, always-manual trigger — regardless of whether the RC came from a PR
   or `main`.** Suggest a dedicated workflow (`.github/workflows/vmimage-graduate.yml`, its own
   file rather than a job in `vmimage-rc.yml`, so it shows up as an obviously-named, deliberate
   choice in the Actions "Run workflow" UI), `workflow_dispatch`-only, with two required inputs:
   - `rc_tag` — the exact RC tag being graduated (e.g. `vmimage-v0.5.1-rc.3`). Required, no
     default — the human must name the specific candidate they boot-tested.
   - `version` — the clean version to publish as (e.g. `0.5.1`, or `0.6.0` if the accumulated
     changes warrant more than the RC series' own provisional patch bump). Required, no default;
     this is exactly the human judgment call release-please's conventional-commit inference would
     otherwise make, made explicitly instead.

   The job resolves `rc_tag`'s commit SHA and creates `vmimage-v<version>` pointing at **that same
   commit** — not "whatever `main`'s tip is right now." This is the property that makes the
   boot-test meaningful: the artifact `image.yml` publishes for the graduated tag must be built
   from the exact source the human already verified, not something that drifted while they were
   testing. (Since the Nix build is meant to be reproducible, the graduated artifact's digest
   *should* end up identical to the already-tested RC's digest — note this expectation in
   `RELEASING.md`; if it ever doesn't hold, that's a reproducibility gap worth investigating, not
   something to route around.)

   Graduating an RC that only exists on an unmerged PR branch is possible but not the expected
   flow — call out in `RELEASING.md` that the RC being graduated should normally already be part of
   `main`'s history.

   Pinning `internal/vmimage/pinned.go` remains completely unchanged by any of this: still fully
   manual, still requires an actual boot test on real hardware, still committed as `fix:` so the
   next CLI release ships the new pin. Only a graduated (`vmimage-vX.Y.Z`, no `-rc`) digest may ever
   be pinned.

5. **Optional, not required for "Done when":** since the Linux/firecracker boot test has already
   been proven to run on a GitHub-hosted runner in this repo's `integration-linux` CI job (KVM
   access via the udev-rule workaround, the Docker `DOCKER-USER` fix, all already in `ci.yml`), the
   `vmimage-rc.yml` workflow could additionally auto-boot-test each RC's Linux/amd64 variant right
   after publishing it, giving a stronger starting signal before any human looks at it. This is a
   genuine nice-to-have, not a requirement — the macOS/vfkit side can never be automated this way
   (no such hardware in any CI runner), so a human boot test stays necessary regardless. Leave it as
   a noted follow-up unless it's cheap to add alongside the rest of this task.

## Deliverables

1. `hack/next-vmimage-tag.sh` (or equivalent) implementing decision 3's logic. Give it a `--dry-run`
   or similar so it's testable without actually pushing a tag.
2. `.github/workflows/vmimage-rc.yml` — triggers, concurrency group, and tag-push per decisions 2–3.
3. `.github/workflows/vmimage-graduate.yml` — `workflow_dispatch` inputs and same-commit re-tagging
   per decision 4.
4. `RELEASING.md` — rewrite "Releasing a new VM image" to describe the new flow end to end: land a
   change under any of the watched paths → PR push (or `main` push) auto-publishes the next
   `-rc.N` → boot-test that specific RC (`vm-image` workflow build, or `nix build ./images#vmImage`
   + `TestBootHello` locally — unchanged from today) → if good, run `vmimage-graduate.yml` with the
   verified `rc_tag` and a chosen `version` → clean tag published → pin **that** digest into
   `internal/vmimage/pinned.go` (unchanged, still manual, still `fix:`) → next CLI release ships the
   pin. Include the fork-PR secrets caveat from decision 2 and the same-commit note from decision 4.

   Also make explicit (this doesn't change the existing convention, just states *why* it matters
   more now): guest/image-only commits stay `chore:` exactly as today — release-please's CLI
   package still watches the whole repo with no path exclusions, so a guest commit typed `feat:`/
   `fix:` would count toward the CLI's next version/changelog **the moment it lands**, not when the
   corresponding vmimage is actually graduated and pinned. Those are now fully decoupled events (that's
   the point of the RC/graduate split), so a `feat:`/`fix:` on the raw guest commit can land in an
   earlier CLI release than the one that actually ships the pin — a changelog entry describing
   something not yet true. Instead, write the **pin commit** itself descriptively: its `fix:`
   message (and body, if useful — e.g. summarizing the graduated RC's changes or linking its tag)
   is what should carry the human-readable "what's new in this vmimage" content, since that's the
   one commit guaranteed to land in the same CLI release that ships the new digest.
5. `docs/ai-tasks/README.md` — add this task's row once done.

## Verify

What you can do yourself:
```sh
bash -n hack/next-vmimage-tag.sh
# exercise the tag-bumping logic against fabricated tag lists (rc -> rc+1, stable -> new patch-rc.1,
# no prior tag at all) without touching the real repo's tags — e.g. point it at a scratch git repo
# with synthetic tags, or add a --dry-run mode that takes the "last tag" as an argument for testing
```
Validate both workflow YAML files parse and their `paths:` filters match the same set used
elsewhere (`ci.yml`'s `changes` job, `image.yml`'s trigger) for consistency — a deliberate mismatch
is fine if you have a reason, an accidental one isn't.

What you cannot verify yourself, and must log to `HUMAN_TODO.md` rather than assume:
- **A real PR push actually triggers `vmimage-rc.yml` and publishes a working RC tag** — needs a
  real PR against GitHub.
- **A real `vmimage-graduate.yml` dispatch actually re-tags the right commit and `image.yml`
  publishes it correctly.**
- **Whether concurrent PRs touching these paths behave as expected under the concurrency group** —
  plausible to reason about, not something to claim proven without seeing it happen.
- Never fabricate any of the above.

## Done when

- `hack/next-vmimage-tag.sh` correctly computes rc→rc+1, stable→next-patch-rc.1, and no-prior-tag
  cases, verified by whatever local exercising is possible without a real push.
- `vmimage-rc.yml` triggers on both PR pushes and `main` pushes to the correct path set, with a
  concurrency group preventing racing tag computations.
- `vmimage-graduate.yml` takes `rc_tag` + `version`, tags the RC's exact commit, and requires no
  other input.
- `image.yml` is unchanged (its existing trigger already covers both tag shapes) — confirmed, not
  assumed.
- `RELEASING.md` accurately describes the full new flow.
- `HUMAN_TODO.md` has honest entries for the things in Verify that need a real GitHub Actions run
  to confirm.

## Constraints

- Never let anything except `vmimage-graduate.yml`, run manually by a human, produce a clean
  (non-`-rc.`) `vmimage-v*` tag. The RC workflow only ever produces `-rc.N` tags, unconditionally,
  regardless of which branch/event triggered it.
- `internal/vmimage/pinned.go` stays a fully manual, hardware-boot-tested step — this task changes
  nothing about that, only about how candidates get proposed and published before it.
- Don't move any files. Don't touch `release-please-config.json` or `.release-please-manifest.json`
  — the vmimage isn't a release-please package, by design (see Background).
- Don't build the optional Linux auto-boot-test (decision 5) unless it's cheap to add alongside the
  required deliverables — it's explicitly a nice-to-have, not a blocker.

## Output

When this task is done, output a suggested branch name and commit message (don't create the
branch or commit yourself unless separately asked to) — kebab-case branch name describing the
outcome (matching this file's own naming convention, e.g. `automate-vmimage-rc-releases`), and a
Conventional Commits message for the change set as a whole. This is tooling/CI, not a CLI-facing
`feat:`/`fix:` — per decision 4/deliverable 4's own convention, type it `chore:`.
