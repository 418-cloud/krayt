#!/usr/bin/env bash
# run-integration-tests.sh — one command to run krayt's real-VM integration suite.
#
# The `//go:build integration` tests boot a real micro-VM per test (vfkit on Apple Silicon,
# firecracker on Linux/KVM). Each test file documents its own `go test` invocation and required
# env vars in a header comment; this script encodes those steps so "run the integration tests" is
# one command instead of a source-reading exercise. The per-file headers remain the authoritative
# manual fallback for running a single test by hand.
#
# What it does:
#   1. Detects the host OS (macOS/vfkit vs Linux/firecracker) and runs that backend's suite.
#   2. Builds `krayt` and runs `krayt doctor` as a preflight (it names exactly what's missing).
#   3. Resolves the base VM image (via `krayt image pull`, unless KRAYT_KERNEL/INITRD/ROOTFS are
#      already exported) and the probe image refs (defaulting to the CI-published krayt-probe tags).
#   4. Runs `go test -tags integration ...` for this host's packages, propagating any failure.
#
# It does NOT run hack/linux-net-setup.sh's one-time persistent host setup (NAT rules, systemd
# unit, kvm group) — that has a different blast radius and is a separate documented step; if it's
# missing, `krayt doctor` says so and this script stops. It DOES run `sudo setcap` on the Linux
# test binaries it just compiled (a per-invocation grant on a throwaway $TMPDIR binary, needed for
# the VM's tap device).
#
# Usage:
#   hack/run-integration-tests.sh                 # the whole suite for this host
#   hack/run-integration-tests.sh --run TestBoot  # forward -run to go test (iterate on one test)
#
# Override any KRAYT_* env var to point at your own image/probe refs; see the table below.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# --- args -------------------------------------------------------------------------------------
run_filter=""
while [ $# -gt 0 ]; do
  case "$1" in
    --run|-run)
      run_filter="${2:-}"
      if [ -z "$run_filter" ]; then
        echo "error: --run needs a pattern (e.g. --run TestBootHello)" >&2
        exit 2
      fi
      shift 2
      ;;
    -h|--help)
      sed -n '2,32p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "error: unknown argument: $1 (try --help)" >&2
      exit 2
      ;;
  esac
done

os="$(uname -s)"
case "$os" in
  Darwin|Linux) ;;
  *)
    echo "error: integration tests only exist for macOS (vfkit) and Linux (firecracker); got '$os'" >&2
    exit 1
    ;;
esac

# --- 1. preflight: build krayt + run doctor ---------------------------------------------------
echo "==> Building krayt"
make krayt

# On Linux, krayt doctor's CAP_NET_ADMIN check inspects the binary it runs as, and `make krayt`
# writes a fresh, un-capped ./bin/krayt every time (so a file cap granted by hack/linux-net-setup.sh
# to an installed krayt on PATH does not apply here). Grant the cap on this throwaway build artifact
# — the same per-invocation `sudo setcap` this script already does for the compiled test binaries
# below (decision 3), NOT linux-net-setup.sh's persistent NAT/systemd host setup — so doctor's
# preflight reflects reality instead of always failing that one check. If setcap can't run (no
# sudo), we let doctor report it and stop with its own pointer.
if [ "$os" = "Linux" ]; then
  echo "==> Granting CAP_NET_ADMIN on ./bin/krayt (sudo setcap) for the doctor preflight"
  if ! sudo setcap cap_net_admin+ep ./bin/krayt; then
    echo "warning: could not setcap ./bin/krayt — 'krayt doctor' will flag CAP_NET_ADMIN below" >&2
  fi
fi

echo "==> Preflight: krayt doctor"
if ! ./bin/krayt doctor; then
  echo "error: 'krayt doctor' reported unmet prerequisites (see its output above)." >&2
  echo "       Fix what it names — e.g. /dev/kvm access, firecracker/vfkit install, or the" >&2
  echo "       one-time hack/linux-net-setup.sh host setup — then re-run this script." >&2
  exit 1
fi

# --- 2. resolve the base VM image -------------------------------------------------------------
# Skip the pull if the caller already exported all three (lets CI reuse a pulled image across runs).
if [ -n "${KRAYT_KERNEL:-}" ] && [ -n "${KRAYT_INITRD:-}" ] && [ -n "${KRAYT_ROOTFS:-}" ]; then
  echo "==> Using base image from KRAYT_KERNEL/KRAYT_INITRD/KRAYT_ROOTFS in the environment"
