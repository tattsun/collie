# Setup

**English** | [日本語](setup.ja.md) · [← README](../README.md)

This runs Collie as a long-running service on Ubuntu 26.04 or later. It runs under a dedicated user and is served over HTTPS on your tailnet.

## 1. Prepare the host (once, with sudo)

```bash
sudo ./deploy/setup-host.sh
```

This installs Podman and creates a `collie` user that runs Collie and every agent, so agents can't touch your own `~/.ssh` or personal files. It also adds a firewall rule: that user's processes can reach the internet, but not your LAN, your tailnet or this server. To let agents reach something like a NAS, pass `--allow 192.168.1.20/32`. See `--help` for all options.

## 2. Install Collie (as `collie`)

```bash
sudo machinectl shell collie@        # a login session where systemd --user works
curl https://mise.run | sh           # then enable mise in ~/.bashrc
git clone <this repo> && cd collie
mise trust && mise install
mise run install --serve
```

This builds Collie and the agent container image (the first build takes a few minutes), then registers a systemd user service. `--serve` publishes the service at `https://<machine>.<tailnet>.ts.net/` with `tailscale serve`. Use it: passkeys, notifications and installing the PWA all need HTTPS.

Logs: `journalctl --user -u collie -f`

## 3. Become the first admin

1. Open the URL the installer printed. Until a passkey exists, anyone on your tailnet can get in without a token.
2. Click **Create passkey**. You become the first admin, and from then on you can sign in only with a passkey.
3. Under Account → **Agent accounts**, connect your Claude Code / Codex. The screen walks you through it. You can add several Claude accounts (a subscription, Amazon Bedrock, an API key or custom settings) and choose one per project in Project → Settings → **Claude account**.
4. Invite other people from Admin → Users → **Invite**. Their agents run on their own accounts, so each person pays for their own usage.

## Update

```bash
cd collie && git pull
mise install
mise run install --serve    # with the same options you used the first time
```

The service restarts. Running agents stop and pick up again on the next message, and open browsers reload by themselves. To update only Claude Code / Codex in the image, run `collie --build-image`.

## Lost passkeys

- Another user: Admin → Users → **Passkey recovery link**
- You (an admin): run `collie link` on the server as `collie`. Add `--user NAME` to make a link for someone else.

## Host-run projects (optional)

Admins can set a project to run on the host instead of in a container (Project → Settings). Those agents use the `collie` user's own CLIs and logins, so install the CLIs and sign in as `collie`:

```bash
mise use -g node@lts npm:@openai/codex 'npm:@anthropic-ai/claude-code[allow_builds=true]'
claude                      # start it once to sign in
codex login --device-auth
```

Keep `allow_builds=true`. Without it, mise skips Claude Code's install script and `claude` fails with `native binary not installed`. If you already installed it without the option, add `allow_builds = true` to that line in `~/.config/mise/config.toml` and run `mise install -f npm:@anthropic-ai/claude-code`.

The service keeps the `PATH` it had at install time, with mise shims first. If you change `PATH`, run `mise run install` again.
