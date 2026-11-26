package sandbox

import (
	"io"
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

// PrepareCodex copies the shared Codex config into the project's home so
// that the next app-server start picks it up.
func (m *Manager) PrepareCodex(projectID string) error {
	src := filepath.Join(m.SharedPath(), "codex", "config.toml")
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	dst := filepath.Join(m.HomeDir(projectID), ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return copyFile(src, dst, 0o600)
}

// StageUpload copies an uploaded image into the project's home and returns
// its path inside the container (for agents that take image paths).
func (m *Manager) StageUpload(projectID, hostPath string) (string, error) {
	name := filepath.Base(hostPath)
	dst := filepath.Join(m.HomeDir(projectID), ".collie-uploads", name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	if _, err := os.Stat(dst); err != nil {
		if err := copyFile(hostPath, dst, 0o600); err != nil {
			return "", err
		}
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
func migrateClaudeState(p model.Project, home, worktreeRoot string) error {
	dst := filepath.Join(home, ".claude", "projects")
	if _, err := os.Stat(dst); err == nil {
		return nil // already initialised
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	hostHome, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	src := filepath.Join(hostHome, ".claude", "projects")
	entries, err := os.ReadDir(src)
	if err != nil {
		return nil
	}
	keys := []string{claudeKey(p.Path), claudeKey(worktreeRoot)}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, k := range keys {
			if e.Name() == k || strings.HasPrefix(e.Name(), k+"-") {
				_ = copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
				break
			}
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
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
	return os.Rename(tmp, dst)
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case fi.IsDir():
			return os.MkdirAll(target, 0o700)
		case fi.Mode().IsRegular():
			return copyFile(p, target, 0o600)
		}
		return nil
	})
}
