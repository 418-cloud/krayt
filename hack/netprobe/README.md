# netprobe — hardware confirmation for the §6.6 egress control

A throwaway "agent" image that proves, on real hardware, that krayt's egress control actually
holds: the allowlisted host is reachable **through the proxy**, a non-allowlisted host is **not**,
and a **raw socket that ignores the proxy is dropped**. That last check is the load-bearing one —
the proxy is advisory (a hostile agent just wouldn't use it), so the real lock is the nftables
`skuid "proxyd"` ruleset. It is the on-hardware regression for security-review finding #1.

Used by `TestEgressEnforcement` as `KRAYT_NETPROBE_IMAGE`. Exit codes are documented in `main.go`
(one per failure mode, so a regression is legible straight from `krayt ls`).

**`KRAYT_ALLOW_HOST` must match.** The run does not forward the network policy into the container,
so the allowlisted host is baked into the image as an `ENV` (default `example.com`). Set the test's
`KRAYT_ALLOW_HOST` to the same value, or the probe will correctly report the allowlisted host as
unreachable and exit 21.

## Run it

CI publishes every probe multi-arch (`linux/amd64` + `linux/arm64`) into one package, with the
probe type as the tag (`.github/workflows/probe-images.yml`), so there is nothing to build:

```sh
KRAYT_KERNEL=…/vmlinuz KRAYT_INITRD=…/initrd KRAYT_ROOTFS=…/rootfs.img \
KRAYT_NETPROBE_IMAGE=ghcr.io/418-cloud/krayt-probe:netprobe KRAYT_ALLOW_HOST=example.com \
  go test -tags integration -run TestEgressEnforcement -v ./internal/orchestrator/
```

To iterate on the probe itself, build it locally and push it anywhere the test host can pull from:

```sh
podman build --platform linux/amd64 -t <registry>/krayt-probe:netprobe \
  -f hack/netprobe/Dockerfile hack/netprobe
```
