#!/usr/bin/env bash
# Install Collie as a systemd *user* service (runs as you, so it uses your
# existing `claude` / `codex` logins and git credentials).
#
#   mise run install                    # plain HTTP on :7878 (trying it out on a LAN; no passkeys)
#   mise run install --domain NAME      # HTTPS on :443 for NAME with a Let's Encrypt certificate
#                                       #   (needs `sudo ./deploy/setup-host.sh --public` first)
#   mise run install --proxy URL        # on 127.0.0.1:7878 behind your own reverse proxy serving URL
#   mise run install --serve            # on 127.0.0.1, published on your tailnet with `tailscale serve`
#   mise run install --funnel           # like --serve, and on the internet with Tailscale Funnel
#
# (or ./deploy/install.sh with the same options)
# Env overrides: PREFIX (default ~/.local/bin), COLLIE_PORT (7878), COLLIE_ROOT, COLLIE_DATA
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
bin_src="$here/bin/collie"
prefix="${PREFIX:-$HOME/.local/bin}"
port="${COLLIE_PORT:-7878}"
serve=0
funnel=0
domain=""
proxy=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --serve) serve=1; shift ;;
    --funnel) serve=1; funnel=1; shift ;;
    --domain) domain="${2:?--domain needs a name, e.g. collie.example.com}"; shift 2 ;;
    --proxy) proxy="${2:?--proxy needs the URL users open, e.g. https://collie.example.com}"; shift 2 ;;
    -h|--help) sed -n '2,13p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
if (( serve + (${#domain} > 0) + (${#proxy} > 0) > 1 )); then
  echo "choose one of --serve / --funnel, --domain and --proxy" >&2
  exit 2
fi
if [[ -n "$proxy" && "$proxy" != https://* ]]; then
  echo "--proxy: the URL must start with https:// (passkeys need HTTPS)" >&2
  exit 2
fi
if [[ -n "$domain" ]]; then
  # The service runs unprivileged, so it can bind ports 80 and 443 only once
  # setup-host.sh --public has lowered the privileged port range.
  low="$(sysctl -n net.ipv4.ip_unprivileged_port_start 2>/dev/null || echo 1024)"
  if (( low > 443 )); then
    echo "Collie can't listen on port 443 as $USER. Run this once, then try again:" >&2
    echo "  sudo ./deploy/setup-host.sh --public" >&2
    exit 1
  fi
fi

if [[ ! -x "$bin_src" ]]; then
  echo "bin/collie not found — run 'mise run build' first (or copy a prebuilt binary to bin/collie)." >&2
  exit 1
fi

for tool in claude codex git; do
  command -v "$tool" >/dev/null 2>&1 || echo "note: '$tool' is not on PATH; sessions using it will not work."
done

install -Dm755 "$bin_src" "$prefix/collie"
echo "installed $prefix/collie"

if command -v podman >/dev/null 2>&1; then
  echo "building the agent container image (first time takes a few minutes)…"
  "$prefix/collie" --build-image ${COLLIE_DATA:+--data "$COLLIE_DATA"} || \
    echo "warning: image build failed; Collie retries when an isolated project first runs."
else
  echo "note: podman not found — projects will run agents directly on this host (see deploy/setup-host.sh)."
fi

addr=":$port"
[[ $serve == 1 || -n "$proxy" ]] && addr="127.0.0.1:$port"

# PATH for the service: if mise is in use, put its shims first so tools it
# manages (and per-project versions in each project's mise.toml) resolve the
# same way they do in your shell, even after upgrades.
unit_path="$PATH"
shims="${MISE_DATA_DIR:-$HOME/.local/share/mise}/shims"
if command -v mise >/dev/null 2>&1 && [[ -d "$shims" ]]; then
  unit_path="$shims:$PATH"
fi

extra=()
[[ -n "${COLLIE_ROOT:-}" ]] && extra+=("--root" "$COLLIE_ROOT")
[[ -n "${COLLIE_DATA:-}" ]] && extra+=("--data" "$COLLIE_DATA")
listen=("--addr" "$addr")
[[ -n "$domain" ]] && listen=("--domain" "$domain")      # :443 (and :80 for the redirect)
[[ -n "$proxy" ]] && listen+=("--public-url" "$proxy")

unit_dir="$HOME/.config/systemd/user"
mkdir -p "$unit_dir"
cat > "$unit_dir/collie.service" <<EOF
[Unit]
Description=Collie - web UI for Claude Code and Codex
After=network-online.target

[Service]
ExecStart=$prefix/collie ${listen[*]} ${extra[*]}
# The agents (claude, codex, git, language toolchains) are found through the
# PATH captured at install time. Re-run install.sh after changing it.
Environment="PATH=$unit_path"
Restart=on-failure
RestartSec=3
# Stopping the service also stops agent processes and terminal shells.
KillMode=control-group

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user enable --now collie.service
systemctl --user restart collie.service

# Keep running after you log out / across reboots.
if ! loginctl show-user "$USER" -p Linger 2>/dev/null | grep -q yes; then
  loginctl enable-linger "$USER" 2>/dev/null || sudo loginctl enable-linger "$USER" || \
    echo "warning: could not enable lingering; run 'sudo loginctl enable-linger $USER' so Collie survives logout."
fi

if [[ $serve == 1 ]]; then
  if command -v tailscale >/dev/null; then
    if [[ $funnel == 1 ]]; then
      # The first time, this prints a link to allow Funnel on your tailnet.
      tailscale funnel --bg "$port" || sudo tailscale funnel --bg "$port" || \
        echo "warning: 'tailscale funnel' failed. Try: sudo tailscale set --operator=$USER && tailscale funnel --bg $port"
    else
      # Not on the internet (any more, if --funnel was used before).
      tailscale funnel --bg "$port" off >/dev/null 2>&1 || true
      tailscale serve --bg "$port" 2>/dev/null || sudo tailscale serve --bg "$port" || \
        echo "warning: 'tailscale serve' failed. Try: sudo tailscale set --operator=$USER && tailscale serve --bg $port"
    fi
    tailscale serve status 2>/dev/null || true
  else
    echo "warning: tailscale CLI not found; install Tailscale and run 'tailscale serve --bg $port'."
  fi
fi

sleep 1
token="$("$prefix/collie" --print-token ${extra[*]})"
# (Shown here once on your terminal; the service log never contains it.)
echo
echo "Collie is running.  Logs: journalctl --user -u collie -f"
url=""
if [[ $serve == 1 ]]; then
  dns="$(tailscale status --json 2>/dev/null | sed -n 's/.*"DNSName": *"\([^"]*\)\.".*/\1/p' | head -1)"
  url="https://${dns:-<this-machine>.<tailnet>.ts.net}"
elif [[ -n "$domain" ]]; then
  url="https://$domain"
  echo "The certificate is fetched on the first visit: $domain must already point at this server,"
  echo "and ports 443 and 80 must be open to the internet."
elif [[ -n "$proxy" ]]; then
  url="${proxy%/}"
  echo "Point your reverse proxy at http://127.0.0.1:$port (see docs/setup.md)."
fi
if [[ -n "$url" ]]; then
  echo "First setup — open this, then create your passkey (the link stops working once one exists):"
  echo "  $url/?token=$token"
else
  ts_ip="$(tailscale ip -4 2>/dev/null | head -1 || true)"
  echo "Open:  http://${ts_ip:-<server-ip>}:$port/?token=$token"
  echo "(passkeys need HTTPS — re-run with --domain, --proxy or --serve to use them)"
fi
