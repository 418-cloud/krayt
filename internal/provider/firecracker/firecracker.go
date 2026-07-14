//go:build linux

// Package firecracker is the Linux provider (§6.3, Phase 7). It drives Firecracker over
// KVM: Create clones the raw rootfs, allocates a tap device + vsock CID, and Start
// launches the firecracker binary as a subprocess configured through its REST API on a
// unix socket — the same subprocess+REST idiom the vfkit provider uses on macOS.
//
// Three things differ materially from vfkit, and they are the whole reason this package
// exists (§6.12):
//
//   - vsock. Firecracker does NOT expose the guest over the host's AF_VSOCK; it
//     deliberately bypasses vhost and mediates between an AF_UNIX socket on the host and
//     AF_VSOCK in the guest. A host→guest connection is a unix dial followed by a
//     "CONNECT <port>\n" handshake (see vsock.go). VMSpec.CID is still the guest's CID,
//     but because each VM has its own unix socket there is no shared host CID namespace
//     to collide in — see allocSlot.
//
//   - CoW. There is no clonefile(2). We use the FICLONE ioctl (reflink) where the
//     filesystem supports it and fall back to a sparse-aware copy where it does not —
//     notably ext4, which has no reflink support at all (see clone.go).
//
//   - Networking. Firecracker has no built-in NAT device: it gives the VM a bare tap
//     interface and nothing else — no DHCP server. The provider therefore creates the tap,
//     addresses it, and passes the guest its address on the kernel cmdline in dracut's
//     `ip=`/`ifname=` form (see tap.go).
//
//     Note *who reads that cmdline*, because the obvious answer is wrong and it is the first
//     place you would look when the guest comes up with no address: NOT the kernel. Kernel-level
//     IP autoconfiguration needs CONFIG_IP_PNP, which the nixpkgs kernel does not set, so the
//     kernel ignores the parameter silently. It is applied in userspace by
//     systemd-network-generator, which the image enables (§11.6). Debug that layer, not the
//     kernel's.
//
//     The in-guest egress proxy + nftables lock (§6.6) run unchanged; this is only about getting
//     an IP onto the wire.
package firecracker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/418-cloud/krayt/internal/provider"
)

// DefaultBinary is the firecracker executable name resolved on PATH when no path is set.
const DefaultBinary = "firecracker"

// DefaultDiskGiB is the scratch-disk size used when VMSpec.DiskGiB is unset. The scratch
// disk holds containerd's content store + snapshots and the guest-agent's working files
// (image tar, bundle, workspace), keeping them off the closure-sized rootfs and out of RAM.
const DefaultDiskGiB = 20

// stopTimeout bounds the graceful guest shutdown before the process is killed.
const stopTimeout = 10 * time.Second

// Provider creates Firecracker-backed VMs.
type Provider struct {
	// Binary is the path to the firecracker executable (default: "firecracker" on PATH).
	Binary string
	// RunDir is the base directory for per-VM working dirs (clone + scratch + logs).
	RunDir string
}

// New returns a firecracker provider. binary may be "" to resolve "firecracker" on PATH;
// runDir is the base dir for per-VM state (rootfs clone, scratch disk, logs).
func New(binary, runDir string) *Provider {
	if binary == "" {
		binary = DefaultBinary
	}
	return &Provider{Binary: binary, RunDir: runDir}
}

