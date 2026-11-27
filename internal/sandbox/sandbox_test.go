package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tattsun/collie/internal/model"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	data := t.TempDir()
	m := &Manager{cfg: Config{DataDir: data}, locks: map[string]*sync.Mutex{}, stale: map[string]bool{},
		imgIDs: map[string]imageIDAt{}, custom: map[string]*customImage{}, customSem: make(chan struct{}, 1)}
	if err := SeedShared(m.SharedPath()); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMigrateClaudeStateExactKeysOnly(t *testing.T) {
	hostHome := t.TempDir()
	projects := filepath.Join(hostHome, ".claude", "projects")
	p := model.Project{ID: "p1", Path: "/x/app"}
	wt := "/data/worktrees/s1"
	for _, dir := range []string{"/x/app", "/x/app-secret", "/x/app/sub", wt, "/x/other"} {
		d := filepath.Join(projects, claudeKey(dir))
		if err := os.MkdirAll(filepath.Join(d, "memory"), 0o700); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(d, "memory", "MEMORY.md"), []byte(dir), 0o600)
	}
	home := t.TempDir()
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := migrateClaudeState(p, root, []string{wt}, hostHome); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadDir(filepath.Join(home, ".claude", "projects"))
	var names []string
	for _, e := range got {
		names = append(names, e.Name())
	}
	want := []string{claudeKey(wt), claudeKey("/x/app")}
	slices.Sort(want)
	slices.Sort(names)
	if !slices.Equal(names, want) {
		t.Fatalf("migrated %v, want only %v (no app-secret / other)", names, want)
	}
}

