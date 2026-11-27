#!/usr/bin/env bash
# Prints the nftables ruleset that lets one user's processes — Collie, its
# agents and their containers — reach the internet, but not the LAN, the
# tailnet, link-local addresses or this machine's own addresses:
#
#   egress-rules.sh <uid> <ports> [CIDR]...
#
# <ports> are Collie's own ports, refused on loopback as well (e.g.
# "7878, 80, 443"); "" where Collie doesn't run on that machine (Podman
# machine's VM). Each CIDR is let through (e.g. 192.168.1.20/32 for a NAS).
# Used by setup-host.sh and setup-mac.sh.
# (Keep this runnable by macOS's bash 3.2.)
set -euo pipefail

uid="$1"
ports="$2"
shift 2
allow_elems=""
if [[ $# -gt 0 ]]; then
  allow_elems="elements = { $(IFS=,; echo "$*") }"
fi
own_ports=""
if [[ -n "$ports" ]]; then
  own_ports="    # agents must never talk to Collie's own API
    ip daddr 127.0.0.0/8 tcp dport { $ports } reject
    ip6 daddr ::1 tcp dport { $ports } reject"
fi

cat <<EOF
# Managed by Collie (deploy/egress-rules.sh) — egress policy for uid $uid.
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
$own_ports
    # other loopback services stay usable (e.g. dev servers of host-mode projects)
    ip daddr 127.0.0.0/8 accept
    ip6 daddr ::1 accept
    ip daddr @allow4 accept
    # none of this machine's own addresses (LAN, tailnet or public IP) …
    fib daddr type local reject
    # … nor anything else on the LAN / tailnet / link-local
    ip daddr @lan4 reject
    ip6 daddr @lan6 reject
  }
}
EOF
