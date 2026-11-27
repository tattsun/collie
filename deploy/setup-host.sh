#!/usr/bin/env bash
# One-time host preparation for Collie. Run on the server with sudo:
#
#   sudo ./deploy/setup-host.sh [--user collie] [--port 7878] [--preview-ports 4] [--allow CIDR]... [--public]
#
# It works on systemd-based distributions with apt, dnf, zypper or pacman
# (Debian, Ubuntu, Fedora, RHEL and its rebuilds, openSUSE, Arch …). It
#   1. installs podman (rootless containers) and nftables,
#   2. creates a dedicated user (default: collie) that will run Collie and all
#      agents, with lingering so its services keep running, and makes sure
#      its containers can get resource limits,
#   3. installs a firewall rule: every process of that user — including the
#      project containers — may reach the internet but NOT your LAN (private
#      addresses, and the networks this server is attached to), your tailnet,
#      link-local addresses, or any of this server's own addresses, Collie's
#      ports included. (--allow adds exceptions, e.g. --allow 192.168.1.20/32.)
#   4. with --public (for `mise run install --domain …`): lets unprivileged
#      users bind ports 80 and up, so Collie can serve HTTPS itself, opens
#      80/443 (and 8443 and up, for previews) in ufw or firewalld if active,
#      and keeps agents off those ports.
#
# Then switch to that user and install Collie itself (see docs/setup.md).
set -euo pipefail

user=collie
port=7878
preview_ports=4
allow=()
public=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --user) user="$2"; shift 2 ;;
    --port) port="$2"; shift 2 ;;
    --preview-ports) preview_ports="$2"; shift 2 ;;
    --allow) allow+=("$2"); shift 2 ;;
    --public) public=1; shift ;;
    -h|--help) sed -n '2,21p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

[[ $EUID -eq 0 ]] || { echo "run with sudo" >&2; exit 1; }
[[ "$(uname -s)" == Linux ]] || { echo "this script prepares a Linux server (on a Mac, see docs/setup.md)" >&2; exit 1; }
if [[ ! -d /run/systemd/system ]]; then
  echo "Collie's setup needs systemd (the service, lingering and the firewall are systemd units)." >&2
  exit 1
fi

# shellcheck source=/dev/null
. /etc/os-release
echo "== packages (${PRETTY_NAME:-unknown system})"
# newuidmap/newgidmap (rootless podman) and machinectl come in different
# packages per distribution.
common=(podman passt nftables git)
command -v curl >/dev/null 2>&1 || common+=(curl)  # RHEL's curl-minimal conflicts with curl
if command -v apt-get >/dev/null 2>&1; then
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y "${common[@]}" uidmap systemd-container ca-certificates
elif command -v dnf >/dev/null 2>&1; then
  dnf install -y "${common[@]}" shadow-utils systemd-container ca-certificates
elif command -v zypper >/dev/null 2>&1; then
  zypper --non-interactive install "${common[@]}" shadow systemd-container ca-certificates
elif command -v pacman >/dev/null 2>&1; then
  # (machinectl is part of systemd here)
  pacman -S --needed --noconfirm "${common[@]}" shadow ca-certificates || {
    echo "pacman failed; if packages were not found, update first (pacman -Syu) and run this again" >&2
    exit 1
  }
else
  echo "warning: no apt-get, dnf, zypper or pacman; install these yourself:" >&2
  echo "  podman (5+), passt, nftables, git, curl, newuidmap/newgidmap, machinectl" >&2
fi
for tool in podman nft newuidmap machinectl git; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing: $tool" >&2; exit 1; }
done
podman_version="$(podman --version | awk '{print $NF}')"
if (( ${podman_version%%.*} < 5 )); then
  echo "warning: Collie needs Podman 5 or later, this is $podman_version — isolated projects will not start" >&2
fi

echo "== user '$user'"
created=0
if ! id "$user" >/dev/null 2>&1; then
  useradd --create-home --shell /bin/bash "$user"
  created=1
fi
uid="$(id -u "$user")"
grep -q "^$user:" /etc/subuid || usermod --add-subuids 200000-265535 "$user"
grep -q "^$user:" /etc/subgid || usermod --add-subgids 200000-265535 "$user"
loginctl enable-linger "$user"