// Create clones the base rootfs, allocates this VM's network + vsock slot, and creates its
// tap device. The VM is not yet running; call Start to launch firecracker and boot it.
func (p *Provider) Create(_ context.Context, spec provider.VMSpec) (provider.VM, error) {
	if spec.Kernel == "" || spec.RootFS == "" {
		return nil, fmt.Errorf("firecracker: VMSpec needs Kernel and RootFS")
	}
	binary, err := exec.LookPath(p.Binary)
	if err != nil {
		return nil, fmt.Errorf("firecracker: binary %q not found "+
			"(download a release from github.com/firecracker-microvm/firecracker): %w", p.Binary, err)
	}

	dir := filepath.Join(p.RunDir, spec.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("firecracker: create run dir: %w", err)
	}

	// Undo every partial step if a later one fails, so a failed Create never leaks a
	// multi-GB clone, a tap device, or a socket dir. Cleared on success.
	var (
		sockDir string
		slot    *netSlot
		success bool
	)
	defer func() {
		if success {
			return
		}
		if slot != nil {
			_ = slot.destroy()
		}
		if sockDir != "" {
			_ = os.RemoveAll(sockDir)
		}
		_ = os.RemoveAll(dir)
	}()

	// CoW clone of the raw rootfs so the run never mutates the shared base image.
	clone := filepath.Join(dir, "rootfs.img")
	if err := cloneFile(spec.RootFS, clone); err != nil {
		return nil, fmt.Errorf("firecracker: clone rootfs: %w", err)
	}

	// Per-run scratch disk (/dev/vdb in the guest): a sparse raw file sized to DiskGiB.
	// The guest formats + mounts it at /var/lib/containerd (§6.10), so the image import and
	// the agent's working files have room without bloating the rootfs or the base image.
	diskGiB := spec.DiskGiB
	if diskGiB == 0 {
		diskGiB = DefaultDiskGiB
	}
	scratch := filepath.Join(dir, "scratch.img")
	if err := createSparse(scratch, diskGiB<<30); err != nil {
		return nil, fmt.Errorf("firecracker: create scratch disk: %w", err)
	}

	// One atomic allocation covers the tap device, the /30 that addresses it, and the vsock
	// CID — the tap's kernel-global name is what makes it safe across concurrent processes
	// (see allocSlot).
	slot, err = allocSlot(spec.CID)
	if err != nil {
		return nil, fmt.Errorf("firecracker: allocate network slot: %w", err)
	}

	// Keep the API + vsock sockets under a short, private, per-user root: sun_path is capped
	// (108 bytes on Linux) and RunDir lives under the user's cache dir, which can be long.
	// Same hardening as the vfkit provider (§6.12): we verify or create the root ourselves
	// and fail closed rather than place VM control sockets under a directory we don't own.
	sockDir, err = newSockDir()
	if err != nil {
		return nil, err
	}

	v := &vm{
		id:         spec.ID,
		binary:     binary,
		dir:        dir,
		sockDir:    sockDir,
		clone:      clone,
		scratch:    scratch,
		slot:       slot,
		apiSock:    filepath.Join(sockDir, "api.sock"),
		vsockSock:  filepath.Join(sockDir, "vsock.sock"),
		ctrlSock:   filepath.Join(sockDir, "control.sock"),
		consoleLog: filepath.Join(dir, "console.log"),
		kernel:     spec.Kernel,
		initrd:     spec.Initrd,
		cmdline:    bootArgs(spec.Cmdline, slot),
		cpus:       spec.CPUs,
		memoryMiB:  spec.MemoryMiB,
	}
	v.api = newAPIClient(v.apiSock)

	success = true
	return v, nil
}

// sockRoot returns the short base directory for this user's per-VM unix sockets, mirroring
// the vfkit provider's socket-root hardening (§6.12). The uid suffix gives each user their
// own root so two users on a shared host never collide.
func sockRoot() string { return "/tmp/krayt-" + strconv.Itoa(os.Getuid()) }

// newSockDir creates a unique short-pathed directory for a VM's API + vsock sockets under
// this user's socket root.
func newSockDir() (string, error) {
	root := sockRoot()
	if err := ensureSockRoot(root); err != nil {
		return "", err
	}
	d, err := os.MkdirTemp(root, "vm-")
	if err != nil {
		return "", fmt.Errorf("firecracker: create socket dir: %w", err)
	}
	return d, nil
}

