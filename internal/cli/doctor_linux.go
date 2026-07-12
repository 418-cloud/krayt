//go:build linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

// hostChecks verifies the Linux prerequisites for the firecracker provider (§13, Phase 7):
// KVM has to be present *and* usable by this user, the firecracker binary has to be
// installed, and krayt needs CAP_NET_ADMIN plus a NAT'd host to give the VM a network.
func hostChecks() []checkResult {
	return []checkResult{
		kvmCheck(),
		firecrackerCheck(),
		tunCheck(),
		netAdminCheck(),
		natCheck(),
	}
}

// kvmCheck tests read/write access, not just presence. Being in the `kvm` group is not enough
// on its own: supplementary groups are applied at login, so a user added to the group during a
// live session still gets EACCES until they start a new one — a genuinely confusing failure,
// so the message names it outright.
func kvmCheck() checkResult {
	c := checkResult{name: "/dev/kvm present + accessible"}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		c.detail = "/dev/kvm not found — the firecracker provider needs KVM " +
			"(on a cloud VM, check that nested virtualization is enabled)"
		return c
	}
	fd, err := unix.Open("/dev/kvm", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.EACCES) {
			c.detail = "/dev/kvm exists but is not readable/writable by this user — add yourself to " +
				"the `kvm` group (`sudo usermod -aG kvm $USER`), then start a NEW login session: " +
				"group membership only takes effect at login, so an existing shell stays denied"
			return c
		}
		c.detail = "/dev/kvm cannot be opened: " + err.Error()
		return c
	}
	_ = unix.Close(fd)
	c.ok = true
	return c
}

func firecrackerCheck() checkResult {
	c := checkResult{name: "firecracker installed + runnable"}
	path, err := exec.LookPath("firecracker")
	if err != nil {
		c.detail = "firecracker not found on PATH — install a release from " +
			"https://github.com/firecracker-microvm/firecracker/releases"
		return c
	}
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		c.detail = "firecracker found at " + path + " but `firecracker --version` failed: " + err.Error()
		return c
	}
	// `firecracker --version` prints the version first, then a log line; keep the first line.
	version, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	c.ok = true
	c.detail = path + " (" + strings.TrimSpace(version) + ")"
	return c
}

func tunCheck() checkResult {
	c := checkResult{name: "/dev/net/tun present"}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		c.detail = "/dev/net/tun not found — krayt needs it to create the VM's tap device (`sudo modprobe tun`)"
		return c
	}
	c.ok = true
	return c
}

// netAdminCheck verifies krayt can actually create a tap device. Firecracker, unlike vfkit,
// has no built-in NAT device — the provider creates and addresses the tap itself, which needs
// CAP_NET_ADMIN. The intended way to grant it is a file capability on the binary, so krayt
// does not have to run as root.
func netAdminCheck() checkResult {
	c := checkResult{name: "CAP_NET_ADMIN (tap device creation)"}
	if os.Geteuid() == 0 {
		c.ok = true
		c.detail = "running as root"
		return c
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		c.detail = "cannot read process capabilities: " + err.Error()
		return c
	}
	// CAP_NET_ADMIN is bit 12, in the first 32-bit capability word.
	const capNetAdmin = 12
	if data[0].Effective&(1<<capNetAdmin) == 0 {
		self, err := os.Executable()
		if err != nil {
			self = "$(which krayt)"
		}
		c.detail = fmt.Sprintf("krayt lacks CAP_NET_ADMIN, so it cannot create the VM's tap device — "+
			"grant it once with `sudo setcap cap_net_admin+ep %s` (see hack/linux-net-setup.sh)", self)
		return c
	}
	c.ok = true
	return c
}

// natCheck reports whether the host is set up to route the guests' traffic out: IP forwarding
// plus a masquerade rule for krayt's subnet. That is one-time host state, not something krayt
// configures per run, so a missing rule is a warning rather than a failure — a task that needs
// no egress runs fine without it, and what the guest is *allowed* to reach is enforced in-VM by
// the proxy either way (§6.6).
func natCheck() checkResult {
	c := checkResult{name: "host NAT for guest egress", optional: true}
	fwd, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil || strings.TrimSpace(string(fwd)) != "1" {
		c.detail = "IP forwarding is off, so guests cannot reach the network — " +
			"run hack/linux-net-setup.sh once"
		return c
	}
	c.ok = true
	return c
}
