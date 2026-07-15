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

// natUnit is the systemd unit hack/linux-net-setup.sh installs to apply krayt's nftables rules at
// boot. The rules live only in the kernel otherwise, so this unit is what makes them survive one.
const natUnit = "krayt-nat.service"

// natCheck reports whether the host is set up to route the guests' traffic out. That takes two
// things — IPv4 forwarding, and krayt's nftables masquerade/forward rules — and both are one-time
// host state, not something krayt configures per run. A gap is a warning rather than a failure: a
// task that needs no egress runs fine without it, and what a guest is *allowed* to reach is
// enforced in-VM by the proxy either way (§6.6).
//
// Checking ip_forward alone would be worse than not checking at all, because it reports [ok] in
// exactly the two cases most likely to be broken. Forwarding is already on for anything running
// Docker, so on its own it says nothing about krayt. And krayt's rules live only in the kernel
// until krayt-nat.service re-applies them, so on a rebooted host forwarding is still on (it is
// persisted via sysctl.d) while the masquerade is gone — a host that looks configured and whose
// guests silently have no network.
//
// The rules themselves cannot be read from here: listing nftables needs CAP_NET_ADMIN, and krayt's
// is a *file* capability, which a child `nft` process would not inherit. So we check the unit that
// owns them, which any user may query. Two blind spots remain, and they are worth naming rather
// than papering over:
//   - if someone flushes the tables by hand while the unit stays active, this still reports [ok].
//   - Docker's own FORWARD-chain policy (DROP by default at dockerd startup) is a separate base
//     chain from krayt's, hooked at the same nftables priority; a DROP in either is terminal
//     regardless of the other, so krayt's own accept rules do nothing to help here.
//     hack/linux-net-setup.sh handles it (an explicit accept in Docker's own DOCKER-USER chain,
//     the customization point Docker itself documents for this), but this check has no way to
//     confirm that rule survived — same CAP_NET_ADMIN blind spot as above — so a host can look
//     fully [ok] here while guest egress is still silently dropped by Docker underneath it.
//     Confirmed on real hardware, not theoretical: a CI runner with dockerd running dropped every
//     bit of krayt's tap-forwarded egress this way. Not specific to that runner or to CI — any
//     Linux host running both Docker and krayt hits it.
func natCheck() checkResult {
	c := checkResult{name: "host NAT for guest egress", optional: true}

	fwd, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil || strings.TrimSpace(string(fwd)) != "1" {
		c.detail = "IPv4 forwarding is off, so guests cannot reach the network — " +
			"run `sudo hack/linux-net-setup.sh` once"
		return c
	}

	// `systemctl is-active` exits non-zero when the unit is not active, so a non-nil err is the
	// normal way it says "inactive". The state it prints is the answer; only a missing systemctl
	// is a real error.
	out, err := exec.Command("systemctl", "is-active", natUnit).CombinedOutput()
	if errors.Is(err, exec.ErrNotFound) {
		c.detail = "IPv4 forwarding is on, but this host has no systemd, so krayt cannot confirm its " +
			"NAT rules are applied — check them by hand (see hack/linux-net-setup.sh)"
		return c
	}
	if state := strings.TrimSpace(string(out)); state != "active" {
		c.detail = fmt.Sprintf("IPv4 forwarding is on, but %s is %q, so krayt's NAT masquerade is not "+
			"applied and guests have no egress — forwarding alone is not enough, and the rules do not "+
			"survive a reboot without this unit. Run `sudo hack/linux-net-setup.sh` once",
			natUnit, state)
		return c
	}

	c.ok = true
	c.detail = "IPv4 forwarding on, " + natUnit + " active"
	if dockerActive() {
		c.detail += "; Docker is also running — its default FORWARD-chain policy is DROP, a " +
			"separate base chain from krayt's own that nftables evaluates independently, so guest " +
			"egress is silently dropped unless hack/linux-net-setup.sh's DOCKER-USER accept rule is " +
			"in place. Re-run it if Docker was installed/started after the last time you ran it — " +
			"this check cannot itself confirm the rule survived (listing it needs CAP_NET_ADMIN a " +
			"child process would not inherit from krayt's file capability, the same blind spot as " +
			"krayt's own rules above)"
	}
	return c
}

// dockerActive is a best-effort, unprivileged hint for natCheck's Docker note above — never a
// check failure on its own, since we cannot confirm from here whether DOCKER-USER's accept rule
// is actually in place (see natCheck's own doc comment on why: reading nftables/iptables state
// needs CAP_NET_ADMIN a child process does not inherit). False on any error, including no
// systemd, rather than "unknown" — this is purely informational.
func dockerActive() bool {
	out, err := exec.Command("systemctl", "is-active", "docker").CombinedOutput()
	return err == nil && strings.TrimSpace(string(out)) == "active"
}
