package session

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
	"github.com/tattsun/collie/internal/store"
)

// Runs real containers: COLLIE_INTEGRATION=1 go test ./internal/session -run Integration
func TestIntegrationMemberCloneRunsInContainer(t *testing.T) {
	if os.Getenv("COLLIE_INTEGRATION") == "" {
		t.Skip("set COLLIE_INTEGRATION=1 (needs podman and the agent image)")
	}
	data, root := t.TempDir(), t.TempDir()
	sb := sandbox.New(sandbox.Config{Podman: "podman", DataDir: data})
	if !sb.Available() || !sb.Status().Ready {
		t.Skip("podman or the agent image is not available")
	}
	st, err := store.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: Config{ProjectRoot: root}, st: st, hub: NewHub(), sb: sb, rts: map[string]*Runtime{}, ctrActive: map[string]time.Time{}, toolLn: map[string]net.Listener{}}
	m.SetAccess(fakeAccess{})
	bob := Actor{ID: "bobintegration", Name: "bob"}
	wait := func(id string) model.Project {
		for i := 0; i < 180; i++ {
			if p, _ := st.Project(id); p.Status != "cloning" {
				return p
			}
			time.Sleep(time.Second)
		}
		t.Fatal("clone did not finish")
		return model.Project{}
	}
	t.Cleanup(func() {
		for _, p := range st.Projects() {
			sb.Remove(p.ID)
		}
	})

	// 1. an ordinary public clone works (from inside bob's container)
	p, err := m.CreateProject(bob, CreateProjectReq{Kind: "clone", URL: "https://github.com/octocat/Hello-World.git"})
	if err != nil {
		t.Fatal(err)
	}
	if p = wait(p.ID); p.Status != "" {
		t.Fatalf("clone failed: %s", p.Error)
	}
	if _, err := os.Stat(filepath.Join(p.Path, "README")); err != nil {
		t.Fatalf("cloned files missing: %v", err)
	}

	// 2. the server's loopback is out of reach: a listener on 127.0.0.1
	// never sees a connection
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var hits atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			c.Close()
		}
	}()
	q, err := m.CreateProject(bob, CreateProjectReq{Kind: "clone", Name: "probe", URL: "https://" + ln.Addr().String() + "/x.git"})
	if err != nil {
		t.Fatal(err)
	}
	if q = wait(q.ID); q.Status != "error" {
		t.Fatalf("clone from the host's loopback succeeded: %+v", q)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the host listener received %d connection(s) from a member's clone", n)
	}
	t.Logf("loopback clone refused: %s", q.Error)
}

// An admin's clone into an isolated project runs in the admin's container
// too: it can't reach the server's loopback, a failed one leaves an empty
// folder and can be cloned again, and the admin's GH_TOKEN is used. Set
// COLLIE_INTEGRATION_PRIVATE_URL (an https:// repository GH_TOKEN can
// read) to also clone a private repository with it.
func TestIntegrationAdminCloneRunsInContainer(t *testing.T) {
	if os.Getenv("COLLIE_INTEGRATION") == "" {
		t.Skip("set COLLIE_INTEGRATION=1 (needs podman and the agent image)")
	}
	data, root := t.TempDir(), t.TempDir()
	sb := sandbox.New(sandbox.Config{Podman: "podman", DataDir: data})
	if !sb.Available() || !sb.Status().Ready {
		t.Skip("podman or the agent image is not available")
	}
	st, err := store.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: Config{ProjectRoot: root}, st: st, hub: NewHub(), sb: sb, rts: map[string]*Runtime{}, ctrActive: map[string]time.Time{}, toolLn: map[string]net.Listener{}}
	m.SetAccess(fakeAccess{})
	admin := Actor{ID: "rootintegration", Name: "root", Admin: true}
	wait := func(id string) model.Project {
		for i := 0; i < 600; i++ {
			if p, _ := st.Project(id); p.Status != model.ProjectCloning {
				return p
			}
			time.Sleep(time.Second)
		}
		t.Fatal("clone did not finish")
		return model.Project{}
	}
	t.Cleanup(func() {
		for _, p := range st.Projects() {
			sb.Remove(p.ID)
			m.closeToolSockets(p.ID)
		}
	})
	token := os.Getenv("GH_TOKEN")
	if token != "" {
		if err := m.SetUserEnv(admin.ID, "GH_TOKEN="+token+"\n"); err != nil {
			t.Fatal(err)
		}
	}

	// 1. a failed clone (no such repository) keeps the project, with an
	// empty folder, and says why without the token
	p, err := m.CreateProject(admin, CreateProjectReq{Kind: "clone", Name: "hello", URL: "https://github.com/octocat/no-such-repository-collie-test.git", Runtime: "container"})
	if err != nil {
		t.Fatal(err)
	}
	if p = wait(p.ID); p.Status != model.ProjectCloneFailed {
		t.Fatalf("a clone of a missing repository: %+v", p)
	}
	t.Logf("failed clone says: %s", p.Error)
	if token != "" && strings.Contains(p.Error, token) {
		t.Fatal("the clone's error shows GH_TOKEN")
	}
	if ents, err := os.ReadDir(p.Path); err != nil || len(ents) != 0 {
		t.Fatalf("folder after a failed clone: %v %v", ents, err)
	}
	if _, err := m.CreateSession(admin, CreateSessionReq{ProjectID: p.ID, Agent: "claude"}); !errors.Is(err, errCloneFailed) {
		t.Fatalf("session in a failed clone: %v", err)
	}

	// 2. cloned again from the right URL, in the same folder
	if _, err := m.RetryClone(admin, p.ID, "https://github.com/octocat/Hello-World.git"); err != nil {
		t.Fatal(err)
	}
	if p = wait(p.ID); p.Status != "" || !p.Git {
		t.Fatalf("clone again: %+v", p)
	}
	if _, err := os.Stat(filepath.Join(p.Path, "README")); err != nil {
		t.Fatalf("cloned files missing: %v", err)
	}

	// 3. the server's loopback is out of reach, which a clone on the server
	// would connect to
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var hits atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			c.Close()
		}
	}()
	q, err := m.CreateProject(admin, CreateProjectReq{Kind: "clone", Name: "probe", URL: "https://" + ln.Addr().String() + "/x.git", Runtime: "container"})
	if err != nil {
		t.Fatal(err)
	}
	if q = wait(q.ID); q.Status != model.ProjectCloneFailed {
		t.Fatalf("clone from the host's loopback succeeded: %+v", q)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the host listener received %d connection(s) from an admin's isolated clone", n)
	}

	// 4. a private repository, with the admin's GH_TOKEN
	if u := os.Getenv("COLLIE_INTEGRATION_PRIVATE_URL"); u != "" && token != "" {
		r, err := m.CreateProject(admin, CreateProjectReq{Kind: "clone", Name: "private", URL: u, Runtime: "container"})
		if err != nil {
			t.Fatal(err)
		}
		if r = wait(r.ID); r.Status != "" || !r.Git {
			t.Fatalf("private clone with GH_TOKEN: %+v", r)
		}
		t.Logf("cloned %s with the admin's GH_TOKEN", u)
	}
}
