package session

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/gitutil"
	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
	"github.com/tattsun/collie/internal/store"
)

// Collie mounts its own executable into the containers, which in tests is
// the test binary: there it answers as `collie git-helper`.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "git-helper" {
		os.Exit(gitutil.ServeHelper(os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

// Runs real containers: COLLIE_INTEGRATION=1 go test ./internal/session -run Integration
// (built with CGO_ENABLED=0 if the test binary doesn't run in the image).
func TestIntegrationGitStartsTheContainer(t *testing.T) {
	if os.Getenv("COLLIE_INTEGRATION") == "" {
		t.Skip("set COLLIE_INTEGRATION=1 (needs podman and the agent image)")
	}
	data := t.TempDir()
	sb := sandbox.New(sandbox.Config{Podman: "podman", DataDir: data})
	if !sb.Available() || !sb.Status().Ready {
		t.Skip("podman or the agent image is not available")
	}
	st, err := store.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: Config{ProjectRoot: t.TempDir()}, st: st, hub: NewHub(), sb: sb, rts: map[string]*Runtime{},
		ctrActive: map[string]time.Time{}, toolLn: map[string]net.Listener{}}
	m.SetAccess(fakeAccess{})
	alice := Actor{ID: "alicegit", Name: "alice"}

	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "first"}} {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	p := model.Project{ID: model.NewID(), Name: "gitstart", Path: repo, OwnerUser: alice.ID, Runtime: "container", Git: true}
	st.PutProject(p)
	t.Cleanup(func() {
		sb.Remove(p.ID)
		m.closeToolSockets(p.ID)
	})
	name := sandbox.Name(p.ID, alice.ID)
	podman := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("podman", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("podman %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	state := func() string {
		out, err := exec.Command("podman", "container", "inspect", "--format", "{{.State.Status}}", name).Output()
		if err != nil {
			return "missing"
		}
		return strings.TrimSpace(string(out))
	}
	started := func() string { return podman("container", "inspect", "--format", "{{.Id}} {{.State.StartedAt}}", name) }
	stop := func(want string) {
		t.Helper()
		if err := sb.Stop(p.ID, alice.ID, false); err != nil {
			t.Fatal(err)
		}
		if want == "created" {
			// A container made but never started, with the same configuration.
			podman("container", "clone", name, name+"-new")
			podman("rm", name)
			podman("rename", name+"-new", name)
		}
		if got := state(); got != want {
			t.Fatalf("container %s, want %s", got, want)
		}
	}

	if err := m.ensureContainer(p, alice.ID, nil); err != nil {
		t.Fatal(err)
	}
	id := strings.Fields(started())[0]

	// 1. Stopped (as idle, or by a restart of Collie): a session with a
	// worktree starts it, the same container.
	stop("exited")
	s, err := m.CreateSession(alice, CreateSessionReq{ProjectID: p.ID, Agent: "claude", Worktree: true})
	if err != nil {
		t.Fatalf("worktree session with the container stopped: %v", err)
	}
	if !s.Worktree || s.Base != gitutil.HeadCommit(repo) {
		t.Fatalf("session = %+v", s)
	}
	if state() != "running" || strings.Fields(started())[0] != id {
		t.Fatalf("container %s (%s, was %s)", state(), started(), id)
	}

	// 2. Only created: the Changes tab starts it.
	stop("created")
	if ch, err := m.Changes(alice, s.ID); err != nil || !ch.Repo || ch.Base != s.Base {
		t.Fatalf("changes with the container only created: %+v, %v", ch, err)
	}
	if state() != "running" {
		t.Fatalf("container %s", state())
	}

	// 3. A failure of the helper itself in the running container is
	// reported, and the container left as it is.
	before := started()
	if _, err := m.runGitHelper(p, alice.ID, true, false, time.Minute, []byte("not a request")); err == nil || !strings.Contains(err.Error(), "collie git-helper:") {
		t.Fatalf("helper given garbage: %v", err)
	}
	if after := started(); after != before {
		t.Fatalf("container restarted for the helper's failure: %s, was %s", after, before)
	}

	// 4. Removing a worktree starts a stopped container…
	stop("exited")
	if err := m.DeleteSession(alice, s.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Cwd); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if out := podman("exec", name, "git", "-C", repo, "worktree", "list"); strings.Contains(out, s.Cwd) {
		t.Fatalf("git still knows the worktree:\n%s", out)
	}

	// 5. …but makes none: without a container the server deletes the
	// folder itself.
	s2, err := m.CreateSession(alice, CreateSessionReq{ProjectID: p.ID, Agent: "claude", Worktree: true})
	if err != nil {
		t.Fatal(err)
	}
	podman("rm", "-f", "-t", "0", name)
	if err := m.DeleteSession(alice, s2.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s2.Cwd); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if state() != "missing" {
		t.Fatalf("removing a worktree made a container: %s", state())
	}

	// 6. Without one, a worktree session makes it.
	s3, err := m.CreateSession(alice, CreateSessionReq{ProjectID: p.ID, Agent: "claude", Worktree: true})
	if err != nil {
		t.Fatalf("worktree session without a container: %v", err)
	}
	if state() != "running" {
		t.Fatalf("container %s", state())
	}
	if _, err := os.Stat(filepath.Join(s3.Cwd, ".git")); err != nil {
		t.Fatal(err)
	}
}
