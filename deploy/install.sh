#!/usr/bin/env bash
# Install Collie as a user service — systemd on Linux, launchd on macOS. It
# runs as you, so it uses your existing `claude` / `codex` logins and git
# credentials.
#
#   mise run install                    # plain HTTP on :7878 (trying it out on a LAN; no passkeys)
#   mise run install --domain NAME      # HTTPS on :443 for NAME with a Let's Encrypt certificate
#                                       #   (on Linux, needs `sudo ./deploy/setup-host.sh --public` first)
#   mise run install --proxy URL        # on 127.0.0.1:7878 behind your own reverse proxy serving URL
#   mise run install --serve            # on 127.0.0.1, published on your tailnet with `tailscale serve`
#   mise run install --funnel           # like --serve, and on the internet with Tailscale Funnel
#   … --preview-url URL                 # where previews of the agents' servers open: :PORT, or https://*.preview.example.com
#   … --preview-ports N                 # how many previews a browser can have open at once (default 4)
#
# (or ./deploy/install.sh with the same options)
# Env overrides: PREFIX (default ~/.local/bin), COLLIE_PORT (7878), COLLIE_ROOT, COLLIE_DATA
#
# (Keep this runnable by macOS's bash 3.2: no empty "${array[@]}" under set -u.)
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
bin_src="$here/bin/collie"
prefix="${PREFIX:-$HOME/.local/bin}"
port="${COLLIE_PORT:-7878}"
os="$(uname -s)"
serve=0
funnel=0
domain=""
proxy=""
preview_url=""
preview_ports=4
while [[ $# -gt 0 ]]; do
  case "$1" in
    --serve) serve=1; shift ;;
    --funnel) serve=1; funnel=1; shift ;;
    --domain) domain="${2:?--domain needs a name, e.g. collie.example.com}"; shift 2 ;;
    --proxy) proxy="${2:?--proxy needs the URL users open, e.g. https://collie.example.com}"; shift 2 ;;
    --preview-url) preview_url="${2:?--preview-url needs :PORT or a URL, e.g. https://*.preview.example.com}"; shift 2 ;;
    --preview-ports) preview_ports="${2:?--preview-ports needs a number}"; shift 2 ;;
    -h|--help) sed -n '2,16p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
