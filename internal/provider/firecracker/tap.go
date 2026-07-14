//go:build linux

package firecracker

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Firecracker gives a VM a bare tap interface: no NAT device, no DHCP server, nothing else
// (unlike vfkit, whose virtio-net NAT device supplies all three). So the provider owns the
// host end of the wire, and a "network slot" is the unit it hands out — one tap device, the
// /30 that addresses it, and the vsock CID, all derived from a single index.
//
// Each VM gets its own /30 rather than sharing one bridged subnet. That means the host holds
// one address per live VM and there is no shared L2 between guests: two concurrent runs
// cannot see each other's traffic at all, which is the property we actually want (§10). The
// in-guest egress proxy + nftables lock (§6.6) are untouched by any of this — they police
// what leaves the guest, while this only decides what the wire is.
//
// The host still needs IP forwarding and a NAT masquerade rule for kraytSubnet before a guest
// can reach the internet. That is one-time host state, not per-VM, so it lives in
// hack/linux-net-setup.sh and `krayt doctor` checks it.
const (
	// maxSlots caps concurrent VMs. 172.16.0.0/16 holds far more /30s than this; the limit is
	// really about not scanning forever when something is wrong.
	maxSlots = 256

	// tapPrefix names the host tap devices. Kernel interface names are capped at 15 bytes, so
	// this leaves ample room for the index.
	tapPrefix = "krayt"

	// guestIface is the name the guest's NIC is pinned to. The provider passes
	// `ifname=<name>:<mac>`, which systemd-network-generator turns into a .link file that renames
	// the device by MAC before networkd runs — so the generated .network's [Match] cannot miss it,
	// whatever predictable-interface-naming would otherwise have called it.
	guestIface = "eth0"
)

// netSlot is one VM's host-side networking + vsock identity. It is held for the VM's lifetime
// and released by destroy().
type netSlot struct {
	index uint32
	cid   uint32

	// lock is an flock'd file held open for the slot's lifetime. It — not the tap device — is
	// what makes allocation safe across concurrent krayt processes (see allocSlot).
	lock *os.File
}

// allocSlot claims a free slot and creates its tap device.
//
// Allocation has to be atomic across *processes*, not just goroutines: `krayt run --detach`
// re-execs a per-run supervisor, so several independent krayt processes can be booting VMs at
// once (§6.2). The tap device's name cannot itself be the lock — a persistent tap with no
// attached fd is re-attachable, and we must detach from it so that firecracker can attach —
// so each slot is guarded by an flock'd file instead, the same cross-process idiom the
// orchestrator's concurrency semaphore uses. The kernel drops the lock if the holder dies, so
// a crashed run's slot is reclaimed automatically (and its stale tap is recreated below).
//
// preferredCID, if non-zero, honours VMSpec.CID. Note that a CID collision between two VMs
// would be harmless here regardless: firecracker's vsock is backed by a per-VM unix socket, so
// unlike a host AF_VSOCK there is no shared CID namespace to collide in (§6.12). Deriving the
// CID from the slot index keeps them unique anyway, which makes a VM identifiable in a trace.
func allocSlot(preferredCID uint32) (*netSlot, error) {
	dir := filepath.Join(sockRoot(), "slots")
	if err := ensureSockRoot(sockRoot()); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create slot dir: %w", err)
	}

	var busy int
	for i := range uint32(maxSlots) {
		f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("%d.lock", i)), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open slot lock: %w", err)
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			_ = f.Close() // held by another live VM
			continue
		}

		s := &netSlot{index: i, cid: 3 + i, lock: f}
		if preferredCID != 0 {
			s.cid = preferredCID
		}
		err = s.createTAP()
		if err == nil {
			return s, nil
		}
		_ = f.Close() // release the slot lock we just took

		// EBUSY means the slot's tap device still has something attached to it while its lock is
		// free — an orphaned firecracker whose krayt process died. That is one unusable slot, not
		// a reason to refuse the whole run, so step over it. Anything else (EPERM, no /dev/net/tun)
		// is a real fault that would recur on every slot, and failing 256 times before saying so
		// would only bury the cause.
		if !errors.Is(err, unix.EBUSY) {
			return nil, err
		}
		busy++
	}
	if busy > 0 {
		return nil, fmt.Errorf("no free network slot: all %d are taken, %d of them by a tap device "+
			"still in use by an orphaned process (check for stray `firecracker` processes)", maxSlots, busy)
	}
	return nil, fmt.Errorf("no free network slot (%d VMs already running)", maxSlots)
}