func TestHostWritesStayInsideHome(t *testing.T) {
	m := newTestManager(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	os.WriteFile(victim, []byte("original"), 0o600)
	home := m.HomeDir("p1", "u1")
	codex := m.CodexDir("u1")
	os.MkdirAll(home, 0o700)
	os.MkdirAll(codex, 0o700)
	if err := m.BuildEffective("u1"); err != nil {
		t.Fatal(err)
	}

	// 1. temp file planted (by the agent, in its CODEX_HOME) as a symlink to a server file
	os.Symlink(victim, filepath.Join(codex, "config.toml.collie-tmp"))
	if err := m.PrepareCodex("u1"); err != nil {
		t.Fatalf("PrepareCodex: %v", err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "original" {
		t.Fatalf("victim overwritten through planted temp symlink: %q", b)
	}

	// 2. the config file itself replaced by a symlink pointing outside
	os.Remove(filepath.Join(codex, "config.toml"))
	os.Symlink(victim, filepath.Join(codex, "config.toml"))
	if err := m.PrepareCodex("u1"); err != nil {
		t.Fatalf("PrepareCodex: %v", err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "original" {
		t.Fatalf("victim overwritten through planted config symlink: %q", b)
	}

	// 3. uploads directory symlinked outside
	os.Symlink(outside, filepath.Join(home, ".collie-uploads"))
	img := filepath.Join(t.TempDir(), "0123456789abcdef.png")
	os.WriteFile(img, []byte("png"), 0o600)
	if _, err := m.StageUpload("p1", "u1", img); err == nil {
		t.Fatal("expected StageUpload to refuse a symlinked uploads dir")
	}
	if _, err := os.Stat(filepath.Join(outside, "0123456789abcdef.png")); err == nil {
		t.Fatal("upload was written outside the home")
	}
}

func TestExtraMountsForLegacyWorktrees(t *testing.T) {
	m := newTestManager(t)
	p := model.Project{ID: "p1", Path: t.TempDir()}
	legacy := filepath.Join(m.cfg.DataDir, "worktrees", "oldsession")
	current := filepath.Join(m.WorktreeRoot(p.ID), "newsession")
	for _, d := range []string{legacy, current} {
		os.MkdirAll(d, 0o700)
	}
	got := m.extraMounts(p, []string{p.Path, filepath.Join(p.Path, "sub"), current, legacy, legacy, "/does/not/exist"})
	if !slices.Equal(got, []string{legacy}) {
		t.Fatalf("extra mounts = %v, want [%s]", got, legacy)
	}
}

func TestProfileMerge(t *testing.T) {
	m := newTestManager(t)
	g := m.SharedPath()
	if err := m.SeedPersonal("u1", "alice", "Alice"); err != nil {
		t.Fatal(err)
	}
	u := m.PersonalDir("u1")
	w := func(root, rel, c string) {
		os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755)
		os.WriteFile(filepath.Join(root, rel), []byte(c), 0o644)
	}
	w(g, "claude/CLAUDE.md", "global rules")
	w(u, "claude/CLAUDE.md", "my rules")
	w(g, "claude/skills/shared/SKILL.md", "global skill")
	w(g, "claude/skills/both/SKILL.md", "global version")
	w(u, "claude/skills/both/SKILL.md", "personal version")
	w(u, "claude/skills/mine/SKILL.md", "my skill")
	w(u, "claude/mcp.json", `{"mcpServers":{"github":{"command":"gh-mcp"}}}`)
	w(g, "claude/settings.json", `{"permissions":{"allow":["Bash(ls)"]},"model":"a"}`)
	w(u, "claude/settings.json", `{"permissions":{"allow":["Bash(git status)"]},"model":"b"}`)
	w(u, "codex/config.toml", "model = \"x\"\n\n[mcp_servers.playwright]\ncommand = \"mine\"\n")
	if err := m.BuildEffective("u1"); err != nil {
		t.Fatal(err)
	}
	e := m.EffectiveDir("u1")
	read := func(rel string) string { b, _ := os.ReadFile(filepath.Join(e, rel)); return string(b) }
	if c := read("claude/CLAUDE.md"); c != "global rules\n\nmy rules\n" {
		t.Errorf("CLAUDE.md = %q", c)
	}
	if read("claude/skills/shared/SKILL.md") != "global skill" || read("claude/skills/mine/SKILL.md") != "my skill" || read("claude/skills/both/SKILL.md") != "personal version" {
		t.Errorf("skills not unioned with personal priority")
	}
	mcp := read("claude/mcp.json")
	if !strings.Contains(mcp, "playwright") || !strings.Contains(mcp, "github") {
		t.Errorf("mcp.json = %s", mcp)
	}
	st := read("claude/settings.json")
	if !strings.Contains(st, "Bash(ls)") || !strings.Contains(st, "Bash(git status)") || !strings.Contains(st, `"model": "b"`) {
		t.Errorf("settings.json = %s", st)
	}
	toml := read("codex/config.toml")
	if strings.Count(toml, "[mcp_servers.playwright]") != 1 || !strings.Contains(toml, `command = "mine"`) || !strings.Contains(toml, `model = "x"`) {
		t.Errorf("config.toml = %s", toml)
	}
	if gc := read("gitconfig"); !strings.Contains(gc, "name = Alice") || !strings.Contains(gc, "defaultBranch") {
		t.Errorf("gitconfig = %s", gc)
	}
	// rebuilding keeps directory identity (bind mounts) and drops removed skills
	before, _ := os.Stat(filepath.Join(e, "claude", "skills"))
	os.RemoveAll(filepath.Join(u, "claude", "skills", "mine"))
	m.BuildEffective("u1")
	after, _ := os.Stat(filepath.Join(e, "claude", "skills"))
	if !os.SameFile(before, after) {
		t.Error("skills directory was replaced (would break running containers' mounts)")
	}
	if _, err := os.Stat(filepath.Join(e, "claude", "skills", "mine")); err == nil {
		t.Error("removed personal skill still present")
	}
}

func TestGitIdentity(t *testing.T) {
	m := newTestManager(t)
	if n, e := m.GitIdentity("u1"); n != "" || e != "" {
		t.Errorf("no profile yet: %q %q", n, e)
	}
	if err := m.SeedPersonal("u1", "alice", "Alice Liddell"); err != nil {
		t.Fatal(err)
	}
	if n, e := m.GitIdentity("u1"); n != "Alice Liddell" || e != "alice@users.noreply.collie" {
		t.Errorf("seeded: %q %q", n, e)
	}
	// The global profile sets an email, the personal one overrides the name only.
	os.WriteFile(filepath.Join(m.SharedPath(), "gitconfig"), []byte("[user]\n\temail = team@example.com\n"), 0o644)
	os.WriteFile(filepath.Join(m.PersonalDir("u1"), "gitconfig"), []byte("[user]\n\tname = Al\n"), 0o644)
	if n, e := m.GitIdentity("u1"); n != "Al" || e != "team@example.com" {
		t.Errorf("merged: %q %q", n, e)
	}
}

func TestProfileEditorPaths(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"env", "x.collie-tmp", "", "a b", "~/x"} {
		if err := WriteProfileFile(dir, bad, "x"); err == nil {
			t.Errorf("write %q accepted", bad)
		}
	}
	// ".." and absolute paths are confined to the profile directory
	for _, p := range []string{"../x", "/etc/passwd", "a/../../b"} {
		_ = WriteProfileFile(dir, p, "x")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "x")); err == nil {
		t.Error("write escaped the profile directory")
	}
	if err := WriteProfileFile(dir, "claude/skills/demo/SKILL.md", "hi"); err != nil {
		t.Fatal(err)
	}
	if err := WriteProfileFile(dir, "claude/mcp.json", "{not json"); err == nil {
		t.Error("invalid JSON accepted")
	}
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(dir, "link"))
	if err := WriteProfileFile(dir, "link/x", "y"); err == nil {
		t.Error("write through symlink accepted")
	}
}

// Env files are passed at exec time (so a saved token reaches the next
// agent without re-creating the container), in override order and before
// explicit -e values.
func TestExecPassesEnvFiles(t *testing.T) {
	m := newTestManager(t)
	m.cfg.Podman = "podman"
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(m.SharedEnvPath(), "A=shared\n")
	write(m.UserEnvPath("u1"), "GH_TOKEN=tok\n")
	write(m.ProjectEnvPath("p1"), "") // empty files are skipped

	argv, _ := m.Exec("p1", "u1", "/work", "id1", false, []string{"GIT_TERMINAL_PROMPT=0"}, []string{"true"})
	var files []string
	firstE := -1
	for i, a := range argv {
		if a == "--env-file" {
			files = append(files, argv[i+1])
		}
		if a == "-e" && firstE < 0 {
			firstE = i
		}
	}
	if want := []string{m.SharedEnvPath(), m.UserEnvPath("u1")}; !slices.Equal(files, want) {
		t.Fatalf("env files = %v, want %v", files, want)
	}
	if last := slices.Index(argv, m.UserEnvPath("u1")); firstE < last {
		t.Fatalf("-e must come after the env files: %v", argv)
	}
	if !slices.Contains(argv, Name("p1", "u1")) {
		t.Fatalf("container name missing: %v", argv)
	}
}
