# How it works

**English** | [日本語](how-it-works.ja.md) · [← README](../README.md)

## Projects and sessions

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="images/mobile-dark.png">
  <img src="images/mobile-light.png" alt="On a phone: the session list with Needs you, a permission request in the chat, and the Changes tab">
</picture>

A **project** is a folder on the server. You can create a new one, clone a repository, or, as an admin, register an existing folder. A **session** is one conversation with one agent (Claude Code or Codex) in a project.

- With **Isolated worktree**, a session gets its own `collie/<id>` branch and checkout, so several sessions can work on the same repository at once. When it is created, Collie fetches the branch's upstream (or the remote's default branch if it has none) and starts from its tip if your local branch is behind. Your local branch is never moved, and unpushed local commits are kept. In an isolated project this happens in the project's container, so the fetch uses that project's network setting and its agents' credentials. A repository with no commits yet (a new project, a clone of an empty repository) gets an empty "Initial commit" to branch off when its first worktree session starts.
- A clone runs in the background (see [Security](security.md#what-agents-can-reach) for where). If it fails, the project stays, showing why, and no session starts in it until **Clone again** on the project page succeeds. The URL can be corrected there. Clone again needs the project's folder empty: a failed clone leaves it so, but one cut off by a restart of Collie can leave files behind. Remove them in the project's **Terminal**, or remove the project.
- An agent process stops after 30 idle minutes. Your next message resumes the conversation, and that works after a server restart too.
- Messages you send while the agent is running reach it as extra instructions. A message typed while an approval is waiting denies the request and passes your text along with the denial.
- **Archive** (swipe a session's row left on a phone, or use the button on the row with a mouse or keyboard) only takes the session out of the list. Its agent is not stopped: a working one carries on, and you are still notified when it finishes or needs you. Sending it a message brings it back to the list.
- Deleting a session also deletes, from the server, the images and files in it: what was attached, the agent's screenshots and what it showed you, and the copies Collie gave the agent. Copies the agent made itself, for example into the project, stay.

## Schedules

**Schedules** (in the sidebar) start a session with the same instructions at set times: every day, on weekdays, on chosen days of the week, monthly, hourly, or any cron expression, in the time zone you pick. "Run now" starts one right away.

- Each run is a new session, marked with a clock in the session list. It runs with your agent accounts and your instructions for all sessions, and only you see or change the schedule.
- A model or effort left at "Your default" uses your defaults for new sessions as they are at each run.
- Runs never overlap. A planned time is skipped (and noted in the history) while the previous run is still working, waiting for you, or has background work left.
- If Collie is not running at a planned time (stopped, restarting for an update), it runs once when it starts again, however many times were missed.
- Runs start while you may be away. With a permission mode that asks first, a run stops at the first approval and waits in **Needs you**, and the next planned times are skipped until it ends.
- A schedule runs at most every 5 minutes. Times that a daylight-saving change skips don't run that day; times it repeats run once.
- Moving a project to the host (Project → Settings) pauses its schedules. Turn them back on in **Schedules** once you've checked them. Schedules that Home made never run there.

## Screenshots, videos and HTML

Agents in every session can show you files right in the chat. Try asking "make three design options and show me screenshots", "record the sign-in flow and show me" or "write it up as an HTML page and show me".

- Playwright records video as WebM. If a video won't play on your iPhone, ask for MP4.
- HTML pages open in a sandbox without network access, so they have to be a single file with styles and images embedded. The agent is told this. Scripts run only after you press **Run scripts**, and only in Collie's viewer: **Open in a new tab** shows the page without them.

## Attaching files

Besides images, you can attach any file to a message, such as a Word brief, a spreadsheet or a PDF: with the paperclip, by pasting, or by dropping it on the message box. Each file can be up to 25 MB.

- The agent gets a copy to read, outside the project, and the message tells it where. If the file belongs in the project, ask the agent to put it there.
- Everyone who can see the session can download its attachments.
- Images and files you attach but don't send are deleted after a day.

## Notifications

Turn them on in Account → **Notifications** on each device you want them on, then press **Send a test notification**. You hear when a session you started needs your approval or an answer, finishes, or stops with an error, also while Collie is closed.

- On iPhone and iPad (iOS 16.4 or later), add Collie to the Home Screen first (Share → Add to Home Screen), open it from there, and turn notifications on in that app.
- If you added Collie to the Home Screen with version 0.4.0 or earlier and it sits too high, with an empty strip along the bottom of the screen or blurred text along the top, delete the icon and add it again. iOS keeps the status bar setting from the day the icon was added. Then turn notifications on again in the new icon, and sign in if asked.
- A device where Collie is open in front of you gets the notification from the page itself, not twice.
- The server needs to reach the browsers' push services over HTTPS: `fcm.googleapis.com` (Chrome, Android), `push.services.mozilla.com` (Firefox), `web.push.apple.com` (Safari, iPhone) and `notify.windows.com` (Edge).
- Behind Tailscale, notifications arrive even when the phone is off the tailnet, but opening one needs Tailscale connected.

## Dev servers

Ask the agent to start the app (`npm run dev` and the like), then open the session's **Ports** tab. It lists what is listening in the project's container, this session's servers first, and a tap opens it in a new tab, with hot reload. Several can be open at once. Only you can open your sessions' servers, even in a shared session. Where previews are served is set up on the server (see [Setup](setup.md#previews)).

## Terminal

On Windows and Linux, Ctrl+V and Ctrl+Shift+V paste, and Ctrl+C copies the selected text; with nothing selected, Ctrl+C interrupts as usual. On a Mac, Cmd+V and Cmd+C do this, and Ctrl keeps going to the shell. Since Ctrl+V no longer reaches the shell on Windows and Linux, press Insert for bash's quoted-insert, and Ctrl+Q for a block selection in Vim.

## Isolation

New projects run in an **Isolated container** by default. You can change this in Project → Settings.

- Each project and user pair gets its own rootless Podman container. That user's agents and terminals for the project all run inside it.
- The container sees only that project's folder and its worktrees. Collie's data, other projects and your host `~/.claude` are not visible. Codex's settings, threads and memories are kept per project too; only the Codex login is shared by all of your projects. Sign it out in Account → **Agent accounts** (`codex logout` in a terminal doesn't).
- Toolchains the project installs with mise (`mise install`, from its `mise.toml` or `.tool-versions`) work by name in its containers, and its mise config is trusted there without `mise trust`. Where the image has its own (Node.js, Python), the image's comes first; run the project's with `mise exec -- node` and the like.
- Memory, CPU and process limits apply. The defaults are 4 GB, half the cores and 4096 processes, and admins can change them per project. There is no swap: when the processes in a container need more memory than its limit, the kernel kills one, often the agent itself. The session then says so, with the limit, and admins get a link to raise it. Builds, test runs and browsers running side by side are the usual cause.
- Changes to a project's limits, its image (or an update of the image) and **Allow containers inside** reach a container when it restarts. While it is in use (an agent at work, waiting for you or with work in the background, or a terminal open), it keeps running as it is, and Project → Settings and the project's sessions say so. Once nothing is at work in it, Collie restarts it by itself. Agents that are only idle stop with it and resume on your next message, and servers your agents left running in it stop too. **Restart now** does it at once: agents at work stop, and terminals close.
- If an agent runs away, **Kill now** stops the whole container. The conversation resumes on the next message.
- When Collie stops or restarts a container while an agent is at work in it (**Restart now**, **Stop everything**, **Kill now**, a change of the network setting), or restarts itself (an update, say), the session says why and shows under **Needs you**. You are notified as for an error, and so is a Home that watches it.
- A project's owner can choose where its containers may connect in Project → Settings → **Network**. With **Allowlist only**, they reach only the listed hosts and Claude's and Codex's own services. Tools that use `HTTPS_PROXY` work as usual, including git over `https://`, but SSH and other direct connections can't get out, so use `https://` remotes with a `GH_TOKEN`. Hosts that were blocked show up under **Recent connections**, where one tap allows them. Edits to the list apply at once, also to connections already open: removing a host ends them within a few seconds. Switching between Internet and an allowlist restarts the project's containers, terminals included. Containers started inside such a project (with **Allow containers inside**) have no internet of their own.
- Since the agent is boxed in, letting it run without approvals (Claude's **Bypass**, Codex's **Full access**) is easier to justify. You can switch the mode at any time from the chip in the message box.
- An admin can turn on **Allow containers inside (Podman)** for a project, for example to develop Collie itself there. Agents can then use `podman` in that project's containers. Each user's images are kept per project and count against the server's disk. Changes apply when the containers restart, and the first start after updating Collie rebuilds the agent image.

## Agent images

Containers run Collie's standard image: Ubuntu with Claude Code, Codex, Playwright, mise and common tools. When your teams need more, such as system packages, your company's CA certificate or internal tools, an admin adds an image in Admin → Containers. An image is a few Containerfile lines that Collie builds on top of the standard one.

- Whoever manages a project (its owner, or the group's owner) picks its image in Project → Settings. Projects that don't pick one, and everyone's Home, run the default image, which admins choose.
- Whatever an image sets as `USER` or `ENTRYPOINT`, agents run as the `collie` user, with the same files, network and limits. A build without Claude Code, Codex or Collie's helpers is rejected.
- Collie rebuilds an image when its definition changes and when an update brings a new standard image. Until the new build succeeds, projects keep running the previous one, so a failed build doesn't stop anyone. Only an image's very first build makes agents wait. **Rebuild** builds it again without the cache, to pick up newer packages.
- Builds sit behind the same firewall as agents, so they can't reach your LAN or tailnet. To use a registry or package mirror on your network, allow it with `setup-host.sh --allow` (one on this server itself can't be allowed). Agents can then reach it too. For a private registry, run `podman login` as the `collie` user. Agents can't see that login.
- There is no folder to copy files from. Write them in with a heredoc (`COPY <<'EOF' /path`), or take them from another image with `COPY --from=`.
- Each image adds its own layers to the server's disk.

## Users and groups

- Personal projects and conversations are visible only to their owner. Admins can't see them either.
- In a **group project**, all members share the files and Changes. Sessions are shared with the group by default (you can choose **Make private**), so members can watch, instruct, approve and interrupt. **Make private** hides the conversation, not its files: every member's container sees all sessions' worktrees.
- An agent always runs on the account of the person who **started** the session (the Claude account they chose for that project), not the person sending the message.
- A terminal is visible only to the person who opened it.
- Admins invite and manage users, edit the global skills, MCP and environment variables, and manage host-run projects, existing folders and resource limits.

## Skills, MCP and instructions

There are two layers, and they are combined when an agent starts:

- **Global**, edited by admins in Admin → Global skills & MCP
- **Personal**, edited by each user in Account → My skills & MCP

Personal settings win when names clash. The repository's own `.claude/`, `.mcp.json` and `AGENTS.md` apply on top. Environment variables override in the order global < personal < project. Playwright MCP is enabled from the start.

Claude's auto memory is shared across your sessions in a project. In a group project, it is shared by all members.

**Your defaults for new sessions** are in Account → Agent accounts → New sessions: a model and reasoning effort for each agent, and instructions for all of your sessions (e.g. "Write PR titles in English").

- New session starts with your model and effort, and your Home agent uses them unless it picks its own. The effort can't be changed once a session runs.
- The instructions are added to the agent's system prompt (Claude Code's `--append-system-prompt`, Codex's developer instructions), in host-run projects and Home too. Your messages and the repository's files are left as they are. For Codex, they take the place of `developer_instructions` in your own Codex config.
- Only you see them, and they go only to the sessions you started. A shared session in a group project gets the instructions of the member who started it.
- Changes apply to sessions started afterwards. A session already under way may keep the earlier instructions.

## Claude Code plugins

Plugins can be turned on at five levels. A later level wins, and **Turn off** keeps a plugin turned on at a broader level out of that scope:

- **Global**: admins, in Admin → Global plugins
- **Group**: the group's owners, on the group's page. Applies to the group's projects
- **Personal**: each user, in Account → My plugins
- **Project**: the project's managers, in Project → Settings
- **Repository**: `enabledPlugins` in the repository's `.claude/settings.json`

Project → Settings shows which plugins your agents get there and which level decided each one.

- Collie installs them into your container right before Claude starts, and when you open a terminal. Progress and failures appear in the session. Changes reach agents started afterwards.
- Plugins are installed per project and user, so the first session after a change in each project takes a little longer.
- Collie removes only the plugins it installed. Plugins you install yourself in a terminal stay, even if a level turns them off.
- Only isolated projects get plugins. Host-run projects use the server user's own `~/.claude`, which Collie leaves alone.
- In a project on an allowlist, the plugins' sources must be on the list too. Most come from GitHub (`github.com`, `*.githubusercontent.com`).

## Home

**Home**, at the top of the sidebar, is a conversation that belongs to no project. You can ask it anything, including work that spans projects:

- "What did I do this week?" or "How does auth work in sample2?" It reads your projects, session history and code to answer.
- "Start a session in sample2 to fix the tests." It can start sessions and send instructions to existing ones.
- "Every Friday at 18:00, sum up what was merged this week." It can create, list and delete your schedules.
- "Tell me when it's done, and check the diff first." Home follows the sessions it starts. When one finishes its turn, waits for you, or fails, Collie wakes Home with a short notification. Home then reads the session and its changes and takes the work forward. It can also stop and archive your sessions.

Home can't edit projects itself. The sessions it starts do the editing, and their approvals still come to **Needs you**. It sees only what you can see. When Home runs in a container (on Linux with Podman), it doesn't start, instruct or schedule agents in host-run projects. Do that yourself in Collie.

Home can also list, read and archive your other Home conversations, so "tidy up my sessions" covers them too. It never archives the conversation you're talking to it in. It doesn't send messages to Home conversations, stop them or watch them, so two of them can't keep setting each other off. Other people's Home conversations stay out of its reach, even when you or they are an admin.

When Home follows sessions:

- Each notification starts a Home turn, which uses your plan like a message would. To stop the notifications about a session, use **Stop watching** on a notification, **Stop Home from watching** in the session's menu, or ask Home.
- A notification waits until Home's current turn is over, and changes that come close together arrive as one. After 30 notifications about the same session, Collie stops watching it.
- When a session finishes its turn with work still running in the background (CI, a build, a sub-agent), Collie doesn't tell Home yet. The session picks up again when that work ends, and Home hears about the turn after that. If the work is still running 10 minutes later (a dev server, say), Home is told once. Approvals, questions and errors are told right away.
- Home stops and archives only sessions you started (not other members', even in a group you own). It never removes a worktree or branch.
