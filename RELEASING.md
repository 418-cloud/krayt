# Releasing krayt

Two independently-versioned artifacts, tied together by a pinned image digest:

- **CLI** (`krayt`) — versioned `vX.Y.Z`, automated by [release-please]. This is what most
  releases are.
- **VM base image** (`krayt-vmimage`) — versioned on its own `vmimage-v*` tags. Candidates
  (`-rc.N`) auto-publish from PR/`main` pushes; graduating one to a clean tag stays a manual step
  because it must be **boot-tested on Apple-Silicon hardware** first (§11.6). Changes rarely (only
  when the guest-agent, protocol, or image flake changes).

The CLI pins the image by **digest** in `internal/vmimage/pinned.go`, so the two versions don't
have to match — the digest is the contract. `krayt version` prints both.

## Cutting a CLI release (the common case)

release-please watches `main` and keeps a **"release PR"** open that bumps the version + updates
`CHANGELOG.md` from your Conventional Commits (`feat:` → minor, `fix:`/`deps:` → patch,
`feat!:`/`BREAKING CHANGE` → major). It also bumps `Version` in `internal/cli/root.go`.

1. Land your changes on `main` with Conventional Commit messages.
2. When ready to ship, **merge the open release PR**. That:
   - tags `vX.Y.Z` and creates the GitHub Release with notes, and
   - builds `krayt` for `darwin/arm64` + `darwin/amd64`, writes `checksums.txt`, and uploads them
     to the release (in the same workflow run — no PAT needed).

That's it. No manual tagging.

## What triggers a CLI release (commit conventions)

release-please decides the CLI version from the **commit type, not the changed files** — there's
no per-folder ignore for a single-package repo. So keep image work out of CLI releases with the
commit type:

- `feat:` → minor, `fix:` → patch, `feat!:`/`BREAKING CHANGE:` → major. `chore:`/`docs:`/`ci:`
  don't bump the version.
- **Guest / image-only changes** — `internal/guest/**`, `cmd/krayt-agent`, `cmd/krayt-proxy`,
  `cmd/krayt-ask`, `images/**` — ship in the VM image, not the `krayt` binary, so commit them as
  **`chore:`**. (Renovate already types Nix-flake, GitHub-Actions, and Dockerfile updates as
  `chore:`, so image-dependency churn doesn't cut CLI releases.)
- **`internal/vmimage/pinned.go` is the exception**: pinning a new image *is* a CLI-facing change,
  so commit it as **`fix:`** to cut a CLI release that ships the new pin.

## Releasing a new VM image (only when the guest/image changed)

The image isn't a release-please package (see below) — it has its own small **RC → graduate**
tagging flow, purpose-built so a candidate can be boot-tested *before* it becomes pin-eligible:

1. **Land a change** under any of the watched paths: `images/**`, `internal/guest/**`,
   `cmd/krayt-agent/**`, `cmd/krayt-proxy/**`, `cmd/krayt-ask/**`.
2. **An RC auto-publishes.** Pushing that change — either to a PR branch or to `main` — triggers
   `vmimage-rc.yml`, which runs `hack/next-vmimage-tag.sh` and pushes the next
   `vmimage-vX.Y.Z-rc.N` tag (rc → rc+1 off the same series, or stable/no-prior-tag → the next
   patch's `-rc.1`). That push alone does **not** trigger `image.yml`'s
   `push: tags: ["vmimage-v*"]` job — tags pushed with the default `GITHUB_TOKEN` don't create new
   workflow runs (GitHub's anti-recursion safeguard; see `release-please.yml`'s note on the same
   restriction). So `vmimage-rc.yml` explicitly dispatches `image.yml` afterwards via
   `gh workflow run image.yml --ref <tag> -f publish=true -f tag=<tag>` (`workflow_dispatch` is one
   of the documented exceptions to that safeguard) — it publishes
   `ghcr.io/418-cloud/krayt-vmimage:vX.Y.Z-rc.N` and prints the ref + digest in a `::notice`. If the
   guest deps changed, regenerate `flake.nix`'s `vendorHash` first (the build log's `::notice`
   prints it).

   PR-triggered builds are the actual improvement over building only post-merge: a reviewer can
   pull and boot-test the artifact for the *specific proposed change* before approving it, not only
   after it's already on `main`. A push to `main` doesn't get special treatment — it's just another
   trigger for the same RC computation, since merging a PR is a code-review event, not a hardware
   boot test.

   > **Fork PRs:** `pull_request`-triggered runs from forks don't get repo secrets by default (a
   > GitHub safeguard), so a fork PR touching these paths won't be able to push the RC tag itself.
   > Not handled yet — if this project starts taking fork PRs against these paths, gate on
   > `github.event.pull_request.head.repo.fork == false` or have a maintainer re-run after review.

   It's expected and fine for RC numbers to end up orphaned (an abandoned or rebased PR) — they're
   throwaway candidate artifacts by design.