if (( serve + (${#domain} > 0) + (${#proxy} > 0) > 1 )); then
  echo "choose one of --serve / --funnel, --domain and --proxy" >&2
  exit 2
fi
if ! [[ "$preview_ports" =~ ^[0-9]+$ ]] || (( preview_ports < 1 || preview_ports > 16 )); then
  echo "--preview-ports: give 1 to 16" >&2
  exit 2
fi
if [[ -n "$proxy" && "$proxy" != https://* ]]; then
  echo "--proxy: the URL must start with https:// (passkeys need HTTPS)" >&2
  exit 2
fi

case "$os" in
  Linux)
    if ! command -v systemctl >/dev/null 2>&1; then
      echo "This installer sets Collie up as a systemd user service, and this system has no systemd." >&2
      echo "Run bin/collie with your own service manager instead (see collie --help)." >&2
      exit 1
    fi
    if ! systemctl --user show-environment >/dev/null 2>&1; then
      echo "No systemd user session for $USER. Log in as $USER with SSH, or with" >&2
      echo "  sudo machinectl shell $USER@" >&2
      echo "(not su / sudo -u), and run this again." >&2
      exit 1
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
    ;;
  Darwin) ;; # (any user may listen on 80 and 443 there)
  *)
    echo "This installer knows Linux (systemd) and macOS (launchd), not $os." >&2
    echo "Run bin/collie with your own service manager instead (see collie --help)." >&2
    exit 1
    ;;
esac

if [[ ! -x "$bin_src" ]]; then
  echo "bin/collie not found — run 'mise run build' first (or copy a prebuilt binary to bin/collie)." >&2
  exit 1
fi

for tool in claude codex git; do
  command -v "$tool" >/dev/null 2>&1 || echo "note: '$tool' is not on PATH; sessions using it will not work."
done

# Copied next to the old one and renamed over it: never overwrite a binary
# that may be running (macOS kills a process whose code changes under it).
mkdir -p "$prefix"
cp "$bin_src" "$prefix/.collie.new"
chmod 755 "$prefix/.collie.new"
mv -f "$prefix/.collie.new" "$prefix/collie"
echo "installed $prefix/collie"

if [[ "$os" != Linux ]]; then
  echo "note: isolated containers need Linux for now — agents run directly on this machine as $USER."
elif command -v podman >/dev/null 2>&1; then
  echo "building the agent container image (first time takes a few minutes)…"
  "$prefix/collie" --build-image ${COLLIE_DATA:+--data "$COLLIE_DATA"} || \
    echo "warning: image build failed; Collie retries when an isolated project first runs."
else
  echo "note: podman not found — projects will run agents directly on this host (see deploy/setup-host.sh)."
fi

addr=":$port"
[[ $serve == 1 || -n "$proxy" ]] && addr="127.0.0.1:$port"
# Previews of the agents' servers are served on the next ports, each an
# origin of its own (docs/setup.md). Over Tailscale they get ports 8443 and
# up of the machine's name; Funnel allows only 8443 and 10000 next to 443.
pport=$((port + 1))
ts_ports=()
ts_auto=0 # 1: this script maps the preview ports on Tailscale
if [[ $serve == 1 && -z "$preview_url" ]]; then
  ts_auto=1
  if [[ $funnel == 1 ]]; then
    ts_ports=(8443 10000)
  else
    for ((i = 0; i < preview_ports; i++)); do ts_ports+=($((8443 + i))); done
  fi
  preview_url=":$(IFS=,; echo "${ts_ports[*]}")"
fi

# PATH for the service: if mise is in use, put its shims first so tools it
# manages (and per-project versions in each project's mise.toml) resolve the
# same way they do in your shell, even after upgrades.
unit_path="$PATH"
shims="${MISE_DATA_DIR:-$HOME/.local/share/mise}/shims"
if command -v mise >/dev/null 2>&1 && [[ -d "$shims" ]]; then
  unit_path="$shims:$PATH"
fi

data=()
[[ -n "${COLLIE_ROOT:-}" ]] && data+=("--root" "$COLLIE_ROOT")
[[ -n "${COLLIE_DATA:-}" ]] && data+=("--data" "$COLLIE_DATA")
listen=("--addr" "$addr")
[[ -n "$domain" ]] && listen=("--domain" "$domain")      # :443 (and :80 for the redirect)
[[ -n "$proxy" ]] && listen+=("--public-url" "$proxy")
[[ -n "$preview_url" ]] && listen+=("--preview-url" "$preview_url")
listen+=("--preview-ports" "$preview_ports")
# (${data[@]+…}: bash 3.2 calls an empty array unbound)
cmd=("$prefix/collie" "${listen[@]}" ${data[@]+"${data[@]}"})
started=1

if [[ "$os" == Linux ]]; then
  unit_dir="$HOME/.config/systemd/user"
  mkdir -p "$unit_dir"
  cat > "$unit_dir/collie.service" <<EOF
[Unit]
Description=Collie - web UI for Claude Code and Codex
After=network-online.target

[Service]
ExecStart=${cmd[*]}
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
  logs="journalctl --user -u collie -f"
else
  # A LaunchAgent: launchd starts it when you log in to this Mac and keeps
  # it running until you log out.
  label=io.github.tattsun.collie
  agents="$HOME/Library/LaunchAgents"
  plist="$agents/$label.plist"
  log="$HOME/Library/Logs/collie.log"
  mkdir -p "$agents" "$HOME/Library/Logs"
  xml() { printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'; }
  {
    cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key>
  <array>
EOF
    for a in "${cmd[@]}"; do
      echo "    <string>$(xml "$a")</string>"
    done
    # launchd passes almost no environment: give the agents and terminals
    # the PATH (see above), a UTF-8 locale and your shell.
    cat <<EOF
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>$(xml "$unit_path")</string>
    <key>LANG</key><string>$(xml "${LANG:-en_US.UTF-8}")</string>
    <key>SHELL</key><string>$(xml "${SHELL:-/bin/zsh}")</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>StandardOutPath</key><string>$(xml "$log")</string>
  <key>StandardErrorPath</key><string>$(xml "$log")</string>
</dict>
</plist>
EOF
  } > "$plist"
  plutil -lint "$plist" >/dev/null

  gui="gui/$(id -u)"
  if ! launchctl print "$gui" >/dev/null 2>&1; then
    started=0
    echo "note: you are not logged in to this Mac's desktop, so Collie starts at your next login there."
    echo "      For a Mac that runs unattended, turn on automatic login (System Settings → Users & Groups)."
  else
    launchctl bootout "$gui/$label" 2>/dev/null || true
    # bootstrap can fail while the old instance is still shutting down
    for i in 1 2 3 4 5; do
      launchctl bootstrap "$gui" "$plist" 2>/dev/null && break
      [[ $i == 5 ]] && { echo "launchctl bootstrap $gui $plist failed" >&2; exit 1; }
      sleep 1
    done
  fi
  logs="tail -f ~/Library/Logs/collie.log"
fi

# The Mac app keeps its CLI inside the app bundle.
tailscale="$(command -v tailscale || true)"
if [[ -z "$tailscale" && -x /Applications/Tailscale.app/Contents/MacOS/Tailscale ]]; then
  tailscale=/Applications/Tailscale.app/Contents/MacOS/Tailscale
fi
if [[ $serve == 1 ]]; then
  if [[ -n "$tailscale" ]]; then
    if [[ $funnel == 1 ]]; then
      # The first time, this prints a link to allow Funnel on your tailnet.
      "$tailscale" funnel --bg "$port" || { [[ "$os" == Linux ]] && sudo "$tailscale" funnel --bg "$port"; } || \
        echo "warning: 'tailscale funnel' failed. Try: sudo tailscale set --operator=$USER && tailscale funnel --bg $port"
    else
      # Not on the internet (any more, if --funnel was used before).
      "$tailscale" funnel --bg "$port" off >/dev/null 2>&1 || true
      "$tailscale" serve --bg "$port" 2>/dev/null || { [[ "$os" == Linux ]] && sudo "$tailscale" serve --bg "$port"; } || \
        echo "warning: 'tailscale serve' failed. Try: sudo tailscale set --operator=$USER && tailscale serve --bg $port"
    fi
    if [[ $ts_auto == 1 ]]; then
      # Take down the preview mappings of an earlier install that aren't
      # wanted now (fewer ports, or serve instead of funnel): whatever came
      # to listen on their local ports would be served on this name.
      wanted=" ${ts_ports[*]} "
      for ((p = 8443; p < 8443 + 16; p++)); do
        case "$wanted" in *" $p "*) [[ $funnel == 1 ]] || "$tailscale" funnel --https="$p" off >/dev/null 2>&1 || true; continue ;; esac
        "$tailscale" funnel --https="$p" off >/dev/null 2>&1 || true
        "$tailscale" serve --https="$p" off >/dev/null 2>&1 || true
      done
      case "$wanted" in
        *" 10000 "*) ;;
        *) "$tailscale" funnel --https=10000 off >/dev/null 2>&1 || true
           "$tailscale" serve --https=10000 off >/dev/null 2>&1 || true ;;
      esac
    else
      echo "note: with your own --preview-url, map the preview ports on Tailscale yourself (see docs/setup.md)."
    fi
    i=0
    for p in ${ts_ports[@]+"${ts_ports[@]}"}; do
      target="http://127.0.0.1:$((pport + i))"
      if [[ $funnel == 1 ]]; then
        "$tailscale" funnel --bg --https="$p" "$target" || \
          echo "warning: 'tailscale funnel' for previews failed. Try: tailscale funnel --bg --https=$p $target"
      else
        "$tailscale" serve --bg --https="$p" "$target" || \
          echo "warning: 'tailscale serve' for previews failed. Try: tailscale serve --bg --https=$p $target"
      fi
      i=$((i + 1))
    done
    "$tailscale" serve status 2>/dev/null || true
  else
    echo "warning: tailscale CLI not found; install Tailscale and run 'tailscale serve --bg $port'."
  fi
fi

sleep 1
token="$("$prefix/collie" --print-token ${data[@]+"${data[@]}"})"
# (Shown here once on your terminal; the service log never contains it.)
echo
if [[ $started == 1 ]]; then
  echo "Collie is running.  Logs: $logs"
else
  echo "Collie is installed.  Logs: $logs"
fi
url=""
if [[ $serve == 1 ]]; then
  dns=""
  if [[ -n "$tailscale" ]]; then
    dns="$("$tailscale" status --json 2>/dev/null | sed -n 's/.*"DNSName": *"\([^"]*\)\.".*/\1/p' | head -1 || true)"
  fi
  url="https://${dns:-<this-machine>.<tailnet>.ts.net}"
elif [[ -n "$domain" ]]; then
  url="https://$domain"
  echo "The certificate is fetched on the first visit: $domain must already point at this server,"
  echo "and ports 443 and 80 must be open to the internet (and 8443-$((8443 + preview_ports - 1)), for previews of the agents' servers)."
elif [[ -n "$proxy" ]]; then
  url="${proxy%/}"
  echo "Point your reverse proxy at http://127.0.0.1:$port (see docs/setup.md)."
  if [[ -z "$preview_url" ]]; then
    echo "To open the agents' dev servers from a session (Ports), also serve http://127.0.0.1:$pport-$((pport + preview_ports - 1))"
    echo "on other ports of this name (or $pport on wildcard names), and run this again with --preview-url (see docs/setup.md)."
  fi
fi
if [[ -n "$url" ]]; then
  echo "First setup — open this, then create your passkey (the link stops working once one exists):"
  echo "  $url/?token=$token"
else
  ts_ip=""
  if [[ -n "$tailscale" ]]; then
    ts_ip="$("$tailscale" ip -4 2>/dev/null | head -1 || true)"
  fi
  echo "Open:  http://${ts_ip:-<server-ip>}:$port/?token=$token"
  echo "(passkeys need HTTPS — re-run with --domain, --proxy or --serve to use them)"
fi
