#!/usr/bin/env bash
# One-time host setup for the Linux (firecracker) backend — §6.6, §14 Phase 7.
#
# Why this exists at all: vfkit hands a macOS VM a NAT'd NIC with a DHCP server built in, so
# the host needs no setup. Firecracker does not. It gives the VM a bare tap interface and
# nothing else, so the *host* has to route the guests' traffic out. That takes privileges krayt
# should not hold at run time, so it is a one-time root step instead of something the provider
# does per run.
#
# This grants exactly two things:
#
#   1. CAP_NET_ADMIN on the krayt binary, as a file capability — so krayt can create and
#      address each VM's tap device without running as root. (A file capability is not
#      inherited by child processes, which is why krayt does the addressing with ioctls
#      in-process rather than shelling out to `ip`.)
#
#   2. IP forwarding + a NAT masquerade rule for krayt's subnet, so a guest can reach the
#      internet through the host's uplink.
#
# None of this weakens the guest's egress policy: what a container is *allowed* to reach is
# still enforced inside the VM by the allowlist proxy and the nftables lock (§6.6). This only
# provides the wire.
#
# Usage:  sudo hack/linux-net-setup.sh [path-to-krayt-binary]
# Verify: krayt doctor
set -euo pipefail

# krayt gives each VM its own /30 out of this range (see internal/provider/firecracker/tap.go).
SUBNET="172.16.0.0/16"

if [[ $EUID -ne 0 ]]; then
  echo "error: run me with sudo (I set a file capability and a host firewall rule)" >&2
  exit 1
fi

KRAYT="${1:-$(command -v krayt || true)}"
if [[ -z "$KRAYT" || ! -x "$KRAYT" ]]; then
  echo "error: could not find the krayt binary; pass its path: sudo $0 /path/to/krayt" >&2
  exit 1
fi

# 1. CAP_NET_ADMIN on the binary, so krayt can make tap devices as an unprivileged user.
setcap cap_net_admin+ep "$KRAYT"
echo "granted CAP_NET_ADMIN to $KRAYT"

# 2. IP forwarding, persisted so it survives a reboot.
sysctl -w net.ipv4.ip_forward=1 >/dev/null
install -d /etc/sysctl.d
echo "net.ipv4.ip_forward = 1" > /etc/sysctl.d/99-krayt.conf
echo "enabled IPv4 forwarding (persisted in /etc/sysctl.d/99-krayt.conf)"

# 3. Masquerade guest traffic behind the host's address. Uses nftables in its own table so it
#    is self-contained and removable (`nft delete table ip krayt_nat`) without touching any
#    other firewall rules on the host.
nft list table ip krayt_nat >/dev/null 2>&1 && nft delete table ip krayt_nat
nft -f - <<EOF
table ip krayt_nat {
  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
    ip saddr $SUBNET oifname != "krayt*" masquerade
  }
}
EOF
echo "installed NAT masquerade for $SUBNET (nft table ip krayt_nat)"

# 4. Guests must be able to talk to the host end of their own /30 and out through it. Many
#    hosts default the filter FORWARD chain to drop, which would silently break guest egress.
if nft list table ip krayt_fwd >/dev/null 2>&1; then nft delete table ip krayt_fwd; fi
nft -f - <<EOF
table ip krayt_fwd {
  chain forward {
    type filter hook forward priority filter; policy accept;
    ip saddr $SUBNET accept
    ip daddr $SUBNET ct state established,related accept
  }
}
EOF
echo "installed forward rules for $SUBNET (nft table ip krayt_fwd)"

echo
echo "done — now run: krayt doctor"
