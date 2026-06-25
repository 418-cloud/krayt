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
}

// ControlPort is the fixed guest vsock port the guest-agent listens on (§6.12).
const ControlPort uint32 = 1024
