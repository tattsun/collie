package sandbox

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/tattsun/collie/internal/gitutil"
	"github.com/tattsun/collie/internal/model"
)

// Profiles. The global profile (<data>/shared, managed by admins) and each
// user's personal profile (<data>/users/<id>/profile) have the same layout.
// Before a user's agents start, the two are merged into the user's
// effective profile (<data>/users/<id>/effective), which is mounted
// read-only into the user's containers:
//
//	claude/CLAUDE.md                 global, then personal
//	claude/skills|agents|commands/   union (personal wins on a name clash)
//	claude/mcp.json                  mcpServers merged (personal wins)
//	claude/settings.json             deep-merged (personal wins)
//	codex/AGENTS.md                  global, then personal
//	codex/skills/                    union
//	codex/config.toml                merged by table (personal wins)
//	gitconfig                        global, then personal (later values win)

const ProfileDir = "/opt/collie/profile"

const sharedReadme = `# Collie global profile

Shared by every user's isolated projects; each user can add their own
personal profile on top (Account → Profile in the UI). Edit here or in the UI
(Admin → Global profile); changes apply the next time an agent starts.

  claude/CLAUDE.md      instructions for Claude Code
  claude/skills/        skills (one folder per skill, with SKILL.md)
  claude/agents/        sub-agents
  claude/commands/      slash commands
  claude/mcp.json       MCP servers for Claude Code (Playwright is on by default)
  claude/settings.json  extra Claude Code settings (permissions, hooks, env…)
  claude/plugins.json   Claude Code plugins (edit under Admin → Global plugins)
  codex/AGENTS.md       instructions for Codex
  codex/skills/         Codex skills
  codex/config.toml     Codex config (MCP servers…)
  gitconfig             ~/.gitconfig in the containers
  env                   KEY=VALUE lines exported in every container

Project-specific settings stay in each repository (.claude/, .mcp.json,
AGENTS.md). The server user's own ~/.claude and ~/.codex are never shared
with containers.
`

const personalReadme = `# Your personal profile

Merged on top of the global profile for all of your projects: personal
skills / agents / commands win over global ones with the same name, MCP
servers and settings are merged, and the .md instructions are appended.
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

const defaultCodexConfig = `# Codex configuration (merged into ~/.codex/config.toml in the containers).

