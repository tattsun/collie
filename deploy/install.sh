#!/usr/bin/env bash
# Install Collie as a systemd *user* service (runs as you, so it uses your
# existing `claude` / `codex` logins and git credentials).
#
#   mise run install             (or ./deploy/install.sh)      # listen on :7878 (all interfaces, token auth)
#   mise run install --serve     (or ./deploy/install.sh --serve) # listen on 127.0.0.1 and publish via `tailscale serve` (HTTPS)
#
# Env overrides: PREFIX (default ~/.local/bin), COLLIE_PORT (7878), COLLIE_ROOT, COLLIE_DATA
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
bin_src="$here/bin/collie"
prefix="${PREFIX:-$HOME/.local/bin}"
port="${COLLIE_PORT:-7878}"
serve=0
for a in "$@"; do
  case "$a" in
    --serve) serve=1 ;;
    -h|--help) sed -n '2,10p' "$0"; exit 0 ;;
    *) echo "unknown option: $a" >&2; exit 2 ;;
  esac
done

if [[ ! -x "$bin_src" ]]; then
  echo "bin/collie not found — run 'mise run build' first (or copy a prebuilt binary to bin/collie)." >&2
  exit 1
fi

for tool in claude codex git; do
  command -v "$tool" >/dev/null 2>&1 || echo "note: '$tool' is not on PATH; sessions using it will not work."
done

install -Dm755 "$bin_src" "$prefix/collie"
echo "installed $prefix/collie"

addr=":$port"
[[ $serve == 1 ]] && addr="127.0.0.1:$port"

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

unit_dir="$HOME/.config/systemd/user"
mkdir -p "$unit_dir"
cat > "$unit_dir/collie.service" <<EOF
[Unit]
Description=Collie - web UI for Claude Code and Codex
After=network-online.target

[Service]
ExecStart=$prefix/collie --addr $addr ${extra[*]}
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
    tailscale serve --bg "$port" 2>/dev/null || sudo tailscale serve --bg "$port" || \
      echo "warning: 'tailscale serve' failed. Try: sudo tailscale set --operator=$USER && tailscale serve --bg $port"
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
if [[ $serve == 1 ]]; then
  dns="$(tailscale status --json 2>/dev/null | sed -n 's/.*"DNSName": *"\([^"]*\)\.".*/\1/p' | head -1)"
  echo "Open on your tailnet:  https://${dns:-<this-machine>.<tailnet>.ts.net}/"
  echo "(tailnet users are signed in automatically; token for other access: $token)"
else
  ts_ip="$(tailscale ip -4 2>/dev/null | head -1 || true)"
  echo "Open:  http://${ts_ip:-<server-ip>}:$port/?token=$token"
fi