# Memory/CPU/process limits for rootless containers need those cgroup
# controllers delegated to the user's systemd instance. Recent systemd does
# that by default; older setups get a drop-in.
systemctl start "user@$uid.service" || true
controllers="/sys/fs/cgroup/user.slice/user-$uid.slice/user@$uid.service/cgroup.controllers"
if [[ ! -f /sys/fs/cgroup/cgroup.controllers ]]; then
  echo "warning: this host uses cgroup v1, where rootless containers get no resource limits" >&2
elif [[ -r $controllers ]]; then
  missing=""
  for c in cpu memory pids; do
    grep -qw "$c" "$controllers" || missing+=" $c"
  done
  if [[ -n $missing ]]; then
    echo "delegating cgroup controllers to user services (missing:$missing)"
    mkdir -p /etc/systemd/system/user@.service.d
    printf '[Service]\nDelegate=cpu cpuset io memory pids\n' > /etc/systemd/system/user@.service.d/60-collie-delegate.conf
    systemctl daemon-reload
    if [[ $created == 1 ]]; then
      systemctl restart "user@$uid.service"
    else
      echo "note: takes effect when $user's services restart (reboot, or: systemctl restart user@$uid.service — stops Collie)"
    fi
  fi
fi
if command -v tailscale >/dev/null 2>&1; then
  # lets the user run `tailscale serve` for Collie
  tailscale set --operator="$user" || true
fi

# Collie's own ports, which agents must never reach: its API and the
# previews of the agents' servers (the next ports; 8443 and up with --domain).
if ! [[ "$preview_ports" =~ ^[0-9]+$ ]] || (( preview_ports < 1 || preview_ports > 16 )); then
  echo "--preview-ports: give 1 to 16" >&2
  exit 2
fi
last=$((preview_ports - 1))
# A range of one port is written as the port: older nft and ufw refuse it.
span() { if (( $2 == 0 )); then echo "$1"; else echo "$1$3$(($1 + $2))"; fi; }
ports="$port, $(span $((port + 1)) $last -)"
if [[ $public == 1 ]]; then
  ports="$ports, 80, 443, $(span 8443 $last -)"
  echo "== public HTTPS"
  # Ports below 1024 are root-only by default; Collie runs as $user.
  echo "net.ipv4.ip_unprivileged_port_start = 80" > /etc/sysctl.d/60-collie.conf
  sysctl -q -p /etc/sysctl.d/60-collie.conf
  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
    ufw allow 80/tcp
    ufw allow 443/tcp
    ufw allow "$(span 8443 $last :)/tcp"
  fi
  if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
    firewall-cmd --permanent --add-service=http --add-service=https "--add-port=$(span 8443 $last -)/tcp"
    firewall-cmd --reload
  fi
fi

