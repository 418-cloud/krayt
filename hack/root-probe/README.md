# root-probe — negative control for the non-root enforcement (findings #1/#3)

A throwaway image that is deliberately **root** (no `USER` set, so `uid 0`) — the
`KRAYT_ROOT_IMAGE` half of the confirmation logged in `HUMAN_TODO.md`
("[Security review] Run the container-hardening integration tests on a Mac (findings #1/#3)").
It is the negative control for `TestRootImageFailsClosed`
(`internal/orchestrator/integration_test.go`): `withEnforceNonRoot()`
(`internal/guest/runner/containerd_linux.go`) must reject the run — with an error naming the
non-root requirement — **before** this container ever launches (§8.2). krayt requires non-root
because the egress lock and secret confinement both depend on it: a uid-0 process could `setuid`
to proxyd regardless of dropped capabilities on some kernels (see `../hardening-probe/`, the
positive control that proves the `setuid(proxyd)` = `EPERM` property for a well-formed non-root
image).

The entrypoint (`entrypoint.sh`) is not expected to ever run — a rejected run never starts the
container process. It exists only so the image is otherwise a normal, runnable container; if the
fail-closed control ever regresses and the container *does* start, it fails loudly (exit 99,
`SHOULD NEVER RUN` on stderr) instead of silently doing something innocuous.

See also `../hardening-probe/`, the positive control (`KRAYT_HARDENING_IMAGE`) for the same test
suite.

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
cd hack/root-probe
docker buildx build --platform linux/arm64 -t <your-registry>/krayt-root-probe:latest --push .
```

## 2. Run the integration test
Also build/push `../hardening-probe` (see its README), then from the repo root:
```sh
KRAYT_KERNEL=…/vmlinuz KRAYT_INITRD=…/initrd KRAYT_ROOTFS=…/rootfs.img \
KRAYT_HARDENING_IMAGE=<your-registry>/krayt-hardening-probe:latest \
KRAYT_ROOT_IMAGE=<your-registry>/krayt-root-probe:latest \
  go test -tags 'integration darwin' \
  -run 'TestContainerHardening|TestRootImageFailsClosed' -v ./internal/orchestrator/
```

## Success looks like
`TestRootImageFailsClosed` passes: `orchestrator.Run` returns a **non-nil error** whose message
mentions `root` or `uid 0`, and no container ever executes (nothing from `entrypoint.sh` appears
in the run's logs).

## If it fails
If the run instead succeeds, or fails with an unrelated error, or the logs show the
`root-probe: SHOULD NEVER RUN` line — the non-root enforcement regressed. Check
`withEnforceNonRoot()` in `internal/guest/runner/containerd_linux.go` and that it still runs
*after* `oci.WithImageConfig(image)` in `buildSpecOpts`, which is what resolves `s.Process.User`
from this image's (absent) `USER`.

## Cleanup
```sh
krayt rm <run-id>   # if run standalone via `krayt run` rather than the Go test
docker rmi <your-registry>/krayt-root-probe:latest   # optional
```
