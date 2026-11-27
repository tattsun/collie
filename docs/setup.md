# Setup

**English** | [日本語](setup.ja.md) · [← README](../README.md)

This runs Collie as a long-running service on a Linux server, under a dedicated user and over HTTPS. Passkeys, notifications and installing the PWA all need HTTPS. To run it on a Mac instead, see [On a Mac](#on-a-mac).

The server needs systemd and Podman 5 or later. `setup-host.sh` installs the packages with apt, dnf, zypper or pacman, so it covers Debian 13+, Ubuntu 26.04+, Fedora, RHEL 9/10 and its rebuilds (AlmaLinux, Rocky), openSUSE and Arch. Elsewhere it lists what to install yourself.

First decide how people will reach it:

| | People open | You need |
|---|---|---|
| **Your own domain** | `https://collie.example.com` from anywhere | A domain name and a server with ports 80 and 443 open to the internet. Collie gets its certificate from Let's Encrypt by itself |
| **Your reverse proxy** | Whatever your Caddy, nginx or Cloudflare Tunnel serves | A proxy on the same machine that already serves HTTPS |
| **Tailscale** | `https://<machine>.<tailnet>.ts.net` on your tailnet, and with Funnel from anywhere | Tailscale on the server and on every device (without Funnel) |

Choose before you create the first passkey. Passkeys work only on the address they were created on (see [Moving to another address](#moving-to-another-address)).

## 1. Prepare the host (once, with sudo)

```bash
sudo ./deploy/setup-host.sh            # add --public for your own domain
```

This installs Podman and creates a `collie` user that runs Collie and every agent, so agents can't touch your own `~/.ssh` or personal files. It also adds a firewall rule: that user's processes can reach the internet, but not your LAN, your tailnet or this server. To let agents reach something like a NAS, pass `--allow 192.168.1.20/32`. See `--help` for all options.

With `--public`, Collie may listen on ports 80 and 443 even though it doesn't run as root. This lets every user on the machine listen on ports from 80 up, so use it on a server that is there for Collie. It also opens 80 and 443 in ufw or firewalld if one is on. Open them in your cloud provider's firewall yourself.

## 2. Install Collie (as `collie`)

```bash
sudo machinectl shell collie@        # a login session where systemd --user works
curl https://mise.run | sh           # then enable mise in ~/.bashrc
git clone <this repo> && cd collie
mise trust && mise install
mise run install --domain collie.example.com     # or one of the lines below
```

| | |
|---|---|
| `--domain collie.example.com` | Your own domain |
| `--proxy https://collie.example.com` | Behind your reverse proxy, at that address |
| `--serve` | Tailscale, tailnet only |
| `--funnel` | Tailscale, tailnet and the internet |

This builds Collie and the agent container image (the first build takes a few minutes), then registers a systemd user service.

Logs: `journalctl --user -u collie -f`

### Your own domain

Point the domain's DNS at the server and open ports 80 and 443 before you first open Collie. The certificate is fetched on the first visit, and Let's Encrypt refuses for a while after a few failed tries. Port 80 only redirects to HTTPS and answers Let's Encrypt.

### Your reverse proxy

Run it on the same machine and forward to `http://127.0.0.1:7878`, including WebSockets. Keep the client's address in `X-Forwarded-For`: Collie limits sign-in attempts per address, and without it every visitor shares one limit. With Caddy, this is all:

```
collie.example.com {
	reverse_proxy 127.0.0.1:7878
}
```

With nginx:

```nginx
location / {
    proxy_pass http://127.0.0.1:7878;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto https;
}
```

For Cloudflare Tunnel, point the public hostname at `http://localhost:7878`.

### Tailscale Funnel

The first time, `--funnel` prints a link to allow Funnel on your tailnet. To take Collie off the internet again, run `mise run install --serve` and check that `tailscale funnel status` no longer lists it.

## 3. Become the first admin

1. Open the link the installer printed. It contains the setup token. To see it again, run `collie --print-token` as `collie`. Until a passkey exists, the setup token is the only way in. Being on your tailnet or LAN doesn't sign anyone in.
2. Click **Create passkey**. You become the first admin. From then on you can sign in only with a passkey, and the setup token stops working.
3. Under Account → **Agent accounts**, connect your Claude Code / Codex. The screen walks you through it. You can add several Claude accounts (a subscription, Amazon Bedrock, an API key or custom settings) and choose one per project in Project → Settings → **Claude account**.
4. Invite other people from Admin → Users → **Invite**. Their agents run on their own accounts, so each person pays for their own usage.

## Update

Collie checks GitHub for new releases. When one is out, admins see `vX → vY` at the bottom of the sidebar, and Admin → **Updates** installs it with one tap. If you turn on **Install updates automatically** there, a new release installs itself once no agent has been working and no terminal has been used for 10 minutes.

- The service restarts. Running agents stop and pick up again on the next message, and open browsers reload by themselves.
- If the new version doesn't come up, Collie goes back to the previous version and its data, and the Updates screen says what happened. A version that failed is never retried automatically.
- Updating itself only works on Linux, for the systemd user service that `mise run install` sets up. A server running a build from before self-updating existed has to be updated by hand once, as below.

To update from the source instead:

```bash
cd collie && git pull
mise install
mise run install --domain collie.example.com    # with the same options you used the first time
```

To update only Claude Code / Codex in the image, use Admin → Containers → **Agent image** → **Rebuild**, or run `collie --build-image`. Each project's container switches to the new image the next time it restarts while nothing in it is in use. That first start on a new image can take a few minutes (Podman copies the image's files for the container's user IDs); the session shows a notice meanwhile.

## Lost passkeys

- Another user: Admin → Users → **Passkey recovery link**
- You (an admin): run `collie link` on the server as `collie`. Add `--user NAME` to make a link for someone else.

## Moving to another address

Passkeys work only on the address they were created on. To move Collie, for example from Tailscale to your own domain:

1. Reinstall with the new option (step 2).
2. As `collie`, run `collie reset-passkeys --yes`. This removes everyone's passkeys and signs everyone out.
3. Open the new address, sign in with the setup token (`collie --print-token`) and create your passkey right away. Until you do, the setup token is the only key.
4. Give everyone else a link to add a passkey: Admin → Users → **Passkey recovery link**.

Moving from `--serve` to `--funnel` keeps the address, so passkeys keep working.

## Host-run projects (optional)

Admins can set a project to run on the host instead of in a container (Project → Settings). Those agents use the `collie` user's own CLIs and logins, so install the CLIs and sign in as `collie`:

```bash
mise use -g node@lts npm:@openai/codex 'npm:@anthropic-ai/claude-code[allow_builds=true]'
claude                      # start it once to sign in
codex login --device-auth
```

Keep `allow_builds=true`. Without it, mise skips Claude Code's install script and `claude` fails with `native binary not installed`. If you already installed it without the option, add `allow_builds = true` to that line in `~/.config/mise/config.toml` and run `mise install -f npm:@anthropic-ai/claude-code`.

The service keeps the `PATH` it had at install time, with mise shims first. If you change `PATH`, run `mise run install` again.

## On a Mac

On a Mac, Collie runs as a LaunchAgent of your own user, and the agents run directly on the Mac with your own `claude` / `codex` logins. There are no isolated containers there yet, and only admins can run agents outside a container, so a Mac is for you alone: people you invite can sign in but can't start anything.

```bash
xcode-select --install               # git, unless you have it already
curl https://mise.run | sh           # then enable mise in ~/.zshrc
git clone <this repo> && cd collie
mise trust && mise install
mise run install --serve             # or --domain / --proxy / --funnel, as above
```

Install and sign in to the CLIs as in [Host-run projects](#host-run-projects-optional), then become the first admin as in step 3.

- Collie starts when you log in to the Mac and runs while you stay logged in. For a Mac that runs unattended, turn on automatic login, and keep it from sleeping (System Settings → Energy: prevent automatic sleeping).
- Keep projects outside Desktop, Documents and Downloads. macOS blocks background processes from those folders unless you give them Full Disk Access.
- With `--domain`, forward ports 80 and 443 from your router to the Mac. If the macOS firewall is on, allow incoming connections for `collie` when it asks.
- Logs: `tail -f ~/Library/Logs/collie.log`. Restart: `launchctl kickstart -k gui/$(id -u)/io.github.tattsun.collie`.
- Collie can't update itself on a Mac. Update from the source as above.
- Read what this means for security in [Security](security.md#what-is-not-protected).