else
  echo "==> Pulling the base VM image (krayt image pull)"
  pull_out="$(./bin/krayt image pull)"
  echo "$pull_out"
  # Parse the "  kernel: <path>" / "  initrd: <path>" / "  rootfs: <path>" lines (runImagePull in
  # internal/cli/image.go). awk keeps everything after the first ": " so paths with spaces survive.
  KRAYT_KERNEL="$(printf '%s\n' "$pull_out" | awk -F': ' '/^  kernel: /{print substr($0, index($0,": ")+2)}')"
  KRAYT_INITRD="$(printf '%s\n' "$pull_out" | awk -F': ' '/^  initrd: /{print substr($0, index($0,": ")+2)}')"
  KRAYT_ROOTFS="$(printf '%s\n' "$pull_out" | awk -F': ' '/^  rootfs: /{print substr($0, index($0,": ")+2)}')"
  if [ -z "$KRAYT_KERNEL" ] || [ -z "$KRAYT_INITRD" ] || [ -z "$KRAYT_ROOTFS" ]; then
    echo "error: could not parse kernel/initrd/rootfs paths from 'krayt image pull' output" >&2
    exit 1
  fi
  export KRAYT_KERNEL KRAYT_INITRD KRAYT_ROOTFS
fi
echo "    kernel: $KRAYT_KERNEL"
echo "    initrd: $KRAYT_INITRD"
echo "    rootfs: $KRAYT_ROOTFS"

# --- 3. resolve probe image refs --------------------------------------------------------------
# Each defaults to the CI-published tag in the single krayt-probe package; override to test your own.
probe_base="ghcr.io/418-cloud/krayt-probe"
: "${KRAYT_IMAGE:=${probe_base}:edit-probe}"            # TestEndToEndRealVM, TestConcurrentRealVMs
: "${KRAYT_NETPROBE_IMAGE:=${probe_base}:netprobe}"     # TestEgressEnforcement
: "${KRAYT_HARDENING_IMAGE:=${probe_base}:hardening-probe}" # TestContainerHardening
: "${KRAYT_ROOT_IMAGE:=${probe_base}:root-probe}"       # TestRootImageFailsClosed
: "${KRAYT_GITCONFIG_IMAGE:=${probe_base}:gitconfig-probe}" # TestGuestGitConfigInjectionInert
: "${KRAYT_SECRETS_IMAGE:=${probe_base}:secrets-probe}" # TestSecretConfinementInArtifacts
: "${KRAYT_ALLOW_HOST:=example.com}"                    # TestEgressEnforcement (netprobe's baked-in default)
export KRAYT_IMAGE KRAYT_NETPROBE_IMAGE KRAYT_HARDENING_IMAGE KRAYT_ROOT_IMAGE \
  KRAYT_GITCONFIG_IMAGE KRAYT_SECRETS_IMAGE KRAYT_ALLOW_HOST
# KRAYT_CMDLINE is intentionally left unset — both backends default it sensibly.

echo "==> Probe images:"
echo "    KRAYT_IMAGE=$KRAYT_IMAGE"
echo "    KRAYT_NETPROBE_IMAGE=$KRAYT_NETPROBE_IMAGE   (KRAYT_ALLOW_HOST=$KRAYT_ALLOW_HOST)"
echo "    KRAYT_HARDENING_IMAGE=$KRAYT_HARDENING_IMAGE"
echo "    KRAYT_ROOT_IMAGE=$KRAYT_ROOT_IMAGE"
echo "    KRAYT_GITCONFIG_IMAGE=$KRAYT_GITCONFIG_IMAGE"
echo "    KRAYT_SECRETS_IMAGE=$KRAYT_SECRETS_IMAGE"

# --- 4. run the suite for this OS -------------------------------------------------------------
if [ "$os" = "Darwin" ]; then
  echo "==> Running the darwin/vfkit integration suite"
  # macOS needs no setcap: vfkit carries the virtualization entitlement and creates its own NAT.
  # Two explicit forms rather than an array with -run appended: macOS ships bash 3.2, where
  # "${empty[@]}" under `set -u` is an "unbound variable" error.
  if [ -n "$run_filter" ]; then
    go test -tags 'integration darwin' -v -run "$run_filter" \
      ./internal/provider/vfkit/... ./internal/orchestrator/...
  else
    go test -tags 'integration darwin' -v \
      ./internal/provider/vfkit/... ./internal/orchestrator/...
  fi
  echo "==> Integration suite passed."
  exit 0
fi

# Linux/firecracker: the test binaries need CAP_NET_ADMIN to create each VM's tap device. Compile
# each package to a throwaway binary, grant the cap on just that binary, then run it.
echo "==> Running the linux/firecracker integration suite"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

status=0
run_pkg() {
  local pkg="$1" name="$2"
  local bin="$tmp/$name.test"
  echo "--> Compiling $pkg"
  go test -c -tags 'integration linux' -o "$bin" "$pkg"
  echo "--> Granting CAP_NET_ADMIN on $bin (sudo setcap)"
  sudo setcap cap_net_admin+ep "$bin"
  local test_args=(-test.v)
  [ -n "$run_filter" ] && test_args+=(-test.run "$run_filter")
  echo "--> Running $name integration tests"
  if ! "$bin" "${test_args[@]}"; then
    echo "!!! $name integration tests FAILED" >&2
    status=1
  fi
}

run_pkg ./internal/provider/firecracker/ firecracker
run_pkg ./internal/orchestrator/ orchestrator

if [ "$status" -ne 0 ]; then
  echo "==> Integration suite FAILED (see above)." >&2
  exit 1
fi
echo "==> Integration suite passed."