[mcp_servers.playwright]
command = "playwright-mcp"
args = ["--browser", "chromium", "--headless", "--isolated", "--no-sandbox"]
`

const defaultEnv = `# KEY=VALUE lines exported in every isolated project's container.
`

func seedFiles(dir string, files map[string]string) error {
	for _, d := range []string{"claude/skills", "claude/agents", "claude/commands", "codex/skills"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return err
		}
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if _, err := os.Lstat(p); err == nil {
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

// SeedShared creates the global profile with defaults the first time.
// Existing files are never overwritten.
func SeedShared(dir string) error {
	return seedFiles(dir, map[string]string{
		"README.md":            sharedReadme,
		"claude/CLAUDE.md":     "",
		"claude/mcp.json":      defaultMCP,
		"claude/settings.json": "{}\n",
		"codex/AGENTS.md":      "",
		"codex/config.toml":    defaultCodexConfig,
		"env":                  defaultEnv,
		"gitconfig":            "# ~/.gitconfig inside isolated projects (each user's personal profile adds [user])\n[init]\n\tdefaultBranch = main\n# HTTPS pushes use GH_TOKEN (set it in your account or project secrets)\n[credential \"https://github.com\"]\n\thelper = !gh auth git-credential\n",
	})
}

// PersonalDir is a user's personal profile.
func (m *Manager) PersonalDir(userID string) string {
	return filepath.Join(m.UserDir(userID), "profile")
}

// EffectiveDir is the merged profile mounted into the user's containers.
func (m *Manager) EffectiveDir(userID string) string {
	return filepath.Join(m.UserDir(userID), "effective")
}

// SeedPersonal creates a user's personal profile (and identity for git).
func (m *Manager) SeedPersonal(userID, name, displayName string) error {
	return seedFiles(m.PersonalDir(userID), map[string]string{
		"README.md":            personalReadme,
		"claude/CLAUDE.md":     "",
		"claude/mcp.json":      "{\n  \"mcpServers\": {}\n}\n",
		"claude/settings.json": "{}\n",
		"codex/AGENTS.md":      "",
		"codex/config.toml":    "# Your Codex settings (merged over the global ones)\n",
		"gitconfig":            fmt.Sprintf("[user]\n\tname = %s\n\temail = %s\n", displayName, NoReplyEmail(name)),
	})
}

// NoReplyEmail is the git email a user's profile starts with.
func NoReplyEmail(name string) string { return name + "@users.noreply.collie" }

// GitIdentity is who the user's agents commit as in isolated projects: the
// [user] of their personal profile, over the global one ("" if unset).
func (m *Manager) GitIdentity(userID string) (name, email string) {
	for _, f := range []string{filepath.Join(m.SharedPath(), "gitconfig"), filepath.Join(m.PersonalDir(userID), "gitconfig")} {
		name = cmp.Or(gitutil.ConfigValue(f, "user.name"), name)
		email = cmp.Or(gitutil.ConfigValue(f, "user.email"), email)
	}
	return name, email
}

// BuildEffective (re)writes the user's merged profile. Files are rewritten
// in place and directories keep their identity, because running containers
// have them bind-mounted.
func (m *Manager) BuildEffective(userID string) error {
	l := m.lock("profile/" + userID)
	l.Lock()
	defer l.Unlock()
	if err := SeedShared(m.SharedPath()); err != nil {
		return err
	}
	if err := os.MkdirAll(m.PersonalDir(userID), 0o700); err != nil {
		return err
	}
	g, u, e := m.SharedPath(), m.PersonalDir(userID), m.EffectiveDir(userID)
	for _, d := range []string{"claude/skills", "claude/agents", "claude/commands", "codex/skills"} {
		if err := os.MkdirAll(filepath.Join(e, d), 0o755); err != nil {
			return err
		}
		if err := unionDir(filepath.Join(e, d), filepath.Join(g, d), filepath.Join(u, d)); err != nil {
			return err
		}
	}
	read := func(root, rel string) string {
		b, _ := os.ReadFile(filepath.Join(root, rel))
		return string(b)
	}
	write := func(rel, content string) error {
		return os.WriteFile(filepath.Join(e, rel), []byte(content), 0o644)
	}
	if err := write("claude/CLAUDE.md", concatText(read(g, "claude/CLAUDE.md"), read(u, "claude/CLAUDE.md"))); err != nil {
		return err
	}
	if err := write("codex/AGENTS.md", concatText(read(g, "codex/AGENTS.md"), read(u, "codex/AGENTS.md"))); err != nil {
		return err
	}
	if err := write("gitconfig", concatText(read(g, "gitconfig"), read(u, "gitconfig"))); err != nil {
		return err
	}
	mcp, err := mergeMCP(read(g, "claude/mcp.json"), read(u, "claude/mcp.json"))
	if err != nil {
		log.Printf("profile %s: %v", userID, err)
	}
	if err := write("claude/mcp.json", mcp); err != nil {
		return err
	}
	settings, err := mergeJSONText(read(g, "claude/settings.json"), read(u, "claude/settings.json"))
	if err != nil {
		log.Printf("profile %s: %v", userID, err)
	}
	if err := write("claude/settings.json", settings); err != nil {
		return err
	}
	return write("codex/config.toml", mergeTOML(read(g, "codex/config.toml"), read(u, "codex/config.toml")))
}

func concatText(a, b string) string {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	switch {
	case a == "" && b == "":
		return ""
	case a == "":
		return b + "\n"
	case b == "":
		return a + "\n"
	}
	return a + "\n\n" + b + "\n"
}

// unionDir makes dst contain the top-level entries of a, then b (b wins),
// clearing dst's children first (dst itself is kept: it may be mounted).
func unionDir(dst, a, b string) error {
	old, _ := os.ReadDir(dst)
	for _, e := range old {
		if err := os.RemoveAll(filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	for _, src := range []string{a, b} {
		entries, err := os.ReadDir(src)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Type()&fs.ModeSymlink != 0 {
				continue
			}
			target := filepath.Join(dst, e.Name())
			_ = os.RemoveAll(target)
			if err := copyHostTree(filepath.Join(src, e.Name()), target); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyHostTree copies regular files and directories (never symlinks).
func copyHostTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(target, b, 0o644)
		}
		return nil
	})
}

func parseJSONObject(s string) (map[string]any, error) {
	out := map[string]any{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return map[string]any{}, err
	}
	return out, nil
}

func mergeMCP(global, personal string) (string, error) {
	g, err1 := parseJSONObject(global)
	p, err2 := parseJSONObject(personal)
	servers := map[string]any{}
	for _, src := range []map[string]any{g, p} {
		if m, ok := src["mcpServers"].(map[string]any); ok {
			for k, v := range m {
				servers[k] = v
			}
		}
	}
	b, _ := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	return string(b) + "\n", errors.Join(err1, err2)
}

func mergeJSONText(global, personal string) (string, error) {
	g, err1 := parseJSONObject(global)
	p, err2 := parseJSONObject(personal)
	b, _ := json.MarshalIndent(deepMerge(g, p), "", "  ")
	return string(b) + "\n", errors.Join(err1, err2)
}

// deepMerge merges objects recursively; arrays are concatenated (without
// duplicates) and other values from b win.
func deepMerge(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, bv := range b {
		av, ok := out[k]
		if !ok {
			out[k] = bv
			continue
		}
		am, aok := av.(map[string]any)
		bm, bok := bv.(map[string]any)
		if aok && bok {
			out[k] = deepMerge(am, bm)
			continue
		}
		as, aok := av.([]any)
		bs, bok := bv.([]any)
		if aok && bok {
			merged := append([]any{}, as...)
			seen := map[string]bool{}
			for _, x := range as {
				j, _ := json.Marshal(x)
				seen[string(j)] = true
			}
			for _, x := range bs {
				j, _ := json.Marshal(x)
				if !seen[string(j)] {
					seen[string(j)] = true
					merged = append(merged, x)
				}
			}
			out[k] = merged
			continue
		}
		out[k] = bv
	}
	return out
}

var (
	tomlHeader = regexp.MustCompile(`^\s*\[\[?\s*[^\]]+?\s*\]\]?\s*(#.*)?$`)
	tomlKey    = regexp.MustCompile(`^\s*([A-Za-z0-9_."-]+)\s*=`)
)

