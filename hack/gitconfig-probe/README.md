# gitconfig-probe — hardware confirmation for the guest git-config isolation (finding #2)

A throwaway "agent" image that attempts the container→guest-root escape from §10 finding #2 —
the `KRAYT_GITCONFIG_IMAGE` half of the confirmation logged in `HUMAN_TODO.md`
("[Security review] Run the guest git-config-injection escape test on a Mac (finding #2)"). It is
the probe for `TestGuestGitConfigInjectionInert` (`internal/orchestrator/integration_test.go`): a
**non-root** image whose entrypoint (`entrypoint.sh`), running inside the writable
`/workspace` bind mount:

- writes an executable `/workspace/pwn.sh` that, if it ever runs, creates a sentinel file
  `/workspace/PWNED_BY_ROOT` — note `pwn.sh` itself is a normal new file that always lands in
  `changes.patch` (it's just a file sitting in the workspace); the escape signal is whether the
  sentinel it *creates* shows up as its own new-file entry, not whether the path string appears
  anywhere in the patch (`pwn.sh`'s own source mentions it, harmlessly);
- appends `[core]\n\tfsmonitor = /workspace/pwn.sh` and a `[diff "evil"] textconv = /workspace/pwn.sh`
  driver to `/workspace/.git/config`, plus `* diff=evil` to `/workspace/.gitattributes` — the two
  ways repo-local git config runs an external program on `git add`/`git diff`;
- makes one normal tracked edit (`greeting.txt`);
- exits 0.

The guest-agent's own git runs as **root** and, after the container exits, generates
`changes.patch` by running `git add -A` + `git diff` (`internal/patch.Diff`). Before the fix
(§6.7, §10 finding #2), that git ran directly against the container-writable `/workspace/.git`,
so `core.fsmonitor`/`textconv` set above would have executed `pwn.sh` **as root** — a
container→guest-root escape. The fix isolates patch generation into a root-only `patchgit`
snapshot (taken pristine, before this container ever runs) with `core.fsmonitor`/
`core.hooksPath` force-cleared and `--no-textconv`, so this image's injection must be inert.

`TestGuestGitConfigInjectionInert` asserts `changes.patch` has no `diff --git a/PWNED_BY_ROOT
b/PWNED_BY_ROOT` entry (the injected config never ran as root, so the sentinel file was never
created) while the normal `greeting.txt` edit **is** present (the patch is still faithful).

> **Published by CI.** `.github/workflows/probe-images.yml` builds every probe multi-arch
> (`linux/amd64` + `linux/arm64`) into one package, with the probe type as the tag:
> `ghcr.io/<owner>/krayt-probe:{probe}`. Use that rather than building by hand — the manual steps
> below remain valid for iterating on the probe itself. Note the arch: the Linux/firecracker
> backend needs `amd64`, the macOS/vfkit backend `arm64`, and CI publishes both.

## Prerequisites
- Apple-Silicon Mac with the `krayt` binary built (`go build -o bin/krayt ./cmd/krayt`).
- The base micro-VM image already built + pinned (same one the other integration tests use).
- A container registry the Mac can pull from (Docker Hub, GHCR, …).

## 1. Build + push the probe image (linux/arm64)
```sh
cd hack/gitconfig-probe
docker buildx build --platform linux/arm64 -t <your-registry>/krayt-gitconfig-probe:latest --push .
```

## 2. Run the integration test
From the repo root:
```sh
KRAYT_KERNEL=…/vmlinuz KRAYT_INITRD=…/initrd KRAYT_ROOTFS=…/rootfs.img \
KRAYT_GITCONFIG_IMAGE=<your-registry>/krayt-gitconfig-probe:latest \
  go test -tags 'integration darwin' \
  -run TestGuestGitConfigInjectionInert -v ./internal/orchestrator/
```

## Success looks like
`TestGuestGitConfigInjectionInert` passes: `changes.patch` is produced, has no `diff --git
a/PWNED_BY_ROOT b/PWNED_BY_ROOT` entry (the injected fsmonitor/textconv never ran as root), and
still carries the normal `greeting.txt` edit. As an extra manual check on the same run, `nft list
ruleset` inside the guest still shows the egress lock and no secret was exfiltrated (root code
never executed to flush it).

Note: `changes.patch` legitimately contains the *text* `PWNED_BY_ROOT` inside `pwn.sh`'s own
diff (its source names that path) — that's expected and not a failure. Only an actual
`PWNED_BY_ROOT` new-file entry means the escape happened.

## If it fails
If `changes.patch` has a `diff --git a/PWNED_BY_ROOT b/PWNED_BY_ROOT` entry, the
container→guest-root escape regressed — check that `internal/guest/service.go` still calls
`patch.SetupPatchGit` **before** `makeContainerWritable`, and that
`internal/patch.Diff`/`BundleCommits` still run against `patchGitDir` (not `workspace/.git`) with
`patchGenGitArgs`'s `core.fsmonitor`/`core.hooksPath` knobs and `--no-textconv` intact.

## Cleanup
```sh
krayt rm <run-id>   # if run standalone via `krayt run` rather than the Go test
docker rmi <your-registry>/krayt-gitconfig-probe:latest   # optional
```
