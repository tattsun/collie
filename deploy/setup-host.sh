#!/usr/bin/env bash
# One-time host preparation for Collie. Run on the server with sudo:
#
#   sudo ./deploy/setup-host.sh [--user collie] [--port 7878] [--allow CIDR]...
#
# It
#   1. installs podman (rootless containers) and nftables,
#   2. creates a dedicated user (default: collie) that will run Collie and all
#      agents, with lingering so its services keep running,
#   3. installs a firewall rule: every process of that user — including the
#      project containers — may reach the internet but NOT your LAN, your
#      tailnet, link-local addresses, any of this server's own addresses, or
#      Collie's own port.
#      (--allow adds exceptions, e.g. --allow 192.168.1.20/32 for a NAS.)
#
# Then switch to that user and install Collie itself (see README).
set -euo pipefail

user=collie
port=7878
allow=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --user) user="$2"; shift 2 ;;
    --port) port="$2"; shift 2 ;;
    --allow) allow+=("$2"); shift 2 ;;
    -h|--help) sed -n '2,17p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

[[ $EUID -eq 0 ]] || { echo "run with sudo" >&2; exit 1; }

. /etc/os-release
if [[ "${ID:-}" != ubuntu ]] || dpkg --compare-versions "${VERSION_ID:-0}" lt 26.04; then
  echo "warning: tested on Ubuntu 26.04+, this is ${PRETTY_NAME:-unknown}" >&2
fi

echo "== packages"
apt-get update -qq
apt-get install -y podman passt uidmap nftables systemd-container git curl ca-certificates

echo "== user '$user'"
if ! id "$user" >/dev/null 2>&1; then
  useradd --create-home --shell /bin/bash "$user"
fi
uid="$(id -u "$user")"
grep -q "^$user:" /etc/subuid || usermod --add-subuids 200000-265535 "$user"
grep -q "^$user:" /etc/subgid || usermod --add-subgids 200000-265535 "$user"
loginctl enable-linger "$user"
if command -v tailscale >/dev/null 2>&1; then
  # lets the user run `tailscale serve` for Collie
  tailscale set --operator="$user" || true
fi

echo "== firewall (uid $uid)"
mkdir -p /etc/collie
allow_elems=""
if [[ ${#allow[@]} -gt 0 ]]; then
  allow_elems="elements = { $(IFS=,; echo "${allow[*]}") }"
fi
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
  set allow4 {
    type ipv4_addr; flags interval
    $allow_elems
  }
  chain output {
    type filter hook output priority filter; policy accept;
    meta skuid != $uid accept
    ct state established,related accept
    # name resolution may go to a local or tailnet resolver (MagicDNS)
    meta l4proto { tcp, udp } th dport 53 accept
    # agents must never talk to Collie's own API
    ip daddr 127.0.0.0/8 tcp dport $port reject
    ip6 daddr ::1 tcp dport $port reject
    # other loopback services stay usable (e.g. dev servers of host-mode projects)
    ip daddr 127.0.0.0/8 accept
    ip6 daddr ::1 accept
    ip daddr @allow4 accept
    # none of this server's own addresses (LAN, tailnet or public IP) …
    fib daddr type local reject
    # … nor anything else on the LAN / tailnet / link-local
    ip daddr @lan4 reject
    ip6 daddr @lan6 reject
  }
}
EOF
nft -c -f /etc/collie/nftables.nft

cat > /etc/systemd/system/collie-firewall.service <<EOF
[Unit]
Description=Collie egress firewall for user $user (no LAN / tailnet access)
Wants=network-pre.target
Before=network-pre.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/collie/nftables.nft
ExecStop=/usr/sbin/nft delete table inet collie

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now collie-firewall.service
systemctl restart collie-firewall.service

cat <<EOF

Done. Next, as the '$user' user:

  sudo machinectl shell $user@        # a proper login session (systemd --user works)
  curl https://mise.run | sh && exec bash -l   # then activate mise in ~/.bashrc
  git clone <collie repo> && cd collie
  mise trust && mise install
  mise run install --serve            # build, install the service, publish via tailscale serve

Agent logins for '$user' (once):
  mise use -g npm:@openai/codex npm:@anthropic-ai/claude-code
  codex login --device-auth            # shared with all project containers
  claude setup-token                   # paste the token into Collie: Project → Settings → Shared environment
EOF
