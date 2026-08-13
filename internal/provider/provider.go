// Package provider defines the single OS-specific seam in krayt. Everything above
// it (orchestration, protocol, patch logic, secrets, CLI) is OS-agnostic Go.
//
// Concrete providers live in build-tagged subpackages:
//   - provider/vfkit       macOS via crc-org/vfkit subprocess (v1)
//   - provider/vz          macOS via direct Code-Hex/vz (fallback)
//   - provider/firecracker Linux via firecracker-go-sdk (later)
//   - provider/fake        in-process loopback for tests (any OS)
//
// See KRAYT_SPEC.md §6.3.
package provider

import (
	"context"
	"net"
)

// VMSpec is the provider-level description of a single micro-VM. The orchestrator
// derives it from RunSpec.Resources plus the pinned base image (§6.3).
type VMSpec struct {
	ID        string
	Kernel    string // path to vmlinuz (or EFI image)
	Initrd    string // path to initramfs for the Linux bootloader; empty for EFI boot
	Cmdline   string // kernel command line (e.g. "console=hvc0 root=/dev/vda")
	RootFS    string // path to the BASE rootfs image; provider makes a CoW clone per run
	CID       uint32 // vsock guest CID — Firecracker only; ignored by the vz/vfkit providers (§6.12)
	CPUs      int
	MemoryMiB uint64
	DiskGiB   uint64
}

// Provider creates VMs. It is the only OS-specific interface in krayt.
type Provider interface {
	Create(ctx context.Context, spec VMSpec) (VM, error)
}

// VM is one running (or startable) micro-VM instance.
type VM interface {
	Start(ctx context.Context) error

	// DialControl opens the control channel to the guest-agent (guest listens, host
	// connects). On vfkit this is a unix-socket dial to the vsock bridge; on direct vz
	// it goes through the per-VM VZVirtioSocketDevice; on Firecracker it is an AF_VSOCK
	// connect to the guest CID. Returns a net.Conn usable as a gRPC transport (§6.12).
	// port is the guest vsock port (fixed; see §6.12).
	DialControl(ctx context.Context, port uint32) (net.Conn, error)

	Stop(ctx context.Context) error
	Destroy(ctx context.Context) error // also removes the CoW clone
	ID() string

	// LogPaths returns the provider's own process log and the guest's serial console log, for
	// diagnostics. The two may point at the same file (vfkit and firecracker both do this
	// today) or differ. Both paths live under the VM's own run directory, which Destroy
	// removes — callers that need this after the VM is gone (e.g. to report why a task failed
	// for a reason the container never saw, like an in-guest proxy/network fault) must read it
	// before calling Destroy, not after. Either path may be empty if the VM never got far
	// enough to produce one.
	LogPaths() (providerLog, consoleLog string)

	// ListenEgress returns a listener accepting guest-initiated connections on the fixed
	// egress vsock port (§6.6, §6.12): the guest's krayt-vsock-forward dials out to it for
	// every TCP connection the container makes, and the host's `krayt __egress-proxy` child
	// accepts from it. This is the ONE new primitive move-egress-proxy-to-host.md adds — every
	// other vsock channel in krayt is host-initiated (DialControl).
	//
	// The provider absorbs the backend asymmetry: on vfkit it is the unix socket a second
	// virtio-vsock device (listen=true) connects to; on Firecracker it is the unix socket at
	// <uds_path>_<port>, which is how a Firecracker vsock device exposes the guest→host
	// direction (no CONNECT handshake — that is host→guest only, see DialControl). On both,
	// the host is the one that binds and listens; the backend's own process (vfkit/
	// firecracker) is a client of it, connecting out only once the guest dials.
	//
	// Must be called after Create and before Start — the socket must exist before the backend
	// process launches so a guest connection racing the boot never finds it missing. Closing
	// the returned listener stops accepting; the VM itself is unaffected.
	ListenEgress(ctx context.Context, port uint32) (net.Listener, error)
}

// ControlPort is the fixed guest vsock port the guest-agent listens on (§6.12).
const ControlPort uint32 = 1024

// EgressPort is the fixed vsock port the guest's krayt-vsock-forward dials on the host for
// every container-initiated connection (§6.6, §6.12). Guest→host, the opposite direction of
// ControlPort — see VM.ListenEgress.
const EgressPort uint32 = 1025