// ensureSockRoot makes root a private directory owned by the current user, or fails. It uses
// Lstat (no symlink following) + os.Mkdir (fails if the path already exists), so a symlink or
// a foreign-owned/loose-mode directory pre-placed at root is refused rather than trusted. The
// sockets it guards are the VM's lifecycle API and its control channel, so we never chmod or
// chown a directory we do not own — we fail closed and let the human fix it (§6.12).
func ensureSockRoot(root string) error {
	fi, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(root, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				// Lost a race with a concurrent creator (the root is shared across every VM
				// this user boots) — re-validate whatever now exists rather than failing a
				// legitimate concurrent `krayt run` on a spurious EEXIST.
				return ensureSockRoot(root)
			}
			return fmt.Errorf("firecracker: create socket root: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("firecracker: stat socket root: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("firecracker: socket root %s: cannot read owner/mode", root)
	}
	if !fi.IsDir() || int(st.Uid) != os.Getuid() || fi.Mode().Perm() != 0o700 {
		return fmt.Errorf("firecracker: socket root %s is not a private directory owned by this user "+
			"(mode %o, uid %d); refusing to place VM control sockets there — remove or fix it",
			root, fi.Mode().Perm(), st.Uid)
	}
	return nil
}

// bootArgs derives the guest kernel command line from the caller's cmdline plus the two things
// only this provider knows: which console the guest actually has, and the address of the tap it
// is wired to.
//
// Firecracker gives the guest an 8250 serial port (ttyS0), not vfkit's virtio-console (hvc0),
// and no DHCP server — so the guest's address has to travel on the command line. See
// netSlot.cmdlineArgs for how it gets picked up in the guest.
//
// Any caller-supplied console=/ip=/ifname=/nameserver= is dropped rather than appended to, so a
// VMSpec written for the vfkit backend (console=hvc0) still boots correctly here instead of
// carrying a second, conflicting console.
func bootArgs(cmdline string, slot *netSlot) string {
	kept := make([]string, 0, 8)
	for _, tok := range strings.Fields(cmdline) {
		switch {
		case strings.HasPrefix(tok, "console="),
			strings.HasPrefix(tok, "ip="),
			strings.HasPrefix(tok, "ifname="),
			strings.HasPrefix(tok, "nameserver="):
			continue
		default:
			kept = append(kept, tok)
		}
	}
	if len(kept) == 0 {
		kept = append(kept, "root=/dev/vda")
	}
	kept = append(kept, "console=ttyS0", "reboot=k", "panic=1")
	kept = append(kept, slot.cmdlineArgs()...)
	return strings.Join(kept, " ")
}

// instanceID adapts a krayt run ID to firecracker's --id, which accepts only alphanumerics and
// dashes and panics on anything else. krayt's run IDs look like "run_3f9a1c" — the underscore
// alone is enough to abort the process at startup — so fold every other character to a dash.
// This value is cosmetic (it labels firecracker's log lines); VM.ID() still reports the real
// krayt run ID.
func instanceID(id string) string {
	out := []rune(id)
	for i, r := range out {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !isAlnum && r != '-' {
			out[i] = '-'
		}
	}
	// Firecracker caps the id at 64 characters.
	if len(out) > 64 {
		out = out[:64]
	}
	return string(out)
}

// createSparse creates a sparse file of the given size in bytes (no blocks allocated until
// written). Used for the per-run scratch disk.
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
	scratch    string
	slot       *netSlot
	apiSock    string
	vsockSock  string // firecracker's vsock UDS (needs the CONNECT handshake)
	ctrlSock   string // our bridge: a plain unix socket that speaks gRPC (see bridge.go)
	consoleLog string
	kernel     string
	initrd     string
	cmdline    string
	cpus       int
	memoryMiB  uint64
	api        *apiClient

	cmd    *exec.Cmd
	bridge *bridge
}

// LogPaths returns the firecracker and guest-console log paths, for diagnostics. Firecracker
// writes both its own log and the guest's ttyS0 output to its stdout/stderr, so they land in
// one file; the second return value names it for both, matching the vfkit provider's shape.
func (v *vm) LogPaths() (fcLog, consoleLog string) {
	return v.consoleLog, v.consoleLog
}