// mergeTOML merges two TOML documents by table: a table present in b
// replaces the same table of a, and top-level keys of b replace a's.
// (Good enough for configuration files; no full TOML parser needed.)
func mergeTOML(a, b string) string {
	type doc struct {
		top    []string
		tables []string // header order
		body   map[string][]string
	}
	parse := func(s string) doc {
		d := doc{body: map[string][]string{}}
		cur := ""
		for _, line := range strings.Split(s, "\n") {
			if tomlHeader.MatchString(line) {
				cur = strings.Join(strings.Fields(line), "")
				if i := strings.Index(cur, "#"); i >= 0 {
					cur = cur[:i]
				}
				if _, ok := d.body[cur]; !ok {
					d.tables = append(d.tables, cur)
				}
				d.body[cur] = append(d.body[cur], line)
				continue
			}
			if cur == "" {
				d.top = append(d.top, line)
			} else {
				d.body[cur] = append(d.body[cur], line)
			}
		}
		return d
	}
	da, db := parse(a), parse(b)
	bKeys := map[string]bool{}
	for _, l := range db.top {
		if m := tomlKey.FindStringSubmatch(l); m != nil {
			bKeys[m[1]] = true
		}
	}
	var out []string
	for _, l := range da.top {
		if m := tomlKey.FindStringSubmatch(l); m != nil && bKeys[m[1]] {
			continue
		}
		out = append(out, l)
	}
	out = append(out, db.top...)
	for _, t := range da.tables {
		if _, ok := db.body[t]; ok {
			continue
		}
		out = append(out, da.body[t]...)
	}
	for _, t := range db.tables {
		out = append(out, db.body[t]...)
	}
	return strings.TrimSpace(strings.Join(out, "\n")) + "\n"
}

// ClaudeArgs are extra command-line flags that load the user's effective
// MCP servers and settings into Claude Code inside the container.
func (m *Manager) ClaudeArgs(userID string) []string {
	var args []string
	e := m.EffectiveDir(userID)
	if b, err := os.ReadFile(filepath.Join(e, "claude", "mcp.json")); err == nil {
		var v struct {
			Servers map[string]any `json:"mcpServers"`
		}
		if json.Unmarshal(b, &v) == nil && len(v.Servers) > 0 {
			args = append(args, "--mcp-config", ProfileDir+"/claude/mcp.json")
		}
	}
	if b, err := os.ReadFile(filepath.Join(e, "claude", "settings.json")); err == nil {
		var v map[string]any
		if json.Unmarshal(b, &v) == nil && len(v) > 0 {
			args = append(args, "--settings", ProfileDir+"/claude/settings.json")
		}
	}
	return args
}

