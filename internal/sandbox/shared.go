package sandbox

import (
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tattsun/collie/internal/model"
)

const sharedReadme = `# Collie shared agent profile

Everything here is mounted read-only into every isolated (container) project.
Edit it on the server; changes apply the next time an agent starts.

  claude/CLAUDE.md      instructions for Claude Code in all projects
  claude/skills/        shared skills   (one folder per skill, with SKILL.md)
  claude/agents/        shared sub-agents
  claude/commands/      shared slash commands
  claude/mcp.json       MCP servers for Claude Code (Playwright is on by default)
  claude/settings.json  extra Claude Code settings (permissions, hooks, env…)
  codex/AGENTS.md       instructions for Codex in all projects
  codex/skills/         shared Codex skills
  codex/config.toml     Codex config (MCP servers…); copied in at each start
  gitconfig             ~/.gitconfig inside the containers
  env                   KEY=VALUE lines exported in every container
                        (e.g. CLAUDE_CODE_OAUTH_TOKEN from "claude setup-token")

Project-specific settings stay in each repository (.claude/, .mcp.json,
AGENTS.md). Secrets for a single project (e.g. a GH_TOKEN scoped to one repo)
go in that project's settings in the Collie UI.

Your own ~/.claude and ~/.codex are deliberately NOT shared with containers:
they hold credentials and every project's history. Copy skills you want here.
`

const defaultMCP = `{
  "mcpServers": {
    "playwright": {
      "command": "playwright-mcp",
      "args": ["--browser", "chromium", "--headless", "--isolated", "--no-sandbox"]
    }
  }
}
`

const defaultCodexConfig = `# Codex configuration for every isolated project (copied to ~/.codex/config.toml
# in the container each time Codex starts).

[mcp_servers.playwright]
command = "playwright-mcp"
args = ["--browser", "chromium", "--headless", "--isolated", "--no-sandbox"]
`

const defaultEnv = `# KEY=VALUE lines exported in every isolated project's container.
# Claude Code login for containers: run "claude setup-token" and paste it here.
# CLAUDE_CODE_OAUTH_TOKEN=
`

// SeedShared creates the shared profile with defaults the first time.
// Existing files are never overwritten.
func SeedShared(dir string) error {
	files := map[string]string{
		"README.md":            sharedReadme,
		"claude/CLAUDE.md":     "",
		"claude/mcp.json":      defaultMCP,
		"claude/settings.json": "{}\n",
		"codex/AGENTS.md":      "",
		"codex/config.toml":    defaultCodexConfig,
		"env":                  defaultEnv,
		"gitconfig":            defaultGitconfig(),
	}
	for _, d := range []string{"claude/skills", "claude/agents", "claude/commands", "codex/skills"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return err
		}
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		mode := os.FileMode(0o644)
		if name == "env" {
			mode = 0o600
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			return err
		}
	}
	return nil
}

func defaultGitconfig() string {
	get := func(k string) string {
		out, err := exec.Command("git", "config", "--global", k).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	var b strings.Builder
	b.WriteString("# ~/.gitconfig inside isolated projects\n")
	if name, email := get("user.name"), get("user.email"); name != "" || email != "" {
		b.WriteString("[user]\n")
		if name != "" {
			b.WriteString("\tname = " + name + "\n")
		}
		if email != "" {
			b.WriteString("\temail = " + email + "\n")
		}
	}
	b.WriteString("[init]\n\tdefaultBranch = main\n")
	b.WriteString("# HTTPS pushes use GH_TOKEN (set it per project in Collie)\n")
	b.WriteString("[credential \"https://github.com\"]\n\thelper = !gh auth git-credential\n")
	return b.String()
}

// ClaudeArgs are extra command-line flags that load the shared profile's
// MCP servers and settings into Claude Code inside the container.
func (m *Manager) ClaudeArgs() []string {
	var args []string
	shared := m.SharedPath()
	if fileNonEmpty(filepath.Join(shared, "claude", "mcp.json")) {
		args = append(args, "--mcp-config", SharedDir+"/claude/mcp.json")
	}
	if fi, err := os.Stat(filepath.Join(shared, "claude", "settings.json")); err == nil && fi.Size() > 3 {
		args = append(args, "--settings", SharedDir+"/claude/settings.json")
	}
	return args
}

// The project home is mounted read-write into the container, so the agent
// controls its contents (it can plant symlinks pointing anywhere on the
// server). Every host-side write into it therefore goes through an os.Root,
// which refuses any path that would resolve outside the home.

func (m *Manager) homeRoot(projectID string) (*os.Root, error) {
	return os.OpenRoot(m.HomeDir(projectID))
}

// PrepareCodex copies the shared Codex config into the project's home so
// that the next app-server start picks it up.
func (m *Manager) PrepareCodex(projectID string) error {
	src := filepath.Join(m.SharedPath(), "codex", "config.toml")
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	root, err := m.homeRoot(projectID)
	if err != nil {
		return err
	}
	defer root.Close()
	return copyInto(root, src, ".codex/config.toml")
}

// StageUpload copies an uploaded image into the project's home and returns
// its path inside the container (for agents that take image paths).
func (m *Manager) StageUpload(projectID, hostPath string) (string, error) {
	name := filepath.Base(hostPath)
	root, err := m.homeRoot(projectID)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := copyInto(root, hostPath, ".collie-uploads/"+name); err != nil {
		return "", err
	}
	return Home + "/.collie-uploads/" + name, nil
}

// claudeKey is how Claude Code names a project directory under
// ~/.claude/projects (every non-alphanumeric character becomes '-').
func claudeKey(path string) string {
	var b strings.Builder
	for _, r := range path {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// migrateClaudeState copies the server user's Claude Code state for this
// project (memory and conversations, so existing sessions can resume) into
// the container home the first time the project runs isolated.
//
// Only entries for exactly the project directory and the working
// directories of its own sessions are copied. Prefix matching would not be
// safe: the key mapping is lossy ("/" and "-" both become "-"), so
// "/x/app" + "-" also prefixes the key of an unrelated "/x/app-secret".
func migrateClaudeState(p model.Project, root *os.Root, sessionDirs []string, hostHome string) error {
	if _, err := root.Lstat(".claude/projects"); err == nil {
		return nil // already initialised
	}
	if err := root.MkdirAll(".claude/projects", 0o700); err != nil {
		return err
	}
	if hostHome == "" {
		return nil
	}
	src := filepath.Join(hostHome, ".claude", "projects")
	want := map[string]bool{claudeKey(p.Path): true}
	for _, d := range sessionDirs {
		want[claudeKey(d)] = true
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() && want[e.Name()] {
			if err := copyTreeInto(root, filepath.Join(src, e.Name()), ".claude/projects/"+e.Name()); err != nil {
				log.Printf("migrate claude state %s: %v", e.Name(), err)
			}
		}
	}
	return nil
}

// copyInto copies a host file to name inside root (atomically, never
// following a symlink out of root).
func copyInto(root *os.Root, src, name string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := root.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	tmp := name + ".collie-tmp"
	_ = root.Remove(tmp)
	out, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}

// copyTreeInto copies a host directory tree (regular files only) into root.
func copyTreeInto(root *os.Root, src, name string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.ToSlash(filepath.Join(name, rel))
		switch {
		case d.IsDir():
			return root.MkdirAll(target, 0o700)
		case d.Type().IsRegular():
			return copyInto(root, p, target)
		}
		return nil
	})
}
