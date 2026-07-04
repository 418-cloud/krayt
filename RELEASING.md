# Releasing krayt

Two independently-versioned artifacts, tied together by a pinned image digest:

- **CLI** (`krayt`) — versioned `vX.Y.Z`, automated by [release-please]. This is what most
  releases are.
- **VM base image** (`krayt-vmimage`) — versioned on its own `vmimage-v*` tags, released
  manually because it must be **boot-tested on Apple-Silicon hardware** first (§11.6). Changes
  rarely (only when the guest-agent, protocol, or image flake changes).

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

Because the image build is a slow, reproducible Nix build and must be verified on real hardware,
it's a deliberate step:

1. **Build + boot-test.** Trigger `vm-image` (open a PR touching `images/**`/`internal/**`, or
   `workflow_dispatch`) to build it; boot-test on an Apple-Silicon Mac (`TestBootHello` /
   end-to-end). If the guest deps changed, regenerate `flake.nix` `vendorHash` first (the build
   log's `::notice` prints it).
2. **Publish.** Push a `vmimage-vI.J.K` tag → `image.yml` publishes
   `ghcr.io/418-cloud/krayt-vmimage:vI.J.K` and prints the ref + digest in a `::notice`.
3. **Pin.** Copy that ref + digest into `internal/vmimage/pinned.go` (note the image version in the
   comment), commit to `main` (a normal `fix:`/`chore:` commit — release-please folds it into the
   next CLI release). **Never pin a digest you haven't boot-tested.**
4. The next CLI release then ships pinning the new image.

Publishing (step 2) ≠ pinning (step 3): an image can sit in the registry unused until it's
verified and pinned.

## Dependency updates

[Renovate] opens grouped `deps:` PRs for Go modules, GitHub Actions (kept SHA-pinned), the Nix
flake inputs, and the `hack/**` Dockerfiles. Auto-merge is **off** — review and merge them
yourself (they land in the next release's Dependencies section). Renovate does **not** touch
`pinned.go` (the boot-test gate is manual by design).

[release-please]: https://github.com/googleapis/release-please
[Renovate]: https://docs.renovatebot.com
