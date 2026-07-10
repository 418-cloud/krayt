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
	"strconv"
	"syscall"
	"time"

	"github.com/crc-org/vfkit/pkg/config"

	"github.com/418-cloud/krayt/internal/provider"
)

// DefaultBinary is the vfkit executable name resolved on PATH when no path is set.
const DefaultBinary = "vfkit"

// DefaultDiskGiB is the scratch-disk size used when VMSpec.DiskGiB is unset. The scratch
// disk holds containerd's content store + snapshots and the guest-agent's working files
// (image tar, bundle, workspace), keeping them off the closure-sized rootfs and out of RAM.
const DefaultDiskGiB = 20

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

	// Clean up the run dir (incl. the multi-GB CoW clone) and socket dir if any step
	// below fails, so a partial Create never leaks disk state. Cleared on success.
	sockDir := ""
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(dir)
			if sockDir != "" {
				_ = os.RemoveAll(sockDir)
			}
		}
	}()

	// CoW clone of the raw rootfs so the run never mutates the shared base image.
	clone := filepath.Join(dir, "rootfs.img")
	if err := cloneFile(spec.RootFS, clone); err != nil {
		return nil, fmt.Errorf("vfkit: clone rootfs: %w", err)
	}

	// Per-run scratch disk (/dev/vdb in the guest): a sparse raw file sized to DiskGiB.
	// The guest formats + mounts it at /var/lib/containerd (§6.10), so the image import
	// and the agent's working files have room without bloating the rootfs or the base
	// image artifact. Sparse on APFS, so it costs nothing until written, and it is removed
	// with the run dir on Destroy.
	diskGiB := spec.DiskGiB
	if diskGiB == 0 {
		diskGiB = DefaultDiskGiB
	}
	scratch := filepath.Join(dir, "scratch.img")
	if err := createSparse(scratch, diskGiB<<30); err != nil {
		return nil, fmt.Errorf("vfkit: create scratch disk: %w", err)
	}

	// Unix-socket paths are capped at 104 bytes (sockaddr_un.sun_path) on macOS, and the
	// run dir (under $TMPDIR or .krayt) can exceed that. Keep the control + REST sockets
	// in a short dir so both vfkit's bind and our dial stay under the limit (§6.12).
	sockDir, err = newSockDir()
	if err != nil {
		return nil, err
	}
	ctrlSock := filepath.Join(sockDir, "control.sock")
	restSock := filepath.Join(sockDir, "rest.sock")

	vmConfig, err := buildConfig(spec, clone, scratch, ctrlSock)
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

	success = true
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

// sockRoot returns the short base directory for this user's per-VM unix sockets. /tmp is
// short on macOS (a symlink to /private/tmp), unlike $TMPDIR, keeping socket paths under
// the 104-byte sockaddr_un limit (§6.12). The uid suffix gives each user their own root so
// two users on a shared host never collide on the same directory — "/tmp/krayt-501" is
// still far under the limit, leaving ample room for the "vm-XXXXXXXX/control.sock" tail.
func sockRoot() string {
	return "/tmp/krayt-" + strconv.Itoa(os.Getuid())
}

// newSockDir creates a unique short-pathed directory for a VM's control + REST sockets under
// this user's socket root (sockRoot). See newSockDirAt for the root-parameterized logic (kept
// separate so tests can exercise it against a throwaway root instead of the real /tmp/krayt-*).
func newSockDir() (string, error) {
	return newSockDirAt(sockRoot())
}

// newSockDirAt creates a unique short-pathed directory for a VM's control + REST sockets under
// root.
//
// The socket root guards a VM's REST control socket (lifecycle: stop/kill) and vsock
// control channel, so it must not be a directory another local user controls. We verify or
// create it ourselves rather than os.MkdirAll (a no-op that leaves a pre-existing dir's
// owner/mode untouched): if the root already exists it must be a real directory (not a
// symlink into an attacker target), owned by this uid, mode exactly 0700 — otherwise we
// fail closed and let the human remove/fix it. We never chmod/chown a dir we don't own.
func newSockDirAt(root string) (string, error) {
	if err := ensureSockRoot(root); err != nil {
		return "", err
	}
	d, err := os.MkdirTemp(root, "vm-")
	if err != nil {
		return "", fmt.Errorf("vfkit: create socket dir: %w", err)
	}
	return d, nil
}

