package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/tattsun/collie/internal/model"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	data := t.TempDir()
	m := &Manager{cfg: Config{DataDir: data}}
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
	home := m.HomeDir("p1")
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)

	// 1. temp file planted as a symlink to a server file
	os.Symlink(victim, filepath.Join(home, ".codex", "config.toml.collie-tmp"))
	if err := m.PrepareCodex("p1"); err != nil {
		t.Fatalf("PrepareCodex: %v", err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "original" {
		t.Fatalf("victim overwritten through planted temp symlink: %q", b)
	}

	// 2. directory replaced by a symlink pointing outside the home
	os.RemoveAll(filepath.Join(home, ".codex"))
	os.Symlink(outside, filepath.Join(home, ".codex"))
	if err := m.PrepareCodex("p1"); err == nil {
		t.Fatal("expected PrepareCodex to refuse writing through a symlinked directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "config.toml")); err == nil {
		t.Fatal("config.toml was written outside the home")
	}

	// 3. uploads directory symlinked outside
	os.Symlink(outside, filepath.Join(home, ".collie-uploads"))
	img := filepath.Join(t.TempDir(), "0123456789abcdef.png")
	os.WriteFile(img, []byte("png"), 0o600)
	if _, err := m.StageUpload("p1", img); err == nil {
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
