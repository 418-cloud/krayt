# Releasing krayt

One artifact: the **CLI** (`krayt`), versioned `vX.Y.Z` and automated by [release-please]. There
is no krayt-built VM image any more — `krayt run` rents a sandbox from msb, which owns its own
image entirely (`docs/adr-microsandbox-sandbox-layer.md`); `krayt` ships nothing to boot.

## Cutting a release

release-please watches `main` and keeps a **"release PR"** open that bumps the version + updates
`CHANGELOG.md` from your Conventional Commits (`feat:` → minor, `fix:`/`deps:` → patch,
`feat!:`/`BREAKING CHANGE` → major). It also bumps `Version` in `internal/cli/root.go`.

1. Land your changes on `main` with Conventional Commit messages.
2. When ready to ship, **merge the open release PR**. That:
   - tags `vX.Y.Z` and creates the GitHub Release with notes, and
   - builds `krayt` for `darwin/arm64` + `darwin/amd64` + `linux/amd64`, writes `checksums.txt`,
     and uploads them to the release (in the same workflow run — no PAT needed).

That's it. No manual tagging.

## What triggers a release (commit conventions)

release-please decides the version from the **commit type**: `feat:` → minor, `fix:`/`deps:` →
patch, `feat!:`/`BREAKING CHANGE:` → major. `chore:`/`docs:`/`ci:` don't bump the version.

## Dependency updates

[Renovate] opens grouped PRs for Go modules, GitHub Actions (kept SHA-pinned), and the
`hack/**`/`images/agents/**` Dockerfiles. **Auto-merge is off** — review and merge them yourself.
Per the commit conventions above, only **Go-module** updates are typed `deps:` (they show up under
Dependencies in the next release); **Actions / Dockerfile** updates are typed `chore:` (hidden,
and they don't cut a release).

Updates are held for **3 days** after a release (`minimumReleaseAge`) — a stability window so a
yanked or hot-fixed release is caught before Renovate proposes it. **Security fixes bypass this**
and are raised immediately (`vulnerabilityAlerts` sets `minimumReleaseAge: 0`).

[release-please]: https://github.com/googleapis/release-please
[Renovate]: https://docs.renovatebot.com
