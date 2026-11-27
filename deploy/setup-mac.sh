#!/usr/bin/env bash
# Prepares a Mac for isolated projects. Run it as yourself (not with sudo),
# after installing Podman (`brew install podman`, or the installer from
# podman.io):
#
#   ./deploy/setup-mac.sh [--allow CIDR]...
#
# It
#   1. creates Podman machine's Linux VM, where the project containers run,
#      unless there is one, and starts it;
#   2. installs a firewall in the VM: the containers may reach the internet
#      but NOT your LAN, your tailnet, link-local addresses or this Mac.
#      (--allow adds exceptions, e.g. --allow 192.168.1.20/32 for a NAS.)
#
# Run it again after re-creating the VM (`podman machine rm` / `init`).
# Then install Collie with `mise run install` (see docs/setup.md).
#
# (Keep this runnable by macOS's bash 3.2: no empty "${array[@]}" under set -u.)
set -euo pipefail

allow=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --allow) allow+=("${2:?--allow needs a CIDR, e.g. 192.168.1.20/32}"); shift 2 ;;
    -h|--help) sed -n '2,17p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

[[ "$(uname -s)" == Darwin ]] || { echo "this is for macOS; on Linux, use deploy/setup-host.sh" >&2; exit 1; }
[[ $EUID -ne 0 ]] || { echo "run this as yourself, not with sudo" >&2; exit 1; }
command -v podman >/dev/null 2>&1 || {
  echo "Install Podman first: brew install podman (or the installer from podman.io)" >&2
  exit 1
}

echo "== Podman machine"
if ! podman machine inspect >/dev/null 2>&1; then
  # Half the Mac for the VM; the containers' limits apply inside it.
  cpus=$(( $(sysctl -n hw.ncpu) / 2 ))
  (( cpus >= 1 )) || cpus=1
  mem=$(( $(sysctl -n hw.memsize) / 1024 / 1024 / 2 ))
  (( mem <= 8192 )) || mem=8192
  echo "creating the VM: $cpus CPUs, $mem MB of memory, 100 GB of disk"
  podman machine init --cpus "$cpus" --memory "$mem" --disk-size 100
fi
if [[ "$(podman machine inspect --format '{{.State}}')" != running ]]; then
  podman machine start
fi
if [[ "$(podman machine inspect --format '{{.Rootful}}')" == true ]]; then
  echo "The VM runs containers as root, where this firewall can't tell them apart. Switch it back:" >&2
  echo "  podman machine stop && podman machine set --rootful=false && podman machine start" >&2
  exit 1
fi
vm() { podman machine ssh "$@"; }
uid="$(vm id -u | tr -d '\r')"
[[ "$uid" =~ ^[0-9]+$ ]] || { echo "could not tell the VM's user id ($uid)" >&2; exit 1; }

echo "== firewall in the VM (uid $uid)"
# Collie itself runs on the Mac, so there are no ports of its own to refuse
# in the VM; the Mac is reached there through the gateway, which is on the
# VM's (blocked) private network.
"$(dirname "$0")/egress-rules.sh" "$uid" "" ${allow[@]+"${allow[@]}"} |
  vm "sudo mkdir -p /etc/collie && sudo tee /etc/collie/nftables.nft >/dev/null"
vm "sudo tee /etc/systemd/system/collie-firewall.service >/dev/null" <<'EOF'
[Unit]
Description=Collie egress firewall for the containers (no LAN / tailnet / Mac access)
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
vm "sudo nft -c -f /etc/collie/nftables.nft && sudo systemctl daemon-reload &&
  sudo systemctl enable --now collie-firewall.service && sudo systemctl restart collie-firewall.service"
vm "sudo nft list table inet collie" >/dev/null

cat <<EOF

Done. Collie starts the VM when it starts, if it isn't running. Next:

  mise trust && mise install
  mise run install --serve            # or --domain / --funnel / --proxy URL (see docs/setup.md)

Keep your projects in your home folder: it is all the VM can see.
EOF
