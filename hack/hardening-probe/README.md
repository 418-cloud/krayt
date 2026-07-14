# hardening-probe — hardware confirmation for the container OCI hardening (findings #1/#3)

A throwaway "agent" image that proves the least-privilege container spec end to end on real
hardware — the `KRAYT_HARDENING_IMAGE` half of the confirmation logged in `HUMAN_TODO.md`
("[Security review] Run the container-hardening integration tests on a Mac (findings #1/#3)").
It is the positive control for `TestContainerHardening`
(`internal/orchestrator/integration_test.go`): a **non-root** image whose entrypoint exits 0
ONLY when every hardening control `securitySpecOpts` (`internal/guest/runner/containerd_linux.go`)
applies actually holds inside the container:

- `/proc/self/status`: `CapEff` and `CapAmb` are all-zero, `NoNewPrivs` is `1`, `Seccomp` is `2`
  (`SECCOMP_MODE_FILTER`).
- `id -u` (checked via `os.Getuid()`) is not `0`.
- `setuid()` to proxyd's uid — read straight out of `/proc/net/tcp`, visible because the
  container shares the VM's network namespace (§6.6) — fails with `EPERM`. This is the
  egress-allowlist-bypass regression (finding #1): without dropped `CAP_SETUID`/`CAP_SETGID`
  and enforced non-root, a container process could assume proxyd's uid and satisfy the
  nftables `skuid "proxyd"` rule directly, routing straight past the L7 allowlist.

It speaks only the Go stdlib (no krayt imports) so a green run proves the OCI spec itself, not
any client code, and logs every check with a distinct non-zero exit code per failure, so a
hardware regression is point-blank obvious from `krayt ls` (the `EXIT` column) or the logs.

See also `../root-probe/`, the negative control (`KRAYT_ROOT_IMAGE`) for the same test suite.

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
cd hack/hardening-probe
docker buildx build --platform linux/arm64 -t <your-registry>/krayt-hardening-probe:latest --push .
```

## 2. Run the integration tests
Also build/push `../root-probe` (see its README), then from the repo root:
```sh
KRAYT_KERNEL=…/vmlinuz KRAYT_INITRD=…/initrd KRAYT_ROOTFS=…/rootfs.img \
KRAYT_HARDENING_IMAGE=<your-registry>/krayt-hardening-probe:latest \
KRAYT_ROOT_IMAGE=<your-registry>/krayt-root-probe:latest \
  go test -tags 'integration darwin' \
  -run 'TestContainerHardening|TestRootImageFailsClosed' -v ./internal/orchestrator/
```

## Success looks like
- `TestContainerHardening` passes — `orchestrator.Run` returns exit code `0`.
- The probe's `[hardening-probe]` log lines (visible via `krayt logs <id>` if you run it
  standalone with `krayt run`) show every check as `ok:`, ending in `done: success`.

## Exit codes (what broke, if it isn't 0)
| exit | meaning | likely cause |
|------|---------|--------------|
| 0  | success | — |
| 10 | `/proc/self/status` unreadable / missing a field | proc not mounted, or an unexpected kernel |
| 11 | `CapEff` not all-zero | a capability leaked through `oci.WithCapabilities` |
| 12 | `CapAmb` not all-zero | `withClearAmbient()` didn't run, or ran before caps were set |
| 13 | `NoNewPrivs` != 1 | containerd's default was overridden somewhere |
| 14 | `Seccomp` != 2 | `seccomp.WithDefaultProfile()` skipped, or `SeccompUnconfined` leaked in |
| 15 | running as uid 0 | `withEnforceNonRoot()` didn't reject the image (shouldn't be reachable) |
| 16 | proxyd's `127.0.0.1:3128` listener not found in `/proc/net/tcp` | proxy not running, or the container doesn't share the VM netns |
| 17 | `setuid(proxyd)` **succeeded** | the egress-allowlist bypass (finding #1) is open — `CAP_SETUID`/`CAP_SETGID` leaked or non-root isn't enforced |
| 18 | `setuid(proxyd)` failed with something other than `EPERM` | unexpected kernel/runtime behavior — investigate before treating as pass |

`krayt logs <id>` shows the full `[hardening-probe]` trace; the exit code is in `krayt ls`.

## Cleanup
```sh
krayt rm <run-id>
docker rmi <your-registry>/krayt-hardening-probe:latest   # optional
```

This whole directory is a nested Go module, isolated from the krayt build.
