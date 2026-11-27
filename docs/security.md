# Security

**English** | [日本語](security.ja.md) · [← README](../README.md)

Collie exists to run commands on your server from a browser. Keep it off the public internet and use it over Tailscale or a LAN.

## Signing in

- Only passkeys work once the first one exists. Before that, you can sign in with the token or through Tailscale, which is meant for first-time setup only.
- Use `--no-auth` only on a network you fully trust.

## Who can see what

- Every permission check runs on the server. Anything you aren't allowed to see is treated as if it doesn't exist.
- Admins can't read other people's personal projects or conversations either.

## What agents can reach

This assumes the configuration from the [setup guide](setup.md): a dedicated `collie` user plus its firewall.

| | Isolated container (default) | Host-run (admins only) |
|---|---|---|
| Files | That project and its worktrees only | Everything the `collie` user can access, including Collie's own data |
| Network | The internet. Not your LAN, your tailnet, this server or Collie's API | The same firewall rules as the `collie` user |
| Your own account (`~/.ssh` etc.) | Unreachable | Unreachable, because `collie` is a separate user |

- Group projects always run in containers, and only admins can drive a host-run agent.
- Members can't register arbitrary folders. Their clones are `https://` only and run inside their own container.
- Project folders can't be nested inside each other, so no container can see another project.
- Agents talk to Collie only through a tool socket given to their own container. A project's agent can address only its user's sessions in that same container.

## What is *not* protected

- An agent in a container can read every file in its project and can reach the internet. Don't keep secrets in a project that you wouldn't want sent out.
- A host-run agent has all the access of the `collie` user.
- In a project with **Allow containers inside**, the container keeps running as the `collie` user's own ids, with the same files and network. But its processes can switch between the container's own ids and reach more of the kernel (FUSE, tun devices, nested namespaces that mount their own `/proc` or set their own hostname), so a kernel bug is easier to reach from there. Turn it on only for projects whose agents you trust.

## Updates

- Collie installs only releases whose signature checks out against a key built into Collie. Swapping files on the release page, or passing an old release off as a new one, gets nothing installed.
- A signed release can be made only by someone who can run the release workflow by hand, because pushing a tag doesn't start it. Keep the **Actions**, **Workflows** and **Secrets** permissions off GitHub tokens that agents use, such as a `GH_TOKEN` in Collie's environment. With them an agent could publish a release, read the signing key or replace it.
- Only admins can install updates. Home agents can't.

## Other things worth knowing

- HTML and SVG that agents show you never run as Collie's own page. They open in a sandbox without network access, so they can't touch your Collie session or send anything out, even after you press **Run scripts**.
- Deleting a project in Collie leaves its files on disk. Only the worktrees Collie created are removed, and their branches are kept.