// PrepareCodex copies the user's effective Codex config into their
// CODEX_HOME so that the next app-server start picks it up.
func (m *Manager) PrepareCodex(userID string) error {
	src := filepath.Join(m.EffectiveDir(userID), "codex", "config.toml")
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	if err := os.MkdirAll(m.CodexDir(userID), 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(m.CodexDir(userID))
	if err != nil {
		return err
	}
	defer root.Close()
	return copyInto(root, src, "config.toml")
}

// StageUpload copies a file the user attached into their container home
// for the project, as name (a path relative to the home), and returns its
// path inside the container.
func (m *Manager) StageUpload(projectID, userID, hostPath, name string) (string, error) {
	root, err := os.OpenRoot(m.HomeDir(projectID, userID))
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := copyInto(root, hostPath, name); err != nil {
		return "", err
	}
	return Home + "/" + name, nil
}

// RemoveStaged deletes a file or folder (relative to the home) from the
// user's container home for the project.
func (m *Manager) RemoveStaged(projectID, userID, name string) error {
	root, err := os.OpenRoot(m.HomeDir(projectID, userID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	defer root.Close()
	return root.RemoveAll(name)
}

// StageDir creates a folder (relative to the home) in the user's container
// home for the project and returns its path inside the container.
func (m *Manager) StageDir(projectID, userID, name string) (string, error) {
	root, err := os.OpenRoot(m.HomeDir(projectID, userID))
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := root.MkdirAll(name, 0o700); err != nil {
		return "", err
	}
	return Home + "/" + name, nil
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
// the container home the first time the project runs isolated. It is only
// used for the server's owner — that state is theirs.
//
// Only entries for exactly the project directory and the working
// directories of its own sessions are copied. Prefix matching would not be
// safe: the key mapping is lossy ("/" and "-" both become "-"), so
// "/x/app" + "-" also prefixes the key of an unrelated "/x/app-secret".
func migrateClaudeState(p model.Project, root *os.Root, sessionDirs []string, hostHome string) error {
	if entries, err := fs.ReadDir(root.FS(), ".claude/projects"); err == nil {
		for _, e := range entries {
			if e.Name() != claudeKey(p.Path) {
				return nil // already initialised
			}
		}
		// only the (possibly empty) shared-memory mount point exists yet
		if mem, err := fs.ReadDir(root.FS(), ".claude/projects/"+claudeKey(p.Path)); err == nil && len(mem) > 1 {
			return nil
		}
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

// ---- profile editing (UI) ----

type ProfileFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Dir  bool   `json:"dir,omitempty"`
}

var profilePathRe = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

const maxProfileFile = 512 << 10

// cleanProfilePath validates a path relative to a profile root. The env
// file is managed separately (it holds secrets).
func cleanProfilePath(rel string) (string, error) {
	rel = strings.Trim(path.Clean("/"+rel), "/")
	if rel == "" || !profilePathRe.MatchString(rel) || strings.Contains(rel, "..") || rel == "env" || strings.HasSuffix(rel, ".collie-tmp") {
		return "", errors.New("invalid path")
	}
	return rel, nil
}

func ListProfile(dir string) ([]ProfileFile, error) {
	out := []ProfileFile{}
	root, err := os.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." || p == "env" || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, ProfileFile{Path: p, Size: info.Size(), Dir: d.IsDir()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

func ReadProfileFile(dir, rel string) (string, error) {
	rel, err := cleanProfilePath(rel)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if fi, err := root.Stat(rel); err == nil && fi.Size() > maxProfileFile {
		return "", errors.New("file too large")
	}
	b, err := root.ReadFile(rel)
	return string(b), err
}

func WriteProfileFile(dir, rel, content string) error {
	rel, err := cleanProfilePath(rel)
	if err != nil {
		return err
	}
	if len(content) > maxProfileFile {
		return errors.New("file too large (max 512 KB)")
	}
	if strings.HasSuffix(rel, ".json") {
		var v any
		if err := json.Unmarshal([]byte(content), &v); err != nil {
			return fmt.Errorf("invalid JSON: %v", err)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if d := path.Dir(rel); d != "." {
		if err := root.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	tmp := rel + ".collie-tmp"
	_ = root.Remove(tmp)
	if err := root.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return root.Rename(tmp, rel)
}

func DeleteProfilePath(dir, rel string) error {
	rel, err := cleanProfilePath(rel)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.RemoveAll(rel)
}