echo "== firewall (uid $uid)"
mkdir -p /etc/collie
allow_elems=""
if [[ ${#allow[@]} -gt 0 ]]; then
  allow_elems="elements = { $(IFS=,; echo "${allow[*]}") }"
fi
# The user's own uid, and its subordinate uids: a container on the host's
# network (`podman run --network host`, which a host-run agent can start)
# opens its sockets as one of those.
subids() {
  awk -F: -v u="$user" -v id="$uid" '($1 == u || $1 == id) && $2 ~ /^[0-9]+$/ && $3 ~ /^[0-9]+$/ && $3 > 0 {
    printf ", %d-%d", $2, $2 + $3 - 1 }' "$1" 2>/dev/null || true
}
skuids="$uid$(subids /etc/subuid)"
cat > /etc/collie/nftables.nft <<EOF
# Managed by collie deploy/setup-host.sh — egress policy for user $user.
table inet collie
delete table inet collie
table inet collie {
  set lan4 {
    type ipv4_addr; flags interval
    elements = { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, 169.254.0.0/16 }
  }
  set lan6 {
    type ipv6_addr; flags interval
    elements = { fc00::/7, fe80::/10 }
  }
  # the networks this server is attached to, also when their addresses are
  # public (an IPv6 LAN, say); kept up to date by collie-firewall-onlink
  set onlink4 {
    type ipv4_addr; flags interval; auto-merge
  }
  set onlink6 {
    type ipv6_addr; flags interval; auto-merge
  }
  set allow4 {
    type ipv4_addr; flags interval
    $allow_elems
  }
  chain output {
    type filter hook output priority filter; policy accept;
    meta skuid != { $skuids } accept
    ct state established,related accept
    # name resolution may go to a local or tailnet resolver (MagicDNS)
    meta l4proto { tcp, udp } th dport 53 accept
    # agents must never talk to Collie's own API
    ip daddr 127.0.0.0/8 tcp dport { $ports } reject
    ip6 daddr ::1 tcp dport { $ports } reject
    # other loopback services stay usable (e.g. dev servers of host-mode projects)
    ip daddr 127.0.0.0/8 accept
    ip6 daddr ::1 accept
    # none of this server's own addresses (LAN, tailnet or public IP), not
    # even within an --allow range …
    fib daddr type local reject
    ip daddr @allow4 accept
    # … nor anything else on the LAN / tailnet / link-local
    ip daddr @lan4 reject
    ip6 daddr @lan6 reject
    ip daddr @onlink4 reject
    ip6 daddr @onlink6 reject
  }
}
EOF
nft -c -f /etc/collie/nftables.nft

# Fills the onlink sets from the routes of the networks this server is
# directly attached to (no gateway), at start and every few minutes, as
# addresses come and go (DHCP, IPv6 router advertisements).
nft="$(command -v nft)"
ip="$(command -v ip || true)"
if [[ -z $ip ]]; then
  echo "warning: no 'ip' (iproute2): hosts on this server's own networks with public addresses stay reachable" >&2
fi
cat > /etc/collie/onlink.sh <<EOF
#!/bin/sh
# Managed by collie deploy/setup-host.sh: puts the networks this server is
# attached to into the collie table's onlink sets.
set -eu
# Routes wider than /8 (IPv4) or /16 (IPv6) are not a LAN: a VPN client may
# send the whole internet through gateway-less routes such as 0.0.0.0/1.
v4=\$($ip -o -4 route show table main scope link 2>/dev/null | awk '\$1 ~ /^[0-9.]+(\/[0-9]+)?\$/ { n = split(\$1, a, "/"); if (n == 1 || a[2] + 0 >= 8) print \$1 }' | sort -u | paste -sd, -)
v6=\$($ip -o -6 route show table main 2>/dev/null | awk '\$1 ~ /^[0-9a-f:]+(\/[0-9]+)?\$/ && \$0 !~ / via / && \$0 !~ / dev lo / { n = split(\$1, a, "/"); if (n == 1 || a[2] + 0 >= 16) print \$1 }' | sort -u | paste -sd, -)
{
  echo "flush set inet collie onlink4"
  echo "flush set inet collie onlink6"
  if [ -n "\$v4" ]; then echo "add element inet collie onlink4 { \$v4 }"; fi
  if [ -n "\$v6" ]; then echo "add element inet collie onlink6 { \$v6 }"; fi
} | $nft -f -
EOF
chmod 755 /etc/collie/onlink.sh
onlink_start=""
if [[ -n $ip ]]; then
  onlink_start="ExecStartPost=-/etc/collie/onlink.sh"
fi

cat > /etc/systemd/system/collie-firewall.service <<EOF
[Unit]
Description=Collie egress firewall for user $user (no LAN / tailnet access)
Wants=network-pre.target
Before=network-pre.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=$nft -f /etc/collie/nftables.nft
$onlink_start
ExecStop=$nft delete table inet collie

[Install]
WantedBy=multi-user.target
EOF
cat > /etc/systemd/system/collie-firewall-onlink.service <<EOF
[Unit]
Description=Collie egress firewall: this server's own networks
After=collie-firewall.service network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/etc/collie/onlink.sh
EOF
cat > /etc/systemd/system/collie-firewall-onlink.timer <<EOF
[Unit]
Description=Keep Collie's egress firewall up to date with this server's networks

[Timer]
OnBootSec=30s
OnUnitActiveSec=2min

[Install]
WantedBy=timers.target
EOF
systemctl daemon-reload
systemctl enable --now collie-firewall.service
systemctl restart collie-firewall.service
if [[ -n $ip ]]; then
  systemctl enable --now collie-firewall-onlink.timer
else
  systemctl disable --now collie-firewall-onlink.timer 2>/dev/null || true
fi

cat <<EOF

Done. Next, as the '$user' user:

  sudo machinectl shell $user@        # a proper login session (systemd --user works)
  curl https://mise.run | sh && exec bash -l   # then activate mise in ~/.bashrc
  git clone <collie repo> && cd collie
  mise trust && mise install
  mise run install --domain collie.example.com   # or --serve / --funnel / --proxy URL (see docs/setup.md)

Then open Collie, create your passkey (you become the first admin) and set
up your Claude / Codex accounts under Account → Agent accounts.
EOF