func (s *netSlot) tapName() string { return fmt.Sprintf("%s%d", tapPrefix, s.index) }

// hostIP / guestIP carve a /30 out of 172.16.0.0/16, one per slot:
// slot i owns 172.16.<i/64>.<(i%64)*4>/30 — network, host, guest, broadcast.
func (s *netSlot) hostIP() net.IP  { return s.addr(1) }
func (s *netSlot) guestIP() net.IP { return s.addr(2) }

func (s *netSlot) addr(offset byte) net.IP {
	return net.IPv4(172, 16, byte(s.index/64), byte((s.index%64)*4)+offset)
}

// netmask is the /30 that every slot's subnet uses.
func (s *netSlot) netmask() net.IP { return net.IPv4(255, 255, 255, 252) }

// guestMAC derives a stable, locally-administered MAC from the slot index (the 02: prefix
// marks it locally administered, so it cannot collide with real hardware).
func (s *netSlot) guestMAC() string {
	return fmt.Sprintf("02:fc:00:00:%02x:%02x", byte(s.index>>8), byte(s.index))
}

// cmdlineArgs renders the guest's network configuration as kernel command-line parameters.
// This is the only way the guest can learn its address: a firecracker tap has no DHCP server
// behind it (vfkit's NAT device does, which is why the vfkit path needs none of this).
//
// A caveat worth stating, because it is the whole reason this works: these are *not* consumed
// by the kernel's own `ip=` autoconfiguration. That needs CONFIG_IP_PNP, which the nixpkgs
// kernel does not enable, so the kernel ignores the parameter entirely and the guest comes up
// with no address at all. They are consumed in userspace by systemd-network-generator, which
// parses the same dracut syntax and writes /run/systemd/network/70-eth0.{link,network} before
// udev and networkd start (the image enables that unit; see images/flake.nix).
//
//   - ifname= pins the NIC to a known name by MAC, so the generated .network's [Match] cannot
//     miss it if predictable-interface-naming decides to rename the device.
//   - ip=<client>:<server>:<gateway>:<netmask>:<hostname>:<device>:<autoconf> — "off" means a
//     static address rather than DHCP.
//   - nameserver= gives resolved an upstream, which the egress proxy needs to resolve the
//     task's allowlist (§6.6).
func (s *netSlot) cmdlineArgs() []string {
	return []string{
		fmt.Sprintf("ifname=%s:%s", guestIface, s.guestMAC()),
		fmt.Sprintf("ip=%s::%s:%s::%s:off", s.guestIP(), s.hostIP(), s.netmask(), guestIface),
		"nameserver=1.1.1.1",
		"nameserver=8.8.8.8",
	}
}

