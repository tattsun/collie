package session

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/gitutil"
	"github.com/tattsun/collie/internal/model"
)

// gitInProcess runs isolated projects' git in this process, as `collie
// git-helper` does in their containers.
func gitInProcess(m *Manager) {
	m.gitHelper = func(model.Project, string) gitutil.Ops {
		return gitutil.Helper{Run: func(_ time.Duration, req []byte) ([]byte, error) {
			var out bytes.Buffer
			gitutil.ServeHelper(bytes.NewReader(req), &out)
			return out.Bytes(), nil
		}}
	}
}

// fakeContainerGit answers for an isolated project's container without
// running git, and notes who asked for what.
type fakeContainerGit struct {
	uid  string
	mu   *sync.Mutex
	logs *[]string
}

func (f fakeContainerGit) note(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.logs = append(*f.logs, op+" as "+f.uid)
}

func (f fakeContainerGit) CheckWorktrees(string) error { f.note("check"); return nil }
func (f fakeContainerGit) NewWorktree(_, path, _ string, a gitutil.Author) (string, error) {
	f.note("new-worktree by " + a.Name)
	return strings.Repeat("a", 40), os.MkdirAll(path, 0o700)
}
func (f fakeContainerGit) RemoveWorktree(string, string) error { f.note("remove"); return nil }
func (f fakeContainerGit) Diff(string, string) (*gitutil.Changes, error) {
	f.note("diff")
	return &gitutil.Changes{Repo: true, Files: []gitutil.FileChange{}}, nil
}

// trapRepo is a repository whose configuration and hooks run a command,
// as an agent in an isolated project's container could make it. The
// returned file appears once git on the server runs any of them.
func trapRepo(t *testing.T) (repo, sprung string) {
	t.Helper()
	repo = t.TempDir()
	sprung = filepath.Join(t.TempDir(), "sprung")
	gitIn(t, repo, "init", "-q")
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	trap := "#!/bin/sh\necho \"$0\" >> " + sprung + "\n"
	for _, h := range []string{"post-checkout", "reference-transaction", "post-index-change", "fsmonitor"} {
		if err := os.WriteFile(filepath.Join(repo, ".git", "hooks", h), []byte(trap), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, repo, "config", "core.fsmonitor", filepath.Join(repo, ".git", "hooks", "fsmonitor"))
	return repo, sprung
}

// Git on the server never touches an isolated project's repository: the
// project's agents can rewrite its hooks and configuration. Each operation
// runs in the container of whoever it is for.
func TestIsolatedProjectsGitRunsInTheirContainers(t *testing.T) {
	// The trap works: git on the server springs it.
	repo, sprung := trapRepo(t)
	if _, err := gitutil.Diff(repo, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sprung); err != nil {
		t.Fatal("git on the server didn't run the repository's fsmonitor, so this test proves nothing")
	}

	repo, sprung = trapRepo(t)
	m, _ := newTestManager(t)
	var mu sync.Mutex
	var logs []string
	m.gitHelper = func(_ model.Project, uid string) gitutil.Ops { return fakeContainerGit{uid, &mu, &logs} }
	m.st.PutProject(model.Project{ID: "pt", Name: "team", Path: repo, OwnerGroup: "team", Runtime: "container"})
	alice, bob := Actor{ID: "alice", Name: "alice"}, Actor{ID: "bob", Name: "bob"}

	s, err := m.CreateSession(alice, CreateSessionReq{ProjectID: "pt", Agent: "claude", Worktree: true})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Worktree || s.Base != strings.Repeat("a", 40) {
		t.Fatalf("session = %+v", s)
	}
	if s, err := m.CreateSession(bob, CreateSessionReq{ProjectID: "pt", Agent: "claude"}); err != nil || s.Branch != "main" && s.Branch != "master" {
		t.Fatalf("session without a worktree: %+v, %v", s, err)
	}
	if _, err := m.Changes(bob, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ProjectChanges(bob, "pt"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateSchedule(alice, ScheduleReq{ProjectID: ptr("pt"), Prompt: ptr("hi"), Agent: ptr("claude"), Cron: ptr("0 9 * * *"), Worktree: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteSession(alice, s.ID, true); err != nil {
		t.Fatal(err)
	}
	want := []string{"new-worktree by alice as alice", "diff as bob", "diff as bob", "check as alice", "remove as alice"}
	if !slices.Equal(logs, want) {
		t.Errorf("container git:\n got %q\nwant %q", logs, want)
	}
	if b, err := os.ReadFile(sprung); err == nil {
		t.Errorf("git ran on the server: %s", b)
	}
}
