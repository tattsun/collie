# Security

**English** | [日本語](security.ja.md) · [← README](../README.md)

Collie exists to run commands on your server from a browser, so whoever signs in can do that. It is built to be reachable from the internet over HTTPS (see [Setup](setup.md)). This page says what that rests on.

## Signing in

- Once the first passkey exists, only passkeys sign anyone in. Passkeys can't be phished: a look-alike site can't use them.
- Before that, only the setup token does. Being on your tailnet or LAN, or anything a proxy adds to a request, never signs anyone in.
- Sign-in attempts, invite and recovery links and the setup token are rate limited per client address, and failed attempts get slower. Each failure is logged as `auth: failed …` with the address, for tools like fail2ban.
- Over HTTPS, pages on other ports of Collie's host name can't overwrite or shadow its sign-in cookie, and a request that carries the sign-ins of two sessions signs nobody in. So a page can sign you out, but never into someone else's account.
- `--no-auth` is for development on the server itself. Collie refuses to start with it on anything but a loopback address, and then answers only requests to `localhost` made on that machine, not ones that came through a proxy from elsewhere or from a web page that points its own name at `127.0.0.1`.

## Changes that need your passkey again

These need a passkey check from the last 15 minutes. Signing in counts. If yours is older, Collie asks for your passkey before making the change.

- adding or removing a passkey, or making a link to add another device
- inviting people, making recovery links, and changing or deleting users
- adding or removing group members and changing their roles (leaving a group yourself needs no check)
- the global skills, MCP, plugins and environment, the plugins of a group or a group project, and the agent images, which reach everyone's agents
- registering an existing server folder and running a project's agents on the host (on a server without Podman, that is every new project)

So someone using a stolen or left-open session can't lock you out, let themselves back in later, or reach beyond your own projects through these. They can still do everything else you can do until that session is signed out, including schedules that keep running your agents afterwards: check **Schedules** after signing out a device you don't trust.

When someone loses admin rights, is removed from a group or is disabled, Collie ends their terminals, agents and containers for what they lost right away, and their Home stops seeing those projects.

## Signed-in devices

Account → **Signed-in devices** lists each device with the address it was last used from. Sign out anything you don't recognize, and check your passkeys. A sign-in lasts 30 days from its last use.

Invite links work for 7 days and device or recovery links for 30 minutes, for anyone who has them, until they are used. Revoke the ones you no longer need in Admin → Users.

## Who can see what

- Every permission check runs on the server. Anything you aren't allowed to see is treated as if it doesn't exist.
- Admins can't read other people's personal projects or conversations either.
- A disabled user's sessions stay readable for their group, but nobody can drive them, so nothing runs on that user's accounts.
- Answers to Codex's secret questions reach the agent but are shown and kept as "(hidden)".
- Plugins turned on for a group or a project, or in the repository's `.claude/settings.json`, run in every member's container with that member's accounts and tokens, just like hooks in the repository's `.claude/`. Add only plugins from sources you trust.

## What agents can reach

This assumes the configuration from the [setup guide](setup.md): a dedicated `collie` user plus its firewall.

| | Isolated container (default) | Host-run (admins only) |
|---|---|---|
| Files | That project and its worktrees only | Everything the `collie` user can access, including Collie's own data |
| Network | The internet, or only the hosts on the project's allowlist. Not your LAN, your tailnet, this server or Collie's API | The same firewall rules as the `collie` user |
| Your own account (`~/.ssh` etc.) | Unreachable | Unreachable, because `collie` is a separate user |

- Group projects always run in containers, and only admins can drive a host-run agent.
- Members can't register arbitrary folders. Their clones are `https://` only and run inside their own container.
- Project folders can't be nested inside each other, so no container can see another project.
- Agents talk to Collie only through a tool socket given to their own container. A project's agent can address only its user's sessions in that same container.
- Collie never runs git on the server in an isolated project's files, because its agents can rewrite the repository's hooks and git settings. New worktrees, their removal and the **Changes** tab run git in the project's container. Except when making a worktree, that git gets none of your tokens and runs none of the repository's hooks, fsmonitor, filters or diff drivers (so **Changes** don't show uncommitted changes inside submodules, and may list Git LFS files whose timestamps changed as modified).
- Home runs in a container too and reads what other agents wrote, so it can't start, instruct or schedule agents of host-run projects, not even an admin's Home. Schedules it made don't run there even after their project moves to the host, and that move pauses all of the project's schedules.
- With SELinux on (Fedora, RHEL and the like), the containers run without SELinux labels, because they share the project's files with the host. Their other walls stay: their own user namespace, no capabilities, seccomp, and only their own mounts.
- A project on an allowlist (Project → Settings → **Network**) gives its containers no network of their own. Their only way out is Collie's proxy, which checks every connection against the list as it is at that moment. It never connects to the loopback or this server's addresses, even when they are listed. A private address, such as a NAS, gets through only when it is listed by IP and the firewall allows it (`--allow`).
- Only the project's owner can change its network setting (for a group project, the group's owners). Members and agents can't loosen it.

