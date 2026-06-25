//go:build darwin

// Package vfkit is the v1 macOS provider. It drives crc-org/vfkit (which wraps
// Code-Hex/vz over Virtualization.framework): Create builds the VM config via vfkit's
// pkg/config Go API and CoW-clones the raw rootfs; Start launches the signed vfkit
// binary as a subprocess controlled over its REST API; vsock is bridged to a host unix
// socket so DialControl is a plain unix dial (§6.3, §6.12). The virtualization
// entitlement lives on the vfkit binary, not krayt (§12).
package vfkit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/crc-org/vfkit/pkg/config"

	"github.com/418-cloud/krayt/internal/provider"
)

// DefaultBinary is the vfkit executable name resolved on PATH when no path is set.
const DefaultBinary = "vfkit"

// Provider creates vfkit-backed VMs.
type Provider struct {
	// Binary is the path to the vfkit executable (default: "vfkit" on PATH).
	Binary string
	// RunDir is the base directory for per-VM working dirs (clone + sockets).
	RunDir string
}

// New returns a vfkit provider. binary may be "" to resolve "vfkit" on PATH; runDir is
// the base dir for per-VM state (clone, control + REST sockets).
func New(binary, runDir string) *Provider {
	if binary == "" {
		binary = DefaultBinary
	}
	return &Provider{Binary: binary, RunDir: runDir}
}

// Create clones the base rootfs and assembles the vfkit VM config. The VM is not yet
// running; call Start to launch the subprocess.
func (p *Provider) Create(_ context.Context, spec provider.VMSpec) (provider.VM, error) {
	if spec.Kernel == "" || spec.RootFS == "" {
		return nil, fmt.Errorf("vfkit: VMSpec needs Kernel and RootFS")
	}
	binary, err := exec.LookPath(p.Binary)
	if err != nil {
		return nil, fmt.Errorf("vfkit: binary %q not found (brew install vfkit): %w", p.Binary, err)
	}

	dir := filepath.Join(p.RunDir, spec.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("vfkit: create run dir: %w", err)
	}

	// CoW clone of the raw rootfs so the run never mutates the shared base image.
	clone := filepath.Join(dir, "rootfs.img")
	if err := cloneFile(spec.RootFS, clone); err != nil {
		return nil, fmt.Errorf("vfkit: clone rootfs: %w", err)
	}

	// Unix-socket paths are capped at 104 bytes (sockaddr_un.sun_path) on macOS, and the
	// run dir (under $TMPDIR or .krayt) can exceed that. Keep the control + REST sockets
	// in a short dir so both vfkit's bind and our dial stay under the limit (§6.12).
	sockDir, err := newSockDir()
	if err != nil {
		return nil, err
	}
	ctrlSock := filepath.Join(sockDir, "control.sock")
	restSock := filepath.Join(sockDir, "rest.sock")

	vmConfig, err := buildConfig(spec, clone, ctrlSock)
	if err != nil {
		return nil, err
	}

	// Capture the guest serial console (kernel `console=hvc0`) to a file so boot failures
	// are diagnosable without an attached terminal.
	consoleLog := filepath.Join(dir, "console.log")
	serial, err := config.VirtioSerialNew(consoleLog)
	if err != nil {
		return nil, fmt.Errorf("vfkit: virtio-serial: %w", err)
	}
	if err := vmConfig.AddDevice(serial); err != nil {
		return nil, fmt.Errorf("vfkit: add serial: %w", err)
	}

	return &vm{
		id:         spec.ID,
		binary:     binary,
		dir:        dir,
		sockDir:    sockDir,
		clone:      clone,
		ctrlSock:   ctrlSock,
		restSock:   restSock,
		consoleLog: consoleLog,
		config:     vmConfig,
		rest:       newRESTClient(restSock),
	}, nil
}

// sockRoot is a short base directory for per-VM unix sockets. /tmp is short on macOS (a
// symlink to /private/tmp), unlike $TMPDIR, keeping socket paths under the 104-byte
// sockaddr_un limit.
const sockRoot = "/tmp/krayt"

// newSockDir creates a unique short-pathed directory for a VM's control + REST sockets.
func newSockDir() (string, error) {
	if err := os.MkdirAll(sockRoot, 0o700); err != nil {
		return "", fmt.Errorf("vfkit: create socket root: %w", err)
	}
	d, err := os.MkdirTemp(sockRoot, "vm-")
	if err != nil {
		return "", fmt.Errorf("vfkit: create socket dir: %w", err)
	}
	return d, nil
}

