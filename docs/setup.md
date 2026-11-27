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

With `--public`, Collie may listen on ports 80 and 443 even though it doesn't run as root. This lets every user on the machine listen on ports from 80 up, so use it on a server that is there for Collie. It also opens 80, 443 and 8443 (for [previews](#previews)) in ufw or firewalld if one is on. Open them in your cloud provider's firewall yourself.

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

This builds Collie and the agent container image, then registers a systemd user service. The image's base (Ubuntu packages, Podman, mise) is downloaded ready-made from GHCR when one is published for your checkout, and built on the server otherwise. The first time takes a few minutes either way.

Logs: `journalctl --user -u collie -f`

### Your own domain

Point the domain's DNS at the server and open ports 80 and 443 (and 8443 for [previews](#previews)) before you first open Collie. The certificate is fetched on the first visit, and Let's Encrypt refuses for a while after a few failed tries. Port 80 only redirects to HTTPS and answers Let's Encrypt.

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

## Previews

In a session, the **Ports** tab lists the servers its agent has started, such as `npm run dev`, in the project's container (or on the server, for a host-run project). Tapping one opens it in a new tab through Collie, hot reload included.

Previews are served on an origin of their own, so a page an agent wrote can't read or use your Collie sign-in. By default that origin is another port of Collie's address:

| | Previews open at | What to set up |
|---|---|---|
| **Your own domain** | `https://collie.example.com:8443` | Open port 8443 too. `setup-host.sh --public` opens it in ufw or firewalld; open it in your cloud provider's firewall yourself |
| **Your reverse proxy** | The address you give | Serve `http://127.0.0.1:7879`, WebSockets included, on a host name or port of its own, and add `--preview-url https://preview.example.com` to `mise run install --proxy …` |
| **Tailscale** | `https://<machine>.<tailnet>.ts.net:8443` | Nothing: `--serve` and `--funnel` set it up (Funnel allows port 8443) |
| Trying it out | `http://<server>:7879` | Nothing |

A port is the default because it works the same on your own domain, over Tailscale and on a LAN, with the certificate Collie already has and nothing to add to DNS. Tailscale's certificates cover only the machine's own name, so there a port is the only option.

A host name of its own keeps previews further from Collie, and is the better choice on your own domain if you can add a DNS record. Browsers keep cookies per host name, not per port. On a port, the browser also sends Collie's sign-in cookie to the preview (Collie drops it before anything reaches the agent's server), and a page there can overwrite Collie's cookies, which signs you out. With `--domain`, point another name at the server and add `--preview-url https://preview.collie.example.com`: Collie gets a certificate for it too and serves it on port 443. A name under another domain, such as `collie-preview.example.net`, also keeps previews from setting cookies for Collie's domain.

Behind Caddy, for example:

```
preview.example.com {
	reverse_proxy 127.0.0.1:7879
}
```

- A browser has one preview open at a time. Opening another replaces it.
- Servers listening on `localhost` or on every address are found. A project's container stops after 30 minutes without agents, terminals or previews in use, and its servers stop with it.
- The servers see requests for `localhost:<port>`, so the host checks of dev servers such as Vite and webpack pass as they are.
- Service workers don't run in previews.
- `--preview-addr off` turns previews off.

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

To update only Claude Code / Codex in the image, use Admin → Containers → **Collie standard** → **Details** → **Rebuild**, or run `collie --build-image`. This reinstalls only the agents; the base stays as it is and is updated with Collie. Agent images that admins added are rebuilt on top of it. Each project's container switches to the new image the next time it restarts while nothing in it is in use. That first start on a new image can take a few minutes (Podman copies the image's files for the container's user IDs); the session shows a notice meanwhile.

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