3. **Boot-test that specific RC.** Same as before: build via the `vm-image` workflow, or locally
   `nix build ./images#vmImage` + `TestBootHello` / end-to-end, on an Apple-Silicon Mac.
4. **Graduate it.** Once you're satisfied, run `vmimage-graduate.yml` (`workflow_dispatch`) with:
   - `rc_tag` — the exact RC you boot-tested (e.g. `vmimage-v0.5.1-rc.3`).
   - `version` — the clean version to publish (e.g. `0.5.1`, or `0.6.0` if the accumulated changes
     warrant more than the RC series' own provisional patch bump — this is the human judgment call
     that stands in for release-please's conventional-commit inference, made explicitly instead).

   This tags `rc_tag`'s **exact commit** as `vmimage-v<version>` — not whatever `main`'s tip
   happens to be at dispatch time — which is what makes the boot-test meaningful: the artifact
   `image.yml` publishes for the graduated tag is built from the exact source you already
   verified. Because the Nix build is meant to be reproducible, the graduated tag's digest
   *should* come out identical to the RC's; if it ever doesn't, that's a reproducibility gap worth
   investigating, not something to route around.

   The RC being graduated should normally already be part of `main`'s history (i.e. its PR has
   merged). Graduating one that only exists on an unmerged branch is possible but not the expected
   flow.
5. **Pin.** Copy the graduated tag's ref + digest into `internal/vmimage/pinned.go` (note the
   image version in the comment), commit to `main` as **`fix:`** so release-please cuts a CLI
   release that ships the new pin (a `chore:` here would *not* release, so the pin wouldn't ship).
   **Never pin a digest you haven't boot-tested** — and only ever pin a *graduated*
   (`vmimage-vX.Y.Z`, no `-rc`) digest, never an RC's.
6. The next CLI release then ships pinning the new image.

Publishing an RC (step 2) ≠ graduating (step 4) ≠ pinning (step 5): an image can sit in the
registry, RC or graduated, unused until it's verified and pinned.

Only `vmimage-graduate.yml`, run manually, ever produces a clean (non-`-rc.`) `vmimage-v*` tag —
`vmimage-rc.yml` only ever produces `-rc.N` tags, regardless of which branch or event triggered it.

**Why guest/image commits stay `chore:` and the pin commit carries the changelog content:**
release-please's CLI package watches the *whole repo* with no path exclusions, so a guest commit
typed `feat:`/`fix:` would count toward the CLI's next version/changelog the moment it lands — not
when the corresponding vmimage is actually graduated and pinned. Those are now fully decoupled
events (that's the point of the RC/graduate split), so an early `feat:`/`fix:` on the raw guest
commit could land in an earlier CLI release than the one that actually ships the pin — a changelog
entry describing something not yet true. Instead, write the **pin commit** itself descriptively
(its `fix:` message and body — e.g. summarizing the graduated RC's changes or linking its tag),
since that's the one commit guaranteed to land in the same CLI release that ships the new digest.

## Dependency updates

[Renovate] opens grouped PRs for Go modules, GitHub Actions (kept SHA-pinned), the Nix flake
inputs, and the `hack/**` Dockerfiles. **Auto-merge is off** — review and merge them yourself.
Per the commit conventions above, only **Go-module** updates are typed `deps:` (they show up under
Dependencies in the next CLI release); **Actions / Nix / Dockerfile** updates are typed `chore:`
(hidden, and they don't cut a CLI release). Renovate does **not** touch `pinned.go` (the boot-test
gate is manual by design).

Updates are held for **3 days** after a release (`minimumReleaseAge`) — a stability window so a
yanked or hot-fixed release is caught before Renovate proposes it. **Security fixes bypass this**
and are raised immediately (`vulnerabilityAlerts` sets `minimumReleaseAge: 0`).

[release-please]: https://github.com/googleapis/release-please
[Renovate]: https://docs.renovatebot.com
