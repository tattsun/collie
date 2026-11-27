package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tattsun/collie/internal/gitutil"
)

// A new project is a repository without commits: its first worktree
// session gives it an empty initial commit to branch off.
func TestWorktreeInNewProject(t *testing.T) {
	// No git identity anywhere, and none guessed from the host name.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "user.useConfigOnly")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	m, _ := newTestManager(t)
	root := Actor{ID: "root", Name: "root", Admin: true}
	p, err := m.CreateProject(root, CreateProjectReq{Kind: "empty", Name: "fresh", Runtime: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if gitutil.HasCommits(p.Path) {
		t.Fatal("a new project starts without commits")
	}
	s, err := m.CreateSession(root, CreateSessionReq{ProjectID: p.ID, Agent: "claude", Worktree: true})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Worktree || s.Base == "" || s.Base != gitutil.HeadCommit(p.Path) || s.Base != gitutil.HeadCommit(s.Cwd) {
		t.Fatalf("session = %+v, project head %s", s, gitutil.HeadCommit(p.Path))
	}
	out, err := exec.Command("git", "-C", p.Path, "log", "--format=%an <%ae> %s").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "root <root@users.noreply.collie> Initial commit" {
		t.Fatalf("log = %q", got)
	}
	// A second session branches off the same commit.
	s2, err := m.CreateSession(root, CreateSessionReq{ProjectID: p.ID, Agent: "claude", Worktree: true})
	if err != nil || s2.Base != s.Base {
		t.Fatalf("second session: %v, base %s vs %s", err, s2.Base, s.Base)
	}
}

func TestWorktreeNeedsCommittedFiles(t *testing.T) {
	m, _ := newTestManager(t)
	root := Actor{ID: "root", Name: "root", Admin: true}
	p, err := m.CreateProject(root, CreateProjectReq{Kind: "empty", Name: "files", Runtime: "host"})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(p.Path, "main.go"), []byte("package main\n"), 0o644)
	if _, err := m.CreateSession(root, CreateSessionReq{ProjectID: p.ID, Agent: "claude", Worktree: true}); !errors.Is(err, errNoCommits) {
		t.Fatalf("worktree over uncommitted files: %v", err)
	}
	if gitutil.HasCommits(p.Path) {
		t.Fatal("committed anyway")
	}
	no := false
	q, err := m.CreateProject(root, CreateProjectReq{Kind: "empty", Name: "plain", Runtime: "host", Git: &no})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateSession(root, CreateSessionReq{ProjectID: q.ID, Agent: "claude", Worktree: true}); !errors.Is(err, errNoRepo) {
		t.Fatalf("worktree without git: %v", err)
	}
}