// ensureSockRoot makes root a private directory owned by the current user, or fails. It
// uses Lstat (no symlink following) + os.Mkdir (fails if the path already exists), so a
// symlink or a foreign-owned/loose-mode directory pre-placed at root is refused rather
// than trusted.
func ensureSockRoot(root string) error {
	fi, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		// Mkdir (not MkdirAll) fails if root exists, incl. as a symlink, so we never
		// follow a pre-placed link into an attacker-controlled target.
		if err := os.Mkdir(root, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				// Lost a race with a concurrent creator (root is shared across every VM this
				// user boots, §6.12) — re-validate whatever now exists rather than failing a
				// legitimate concurrent `krayt run` on a spurious EEXIST.
				return ensureSockRoot(root)
			}
			return fmt.Errorf("vfkit: create socket root: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("vfkit: stat socket root: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("vfkit: socket root %s: cannot read owner/mode", root)
	}
	if !fi.IsDir() || int(st.Uid) != os.Getuid() || fi.Mode().Perm() != 0o700 {
		return fmt.Errorf("vfkit: socket root %s is not a private directory owned by this user "+
			"(mode %o, uid %d); refusing to place VM control sockets there — remove or fix it",
			root, fi.Mode().Perm(), st.Uid)
	}
	return nil
}

// buildConfig assembles the vfkit VirtualMachine: Linux bootloader (kernel+initrd+
// cmdline), the CoW rootfs and the scratch disk as virtio-blk disks, a NAT NIC, and a
// host→guest vsock device bridged to ctrlSock on the host (§6.3, §6.6, §6.12). The rootfs
// is added first so it enumerates as /dev/vda (the cmdline's root=) and the scratch disk
// as /dev/vdb (mounted by the guest at /var/lib/containerd, §6.10).
func buildConfig(spec provider.VMSpec, rootfs, scratch, ctrlSock string) (*config.VirtualMachine, error) {
	bootloader := config.NewLinuxBootloader(spec.Kernel, spec.Cmdline, spec.Initrd)
	vmConfig := config.NewVirtualMachine(uint(spec.CPUs), spec.MemoryMiB, bootloader)

	blk, err := config.VirtioBlkNew(rootfs)
	if err != nil {
		return nil, fmt.Errorf("vfkit: virtio-blk (rootfs): %w", err)
	}
	scratchBlk, err := config.VirtioBlkNew(scratch)
	if err != nil {
		return nil, fmt.Errorf("vfkit: virtio-blk (scratch): %w", err)
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
	if err := vmConfig.AddDevices(blk, scratchBlk, nic, vsockDev); err != nil {
		return nil, fmt.Errorf("vfkit: add devices: %w", err)
	}
	return vmConfig, nil
}

// createSparse creates a sparse file of the given size in bytes (no blocks allocated until
// written; APFS-backed). Used for the per-run scratch disk.
func createSparse(path string, sizeBytes uint64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := f.Truncate(int64(sizeBytes)); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
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
	// The child inherited the log FD at Start; close the parent's copy so it isn't
	// leaked across runs (exec.Cmd only closes files it created itself).
	_ = logf.Close()
	v.cmd = cmd
	return nil
}

// DialControl dials vfkit's host-side vsock bridge socket. port is ignored: the guest
// vsock port is fixed in the VM config (ControlPort), and vfkit maps it to ctrlSock.
func (v *vm) DialControl(ctx context.Context, _ uint32) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", v.ctrlSock)
}

// ControlSocket returns the host-side control socket path, which the orchestrator records so
// a later `krayt answer`/`stop` can dial this run's guest directly (§6.2, §6.13).
func (v *vm) ControlSocket() string { return v.ctrlSock }

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

// isExpectedExit reports whether a finished vfkit process exited the way teardown
// expects: terminated by a signal (our Process.Kill). A graceful REST stop exits 0 (so
// err is nil and this isn't consulted); a non-zero, non-signal exit is a real vfkit
// failure (crash, bad config) and must surface rather than be swallowed by wait().
func isExpectedExit(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	// ExitCode() is -1 only when the process was terminated by a signal.
	return exitErr.ExitCode() == -1
}

var (
	_ provider.Provider = (*Provider)(nil)
	_ provider.VM       = (*vm)(nil)
)
