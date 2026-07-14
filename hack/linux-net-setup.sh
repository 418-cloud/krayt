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

# 3. The nftables rules, written to a file and applied by a systemd unit at boot.
#
#    Persistence is the point. Applying them with a bare `nft -f` here would work until the next
#    reboot and then quietly stop: `ip_forward` above survives (sysctl.d) but the tables would
#    not, leaving a host that looks configured — forwarding on, `krayt doctor` happy — while every
#    guest silently has no egress. `nftables.service` is not a reliable home for them either (it
#    ships disabled on Ubuntu, and /etc/nftables.conf belongs to the distro, not to us), so krayt
#    brings its own unit.
#
#    Both tables are krayt's own, so they are self-contained and removable
#    (`systemctl disable --now krayt-nat && nft delete table ip krayt_nat`) without touching any
#    other firewall rules on the host.
#
#    krayt_nat masquerades guest traffic behind the host's address. krayt_fwd exists because many
#    hosts default the filter FORWARD chain to drop, which would silently break guest egress.
NFT="$(command -v nft)"
install -d /etc/krayt
cat > /etc/krayt/nat.nft <<EOF
#!${NFT} -f
# Managed by krayt (hack/linux-net-setup.sh). Applied at boot by krayt-nat.service.
#
# The create/delete/create dance makes this file idempotent: declaring the table creates it if
# absent, so the delete cannot fail on a fresh boot, and re-applying never stacks duplicate rules.
table ip krayt_nat
delete table ip krayt_nat
table ip krayt_nat {
  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
    ip saddr $SUBNET oifname != "krayt*" masquerade
  }
}

table ip krayt_fwd
delete table ip krayt_fwd
table ip krayt_fwd {
  chain forward {
    type filter hook forward priority filter; policy accept;
    ip saddr $SUBNET accept
    ip daddr $SUBNET ct state established,related accept
  }
}
EOF

cat > /etc/systemd/system/krayt-nat.service <<EOF
[Unit]
Description=krayt: NAT + forwarding for micro-VM guest egress
Documentation=https://github.com/418-cloud/krayt
After=network-pre.target
Wants=network-pre.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=${NFT} -f /etc/krayt/nat.nft
ExecStop=${NFT} delete table ip krayt_nat
ExecStop=${NFT} delete table ip krayt_fwd

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now krayt-nat.service
echo "installed NAT masquerade + forward rules for $SUBNET"
echo "  rules:   /etc/krayt/nat.nft"
echo "  applied: krayt-nat.service (enabled — survives reboot)"

echo
echo "done — now run: krayt doctor"