// createTAP creates the persistent tap device and brings it up with the host's /30 address.
//
// This needs CAP_NET_ADMIN. krayt is expected to carry it as a file capability
// (`setcap cap_net_admin+ep`), not to run as root — see hack/linux-net-setup.sh and the
// doctor check. Note that a file capability is NOT inherited by children, which is why the
// addressing below is done with ioctls in-process rather than by shelling out to `ip`.
func (s *netSlot) createTAP() error {
	// A slot reclaimed from a crashed run may still have its tap lying around. We hold the
	// slot's lock, so the device is ours to remove.
	_ = s.deleteTAP()

	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open /dev/net/tun: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	ifr, err := unix.NewIfreq(s.tapName())
	if err != nil {
		return err
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		// Name the actual cause. Blaming CAP_NET_ADMIN unconditionally sends someone chasing a
		// permissions problem that isn't there when the real answer is a leftover device — and the
		// error has to keep wrapping the errno, because allocSlot decides whether to step over
		// this slot by testing for EBUSY.
		if errors.Is(err, unix.EPERM) {
			return fmt.Errorf("create tap %s: %w (krayt needs CAP_NET_ADMIN — see `krayt doctor`)",
				s.tapName(), err)
		}
		return fmt.Errorf("create tap %s: %w", s.tapName(), err)
	}
	// Persist the device so it outlives this fd: firecracker attaches to it by name, and a
	// tap can only carry one attached fd, so we must let go of ours before it starts.
	if err := unix.IoctlSetInt(fd, unix.TUNSETPERSIST, 1); err != nil {
		return fmt.Errorf("persist tap %s: %w", s.tapName(), err)
	}
	// Hand the device to this user, so the *firecracker* process — which runs unprivileged and
	// merely attaches to the tap by name — can open it without CAP_NET_ADMIN of its own. Only
	// krayt needs the capability, and only to get to this point.
	if err := unix.IoctlSetInt(fd, unix.TUNSETOWNER, os.Getuid()); err != nil {
		return fmt.Errorf("set owner on tap %s: %w", s.tapName(), err)
	}
	if err := unix.IoctlSetInt(fd, unix.TUNSETGROUP, os.Getgid()); err != nil {
		return fmt.Errorf("set group on tap %s: %w", s.tapName(), err)
	}

	if err := s.configureTAP(); err != nil {
		_ = s.deleteTAP()
		return err
	}
	return nil
}

// configureTAP assigns the host-side address and brings the link up, via the classic
// SIOCSIF* ioctls on an AF_INET socket. The connected route for the /30 is installed by the
// kernel when the address is set, so there is no route to add: the host reaches the guest,
// and the one-time masquerade rule takes it from there.
func (s *netSlot) configureTAP() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open netdev socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	setAddr := func(req uint, ip net.IP) error {
		ifr, err := unix.NewIfreq(s.tapName())
		if err != nil {
			return err
		}
		if err := ifr.SetInet4Addr(ip.To4()); err != nil {
			return err
		}
		return unix.IoctlIfreq(fd, req, ifr)
	}
	if err := setAddr(unix.SIOCSIFADDR, s.hostIP()); err != nil {
		return fmt.Errorf("set address on %s: %w", s.tapName(), err)
	}
	if err := setAddr(unix.SIOCSIFNETMASK, s.netmask()); err != nil {
		return fmt.Errorf("set netmask on %s: %w", s.tapName(), err)
	}

	ifr, err := unix.NewIfreq(s.tapName())
	if err != nil {
		return err
	}
	ifr.SetUint16(unix.IFF_UP | unix.IFF_RUNNING)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("bring up %s: %w", s.tapName(), err)
	}
	return nil
}

// deleteTAP removes the persistent tap device. Clearing TUNSETPERSIST on the last attached fd
// destroys it. If the device does not exist, TUNSETIFF creates it and we immediately drop it
// again — harmless, and it keeps this idempotent.
func (s *netSlot) deleteTAP() error {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open /dev/net/tun: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	ifr, err := unix.NewIfreq(s.tapName())
	if err != nil {
		return err
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		// EBUSY means something still has the device attached — firecracker, if it somehow
		// outlived us. The caller kills the process before releasing the slot, so this is a
		// real error worth reporting rather than swallowing.
		if errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("tap %s still in use: %w", s.tapName(), err)
		}
		return fmt.Errorf("attach tap %s for removal: %w", s.tapName(), err)
	}
	if err := unix.IoctlSetInt(fd, unix.TUNSETPERSIST, 0); err != nil {
		return fmt.Errorf("unpersist tap %s: %w", s.tapName(), err)
	}
	return nil
}

// destroy removes the tap and releases the slot lock, freeing the index for the next VM.
func (s *netSlot) destroy() error {
	err := s.deleteTAP()
	if s.lock != nil {
		// Closing the fd drops the flock.
		_ = s.lock.Close()
		s.lock = nil
	}
	return err
}