// tailLog returns the last few KiB of firecracker's output, for folding into a start-up error.
// Boot failures are otherwise near-undebuggable: the interesting message is on the guest
// console, and Destroy removes the run dir on the way out.
func (v *vm) tailLog() string {
	const maxTail = 4 << 10
	b, err := os.ReadFile(v.consoleLog)
	if err != nil {
		return "(console log unavailable: " + err.Error() + ")"
	}
	if len(b) > maxTail {
		b = b[len(b)-maxTail:]
	}
	return string(b)
}

// Start launches firecracker, pushes the machine configuration over its REST API, and issues
// InstanceStart. It returns once the guest has been told to boot; boot readiness (the
// guest-agent answering Hello) is established by the host control client, not here (§11.6).
func (v *vm) Start(ctx context.Context) error {
	if v.cmd != nil {
		return fmt.Errorf("firecracker: vm %s already started", v.id)
	}

	// Firecracker writes the guest serial console to its own stdout, so this file is both
	// the VM log and the guest console — the only boot diagnostic we get.
	logf, err := os.Create(v.consoleLog)
	if err != nil {
		return fmt.Errorf("firecracker: open console log: %w", err)
	}
	cmd := exec.Command(v.binary, "--api-sock", v.apiSock, "--id", instanceID(v.id))
	cmd.Stdout = logf
	cmd.Stderr = logf
	// Own the child's process group so a kill reaps firecracker even if it has spawned
	// helpers, and so it does not take our terminal's signals.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return fmt.Errorf("firecracker: start subprocess: %w", err)
	}
	// The child inherited the log FD at Start; close the parent's copy so it isn't leaked
	// across runs (exec.Cmd only closes files it created itself).
	_ = logf.Close()
	v.cmd = cmd

	if err := v.configureAndBoot(ctx); err != nil {
		// A half-configured firecracker is useless and would otherwise linger as an orphan.
		_ = v.kill()
		return err
	}

	// The control channel is a plain unix socket to everything above the provider — including
	// `krayt answer`/`stop`, which dial the recorded ControlSocket() path from a separate
	// process. Firecracker's vsock UDS needs a CONNECT handshake first, so bridge it (§6.12).
	b, err := newBridge(v.ctrlSock, v.vsockSock, provider.ControlPort)
	if err != nil {
		_ = v.kill()
		return err
	}
	v.bridge = b
	return nil
}

// configureAndBoot waits for the API socket, PUTs the full machine configuration, and starts
// the instance. Firecracker accepts configuration in any order but requires it all before
// InstanceStart.
func (v *vm) configureAndBoot(ctx context.Context) error {
	if err := v.api.waitReady(ctx, 10*time.Second); err != nil {
		// Firecracker refuses some configuration before it ever binds the socket (a bad --id
		// makes it panic outright), and Destroy will shortly delete the log. Fold what it said
		// into the error, or the caller is left with a bare "no such file or directory".
		return fmt.Errorf("firecracker: API socket never appeared: %w\n--- firecracker output ---\n%s",
			err, v.tailLog())
	}
	if err := v.api.setMachineConfig(ctx, v.cpus, v.memoryMiB); err != nil {
		return err
	}
	if err := v.api.setBootSource(ctx, v.kernel, v.initrd, v.cmdline); err != nil {
		return err
	}
	// The rootfs must be the first drive so it enumerates as /dev/vda (the cmdline's root=),
	// and the scratch disk second as /dev/vdb (the guest mounts it at /var/lib/containerd).
	if err := v.api.setDrive(ctx, "rootfs", v.clone, true, false); err != nil {
		return err
	}
	if err := v.api.setDrive(ctx, "scratch", v.scratch, false, false); err != nil {
		return err
	}
	if err := v.api.setNetworkInterface(ctx, "eth0", v.slot.tapName(), v.slot.guestMAC()); err != nil {
		return err
	}
	if err := v.api.setVsock(ctx, v.slot.cid, v.vsockSock); err != nil {
		return err
	}
	return v.api.instanceStart(ctx)
}