// buildConfig assembles the vfkit VirtualMachine: Linux bootloader (kernel+initrd+
// cmdline), the CoW rootfs as a virtio-blk disk, a NAT NIC, and a host→guest vsock
// device bridged to ctrlSock on the host (§6.3, §6.6, §6.12).
func buildConfig(spec provider.VMSpec, rootfs, ctrlSock string) (*config.VirtualMachine, error) {
	bootloader := config.NewLinuxBootloader(spec.Kernel, spec.Cmdline, spec.Initrd)
	vmConfig := config.NewVirtualMachine(uint(spec.CPUs), spec.MemoryMiB, bootloader)

	blk, err := config.VirtioBlkNew(rootfs)
	if err != nil {
		return nil, fmt.Errorf("vfkit: virtio-blk: %w", err)
	}
	nic, err := config.VirtioNetNew("") // NAT (VirtioNetNew sets Nat=true)
	if err != nil {
		return nil, fmt.Errorf("vfkit: virtio-net: %w", err)
	}
	// listen=false: connections are host→guest. vfkit creates the host unix socket
	// (ctrlSock); the guest-agent listens on the vsock port inside the VM (§6.12).
	vsockDev, err := config.VirtioVsockNew(uint(provider.ControlPort), ctrlSock, false)
	if err != nil {
		return nil, fmt.Errorf("vfkit: virtio-vsock: %w", err)
	}
	if err := vmConfig.AddDevices(blk, nic, vsockDev); err != nil {
		return nil, fmt.Errorf("vfkit: add devices: %w", err)
	}
	return vmConfig, nil
}

type vm struct {
	id         string
	binary     string
	dir        string
	sockDir    string
	clone      string
	ctrlSock   string
	restSock   string
	consoleLog string
	config     *config.VirtualMachine
	rest       *restClient

	cmd *exec.Cmd
}

// LogPaths returns the vfkit and guest-console log file paths, for diagnostics.
func (v *vm) LogPaths() (vfkitLog, consoleLog string) {
	return filepath.Join(v.dir, "vfkit.log"), v.consoleLog
}

// Start launches the vfkit subprocess with the assembled config plus a RESTful control
// URI over restSock. It returns once the process is launched; boot readiness (the guest
// answering Hello) is established by the host control client, not here (§11.6).
func (v *vm) Start(_ context.Context) error {
	if v.cmd != nil {
		return fmt.Errorf("vfkit: vm %s already started", v.id)
	}
	cmd, err := v.config.Cmd(v.binary)
	if err != nil {
		return fmt.Errorf("vfkit: build command: %w", err)
	}
	// vfkit serves its lifecycle REST API on this unix socket (§6.3).
	cmd.Args = append(cmd.Args, "--restful-uri", "unix://"+v.restSock)

	logf, err := os.Create(filepath.Join(v.dir, "vfkit.log"))
	if err != nil {
		return fmt.Errorf("vfkit: open log: %w", err)
	}
	cmd.Stdout = logf
	cmd.Stderr = logf

	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return fmt.Errorf("vfkit: start subprocess: %w", err)
	}
	v.cmd = cmd
	return nil
}

// DialControl dials vfkit's host-side vsock bridge socket. port is ignored: the guest
// vsock port is fixed in the VM config (ControlPort), and vfkit maps it to ctrlSock.
func (v *vm) DialControl(ctx context.Context, _ uint32) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", v.ctrlSock)
}

// Stop asks vfkit to stop the guest gracefully, then waits for the process to exit.
func (v *vm) Stop(ctx context.Context) error {
	if v.cmd == nil {
		return nil
	}
	// Best-effort graceful stop via the REST API; fall back to killing the process.
	if err := v.rest.stop(ctx); err != nil {
		_ = v.cmd.Process.Kill()
	}
	return v.wait(ctx)
}

// wait reaps the vfkit process, bounded by ctx; kills it if ctx expires first.
func (v *vm) wait(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- v.cmd.Wait() }()
	select {
	case err := <-done:
		v.cmd = nil
		if err != nil && !isExpectedExit(err) {
			return fmt.Errorf("vfkit: process exit: %w", err)
		}
		return nil
	case <-ctx.Done():
		_ = v.cmd.Process.Kill()
		<-done
		v.cmd = nil
		return ctx.Err()
	}
}

// Destroy stops the VM and removes its working dir (CoW clone) and socket dir.
func (v *vm) Destroy(ctx context.Context) error {
	if v.cmd != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = v.Stop(stopCtx)
		cancel()
	}
	if v.sockDir != "" {
		_ = os.RemoveAll(v.sockDir)
	}
	if err := os.RemoveAll(v.dir); err != nil {
		return fmt.Errorf("vfkit: remove run dir: %w", err)
	}
	return nil
}

func (v *vm) ID() string { return v.id }

// isExpectedExit treats a killed/stopped vfkit as a clean teardown rather than an error.
func isExpectedExit(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

var (
	_ provider.Provider = (*Provider)(nil)
	_ provider.VM       = (*vm)(nil)
)
