package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/model"
)

// mounts returns the -v sources and targets of podman run arguments.
func mounts(args []string) map[string]string {
	out := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-v" {
			src, dst, _ := strings.Cut(args[i+1], ":")
			dst, _, _ = strings.Cut(dst, ":")
			out[dst] = src
		}
	}
	return out
}

// A container's CODEX_HOME is its own: only the login directory is shared
// between the user's containers, and the old shared CODEX_HOME is mounted
// nowhere.
func TestCodexHomePerProject(t *testing.T) {
	m := newTestManager(t)
	a := model.Project{ID: "pa", Path: t.TempDir()}
	b := model.Project{ID: "pb", Path: t.TempDir()}
	for _, p := range []model.Project{a, b} {
		mv := mounts(m.runArgs(Spec{Project: p, UserID: "u1"}, nil))
		if got := mv[Home]; got != m.HomeDir(p.ID, "u1") {
			t.Fatalf("%s: home is %q", p.ID, got)
		}
		if src, ok := mv[Home+"/.codex"]; ok {
			t.Fatalf("%s: ~/.codex is mounted from %s; it must be the project's own", p.ID, src)
		}
		if got := mv[codexAuthMount]; got != m.CodexAuthDir("u1") {
			t.Fatalf("%s: login dir mounted from %q", p.ID, got)
		}
		for dst, src := range mv {
			if src == m.legacyCodexDir("u1") {
				t.Fatalf("%s: the old shared CODEX_HOME is still mounted at %s", p.ID, dst)
			}
		}
	}
	for _, p := range []model.Project{a, b} {
		if err := m.prepare(Spec{Project: p, UserID: "u1"}); err != nil {
			t.Fatal(err)
		}
		home := m.HomeDir(p.ID, "u1")
		if got, _ := os.Readlink(filepath.Join(home, ".codex", "auth.json")); got != codexAuthLink {
			t.Fatalf("%s: auth.json links to %q", p.ID, got)
		}
		for _, mp := range []string{".codex/AGENTS.md", ".codex/skills", ".codex-auth"} {
			if _, err := os.Lstat(filepath.Join(home, mp)); err != nil {
				t.Fatalf("%s: mount point %s: %v", p.ID, mp, err)
			}
		}
	}
	// What one project's agent leaves in its CODEX_HOME stays there.
	os.WriteFile(filepath.Join(m.HomeDir(a.ID, "u1"), ".codex", ".env"), []byte("BASH_ENV=/x\n"), 0o600)
	if _, err := os.Stat(filepath.Join(m.HomeDir(b.ID, "u1"), ".codex", ".env")); err == nil {
		t.Fatal("project a's ~/.codex/.env shows up in project b")
	}
}