// DialControl opens the control channel to the guest-agent. On Firecracker this is a unix
// dial to the VM's vsock socket followed by the "CONNECT <port>\n" handshake, which
// firecracker forwards to whatever is listening on that AF_VSOCK port in the guest (§6.12).
//
// The guest-agent may not be listening yet while the VM is still booting; in that case
// firecracker closes the connection and this returns an error. That is expected and correct:
// gRPC calls the dialer again on each reconnect, so the boot-readiness poll in
// controlclient.WaitReady drives the retry (§11.6).
func (v *vm) DialControl(ctx context.Context, port uint32) (net.Conn, error) {
	return dialVsock(ctx, v.vsockSock, port)
}

// ControlSocket returns the host-side control socket path, which the orchestrator records so
// a later `krayt answer`/`stop` can dial this run's guest directly (§6.2, §6.13). This is the
// bridge socket, not firecracker's raw vsock socket, so the caller gets a plain gRPC
// transport exactly as it does on vfkit.
func (v *vm) ControlSocket() string { return v.ctrlSock }

// Stop shuts the guest down gracefully and waits for the process to exit. Firecracker has no
// "power off" API: the closest is SendCtrlAltDel, which systemd in the guest turns into an
// orderly shutdown, and firecracker exits when the guest resets. If that does not land inside
// stopTimeout we kill the process — the VM is ephemeral and its artifacts have already been
// collected, so a hard kill is a fallback, not data loss.
func (v *vm) Stop(ctx context.Context) error {
	if v.cmd == nil {
		return nil
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()

	if err := v.api.sendCtrlAltDel(stopCtx); err != nil {
		return v.kill()
	}
	if err := v.wait(stopCtx); err != nil {
		return v.kill()
	}
	return nil
}

// wait reaps the firecracker process, bounded by ctx.
func (v *vm) wait(ctx context.Context) error {
	if v.cmd == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- v.cmd.Wait() }()
	select {
	case err := <-done:
		v.cmd = nil
		if err != nil && !isExpectedExit(err) {
			return fmt.Errorf("firecracker: process exit: %w", err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// kill terminates firecracker and reaps it. It is the teardown path of last resort, so it
// signals the whole process group and does not give up on a slow exit.
func (v *vm) kill() error {
	if v.cmd == nil || v.cmd.Process == nil {
		return nil
	}
	// Negative pid signals the process group we created in Start, so nothing survives us.
	if err := syscall.Kill(-v.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = v.cmd.Process.Kill()
	}
	_ = v.cmd.Wait()
	v.cmd = nil
	return nil
}

// Destroy stops the VM and releases everything Create acquired: the tap device, the socket
// dir, and the run dir holding the CoW clone + scratch disk. Every step runs even if an
// earlier one fails — a leaked tap or a multi-GB clone is worse than a lost error.
func (v *vm) Destroy(ctx context.Context) error {
	var errs []error

	if v.cmd != nil {
		if err := v.Stop(ctx); err != nil {
			errs = append(errs, err)
		}
		// Stop falls back to kill internally, but if the graceful path reported success while
		// the process somehow lingers, make sure it is gone before we pull its disks away.
		_ = v.kill()
	}
	if v.bridge != nil {
		_ = v.bridge.close()
		v.bridge = nil
	}
	if v.slot != nil {
		if err := v.slot.destroy(); err != nil {
			errs = append(errs, fmt.Errorf("firecracker: remove tap: %w", err))
		}
		v.slot = nil
	}
	if v.sockDir != "" {
		if err := os.RemoveAll(v.sockDir); err != nil {
			errs = append(errs, fmt.Errorf("firecracker: remove socket dir: %w", err))
		}
		v.sockDir = ""
	}
	if err := os.RemoveAll(v.dir); err != nil {
		errs = append(errs, fmt.Errorf("firecracker: remove run dir: %w", err))
	}
	return errors.Join(errs...)
}

func (v *vm) ID() string { return v.id }

// isExpectedExit reports whether a finished firecracker process exited the way teardown
// expects: cleanly (a guest-initiated reset exits 0) or terminated by our own signal. A
// non-zero, non-signal exit is a real firecracker failure (bad config, KVM error) and must
// surface rather than be swallowed by wait().
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
