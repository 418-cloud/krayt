# edit-probe — the trivial user image for the real-VM end-to-end suite

A throwaway "agent" image that is the **positive control** for `TestEndToEndRealVM` and
`TestConcurrentRealVMs` (`internal/orchestrator/integration_test.go`), the `KRAYT_IMAGE` those
tests need. Its entrypoint (`entrypoint.sh`), running non-root inside the writable `/workspace`
bind mount:

- writes a single fixed line to a fixed file (`/workspace/EDITED_BY_KRAYT.txt`) — a deterministic,
  idempotent edit, identical on every run (so the concurrent suite sees the same patch each time);
- logs what it did to stdout;
- exits 0.

That is all. Unlike the security probes (`netprobe`, `hardening-probe`, `root-probe`,
`gitconfig-probe`, `secrets-probe`) this image asserts nothing from inside the container and has no
sentinel/attack logic — it is the happy-path control that proves a real run boots, runs a
container, captures the edit into `changes.patch`, and hands back a diff that `krayt apply` applies
cleanly to the host repo. It exists so the integration suite can run end-to-end with **zero live
LLM credentials** (a real agent image like `hack/claude-code` needs an Anthropic key; this does
not).

`hack/run-integration-tests.sh` defaults `KRAYT_IMAGE` to `ghcr.io/418-cloud/krayt-probe:edit-probe`.

> **Published by CI.** `.github/workflows/probe-images.yml` builds every probe multi-arch
> (`linux/amd64` + `linux/arm64`) into one package, with the probe type as the tag:
> `ghcr.io/<owner>/krayt-probe:{probe}`. Use that rather than building by hand — the manual steps
> below remain valid for iterating on the probe itself. Note the arch: the Linux/firecracker
> backend needs `amd64`, the macOS/vfkit backend `arm64`, and CI publishes both.

## Prerequisites
- A host that can run the base micro-VM image (Apple-Silicon Mac with `vfkit`, or a Linux host
  with `/dev/kvm` + `firecracker`) and the `krayt` binary built (`go build -o bin/krayt ./cmd/krayt`).
- The base micro-VM image already built + pinned (same one the other integration tests use).
- A container registry the host can pull from (Docker Hub, GHCR, …).

## 1. Build + push the probe image
```sh
cd hack/edit-probe
# arm64 for the macOS/vfkit backend, amd64 for the Linux/firecracker backend:
docker buildx build --platform linux/arm64 -t <your-registry>/krayt-edit-probe:latest --push .
```

## 2. Run the integration test
From the repo root (macOS/vfkit):
```sh
KRAYT_KERNEL=…/vmlinuz KRAYT_INITRD=…/initrd KRAYT_ROOTFS=…/rootfs.img \
KRAYT_IMAGE=<your-registry>/krayt-edit-probe:latest \
  go test -tags 'integration darwin' \
  -run 'TestEndToEndRealVM|TestConcurrentRealVMs' -v ./internal/orchestrator/
```
On Linux/firecracker the test binary needs `CAP_NET_ADMIN` — see the header of
`internal/orchestrator/integration_test.go`, or just run `hack/run-integration-tests.sh`.

## Success looks like
`TestEndToEndRealVM` passes: `changes.patch` is produced, carries the `EDITED_BY_KRAYT.txt` edit,
and applies cleanly to the host repo. `TestConcurrentRealVMs` passes with several VMs booting this
image at once.

## Cleanup
```sh
krayt rm <run-id>   # if run standalone via `krayt run` rather than the Go test
docker rmi <your-registry>/krayt-edit-probe:latest   # optional
```