## Previews of the agents' servers

- Previews open on an origin of their own: another port of Collie's address, or a host name per project. Collie's sign-in never opens anything there, and Collie's cookies are taken out of every request before it reaches the agent's server. Collie's API answers only its own origin, not other ports of the same name.
- Only a session's owner can open its servers, even in a shared session. Each tap makes a link that works once, within a minute, for that session and port only. On a shared port, the preview then works only in the browser signed in to Collie with the sign-in that asked for it, so a preview cookie copied into another browser, or planted by a page, opens nothing. The preview closes when that sign-in ends or the session stops being theirs.
- A preview reaches the servers through Collie, not over the network, so it opens no way into or out of a container. For a host-run project it reaches only the `collie` user's own servers working in that session's folder, never Collie itself. Host-run agents all run as that user, though: they reach each other's servers themselves, and one could start a server in another session's folder to have it listed there.

## What is *not* protected

- An agent in a container can read every file in its project and can reach the internet. Don't keep secrets in a project that you wouldn't want sent out.
- An allowlist narrows where data can go, but allowed hosts can still carry it further. Claude's and Codex's own services are always allowed and accept uploads from any account. Hosts that serve many sites, such as CDNs or GitHub, can pass traffic on to them.
- An agent can read the accounts and tokens its container gets (the Claude and Codex logins, environment variables such as a `GH_TOKEN`), and so can whatever it runs. One project's agent can also replace the Codex login your other projects use. Give a project only the tokens it needs, e.g. a `GH_TOKEN` for just that repository in Project → Settings → **Project secrets**.
- A host-run agent has all the access of the `collie` user.
- Previews on ports of Collie's address reuse each port. A page that you keep open on one can read whatever you open on that port next. The browser also sends it the cookies of other apps on that host name, and it can overwrite or pile up cookies for Collie (signing you out, or keeping Collie from loading until you clear its site data). Names per project on another domain avoid most of this; there, pages of one project can still set cookies for the others' names (see [Setup](setup.md#previews)).
- A preview of a project on an allowlist may load and send only to its own origin and the listed hosts, and the reports a server asks browsers to send are dropped. But it runs in your browser: by navigating away, a page can still carry data out, and in browsers that don't support blocking WebRTC (Chromium-based ones, as of this writing) it can reach any host over WebRTC.
- On a Mac there are no containers and no firewall. Agents run as you, with everything your account can reach: your files, your LAN, and Collie's own data.
- In a project with **Allow containers inside**, the container keeps running as the `collie` user's own ids, with the same files and network. But its processes can switch between the container's own ids and reach more of the kernel (FUSE, tun devices, nested namespaces that mount their own `/proc` or set their own hostname), so a kernel bug is easier to reach from there. Turn it on only for projects whose agents you trust.
- An agent image an admin adds runs whatever its definition installs, in every project that uses it, and its build steps run as root in a build container that can reach the internet. Agents still run as the `collie` user, with the same files, network and limits. Only admins can add or change images.

## Updates

- Collie installs only releases whose signature checks out against a key built into Collie. Swapping files on the release page, or passing an old release off as a new one, gets nothing installed.
- A signed release can be made only by someone who can run the release workflow by hand, because pushing a tag doesn't start it. Keep the **Actions**, **Workflows** and **Secrets** permissions off GitHub tokens that agents use, such as a `GH_TOKEN` in Collie's environment. With them an agent could publish a release, read the signing key or replace it.
- Each release also pins the base of the agent image (Ubuntu packages, Podman, mise) by digest, so the image registry can't hand servers a different one. The agents on top (Claude Code, Codex, the Playwright MCP server) are installed from npm when the image is built on the server.
- Only admins can install updates. Home agents can't.

## Other things worth knowing

- HTML and SVG that agents show you never run as Collie's own page. They open in a sandbox without network access, so they can't touch your Collie session. Without scripts they can't send anything out. Scripts run only in Collie's viewer after you press **Run scripts**, never when you open the page in a new tab. Even then, browsers that don't support blocking WebRTC (Chromium-based ones, as of this writing) let a page's scripts send data out over WebRTC, so run them only in pages whose data may leave.
- Notifications carry only the project name, the session's title and its state, never what was said. They are encrypted for your browser but pass through its maker's push service (Apple, Google, Mozilla or Microsoft), which learns when they are sent and the address you open Collie at. Signing a device out stops its notifications.
- Deleting a project in Collie leaves its files on disk. Only the worktrees Collie created are removed, and their branches are kept.

## Reporting a vulnerability

Report it privately, not in a public issue: see [SECURITY.md](../SECURITY.md).