func TestLinkCodexAuth(t *testing.T) {
	m := newTestManager(t)
	home := t.TempDir()
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	link := filepath.Join(home, ".codex", "auth.json")
	shared := filepath.Join(m.CodexAuthDir("u1"), "auth.json")
	check := func(step string) {
		t.Helper()
		if err := m.linkCodexAuth(root, "u1"); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if got, _ := os.Readlink(link); got != codexAuthLink {
			t.Fatalf("%s: auth.json links to %q", step, got)
		}
	}
	check("fresh")
	check("again")

	os.Remove(link)
	os.Symlink("/etc/passwd", link)
	check("foreign link")

	// A login of its own (after `codex logout` removed the link) newer than
	// the shared one becomes the user's.
	os.MkdirAll(m.CodexAuthDir("u1"), 0o700)
	os.WriteFile(shared, []byte(`{"old":1}`), 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(shared, old, old)
	os.Remove(link)
	os.WriteFile(link, []byte(`{"new":1}`), 0o600)
	check("newer own login")
	if b, _ := os.ReadFile(shared); string(b) != `{"new":1}` {
		t.Fatalf("shared login = %s, want the newer one", b)
	}
	// An older one doesn't replace it.
	os.Remove(link)
	os.WriteFile(link, []byte(`{"older":1}`), 0o600)
	os.Chtimes(link, old, old)
	check("older own login")
	if b, _ := os.ReadFile(shared); string(b) != `{"new":1}` {
		t.Fatalf("shared login = %s, want it kept", b)
	}

	os.Remove(link)
	os.MkdirAll(filepath.Join(link, "x"), 0o700)
	check("directory in its place")

	if !m.CodexLoggedIn("u1") {
		t.Fatal("CodexLoggedIn = false with a login")
	}
	os.Remove(shared)
	os.Symlink("/etc/passwd", shared)
	if m.CodexLoggedIn("u1") {
		t.Fatal("CodexLoggedIn follows a planted symlink")
	}
}

func TestMigrateCodex(t *testing.T) {
	m := newTestManager(t)
	a := model.Project{ID: "pa", Path: filepath.Join(t.TempDir(), "app")}
	b := model.Project{ID: "pb", Path: filepath.Join(t.TempDir(), "lib")}
	old := m.legacyCodexDir("u1")
	day := filepath.Join(old, "sessions", "2026", "11", "27")
	os.MkdirAll(day, 0o700)
	write := func(name, cwd string) {
		line := `{"timestamp":"x","type":"session_meta","payload":{"id":"t","cwd":"` + cwd + `","base_instructions":{"text":"` + strings.Repeat("x", 100<<10) + `"}}}` + "\n"
		if err := os.WriteFile(filepath.Join(day, name), []byte(line+`{"type":"event_msg"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("rollout-a.jsonl", a.Path)
	write("rollout-a-sub.jsonl", filepath.Join(a.Path, "pkg"))
	write("rollout-b.jsonl", filepath.Join(m.WorktreeRoot(b.ID), "s1"))
	write("rollout-elsewhere.jsonl", a.Path+"-secret")
	victim := filepath.Join(t.TempDir(), "victim.jsonl")
	os.WriteFile(victim, []byte(`{"type":"session_meta","payload":{"cwd":"`+a.Path+`"}}`+"\n"), 0o600)
	os.Symlink(victim, filepath.Join(day, "rollout-link.jsonl"))
	if err := syscall.Mkfifo(filepath.Join(day, "rollout-fifo.jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(old, "auth.json"), []byte(`{"tokens":1}`), 0o600)
	os.WriteFile(filepath.Join(old, ".env"), []byte("X=1\n"), 0o600)

	done := make(chan struct{})
	go func() { m.MigrateCodex([]model.Project{a, b}); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("MigrateCodex hangs (on the FIFO?)")
	}

	if got, _ := os.ReadFile(filepath.Join(m.CodexAuthDir("u1"), "auth.json")); string(got) != `{"tokens":1}` {
		t.Fatalf("login = %q", got)
	}
	rel := filepath.Join(".codex", "sessions", "2026", "11", "27")
	has := func(p model.Project, name string) bool {
		_, err := os.Stat(filepath.Join(m.HomeDir(p.ID, "u1"), rel, name))
		return err == nil
	}
	for _, c := range []struct {
		p    model.Project
		name string
	}{{a, "rollout-a.jsonl"}, {a, "rollout-a-sub.jsonl"}, {b, "rollout-b.jsonl"}} {
		if !has(c.p, c.name) {
			t.Fatalf("%s not moved to %s", c.name, c.p.ID)
		}
		if _, err := os.Lstat(filepath.Join(day, c.name)); err == nil {
			t.Fatalf("%s left in the old directory", c.name)
		}
	}
	for _, name := range []string{"rollout-elsewhere.jsonl", "rollout-link.jsonl", "rollout-fifo.jsonl"} {
		if has(a, name) || has(b, name) {
			t.Fatalf("%s should not have been moved", name)
		}
	}
	if b, _ := os.ReadFile(victim); !strings.Contains(string(b), "session_meta") {
		t.Fatal("victim changed")
	}
	if _, err := os.Stat(filepath.Join(old, ".env")); err != nil {
		t.Fatal("the rest should stay behind")
	}
	if _, err := os.Stat(filepath.Join(old, codexMigrated)); err != nil {
		t.Fatal("no marker")
	}
	// Once only.
	write("rollout-late.jsonl", a.Path)
	m.MigrateCodex([]model.Project{a, b})
	if has(a, "rollout-late.jsonl") {
		t.Fatal("migrated twice")
	}
	got, _ := os.ReadDir(filepath.Join(m.HomeDir(a.ID, "u1"), rel))
	var names []string
	for _, e := range got {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"rollout-a-sub.jsonl", "rollout-a.jsonl"}) {
		t.Fatalf("project a has %v", names)
	}
}

// Answers of commands run in a container are capped: its head (or
// whatever podman exec runs) may print without end.
func TestContainerOutputIsCapped(t *testing.T) {
	m := newTestManager(t)
	fake := filepath.Join(t.TempDir(), "podman")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec yes AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	m.cfg.Podman = fake
	done := make(chan struct{})
	go func() {
		defer close(done)
		if b, err := m.ReadFile("p1", "u1", "/x", 1<<20); err == nil {
			t.Errorf("ReadFile returned %d bytes from an endless head", len(b))
		}
		if _, _, err := m.TailFile("p1", "u1", "/x", 1<<20); err == nil {
			t.Error("TailFile accepted an endless tail")
		}
		if out, err := m.podman(context.Background(), "exec", "c", "ps"); err == nil || len(out) > maxPodmanOutput {
			t.Errorf("podman kept %d bytes, err %v", len(out), err)
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("reading an endless answer doesn't stop")
	}
}

// Signing out removes the one login all of the user's containers link to.
func TestCodexLogoutRemovesTheSharedLogin(t *testing.T) {
	m := newTestManager(t)
	if err := m.CodexLogout("u1"); err != nil {
		t.Fatalf("no login yet: %v", err)
	}
	dir := m.CodexAuthDir("u1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !m.CodexLoggedIn("u1") {
		t.Fatal("not logged in")
	}
	if err := m.CodexLogout("u1"); err != nil {
		t.Fatal(err)
	}
	if m.CodexLoggedIn("u1") {
		t.Error("still logged in")
	}
}
