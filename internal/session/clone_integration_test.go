package session

import (
	"net"
	"os"
	"path/filepath"
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
	sb := sandbox.New(sandbox.Config{Podman: testPodman(), DataDir: data})
	for i := 0; !sb.Status().Ready && sb.Remote() && i < 120; i++ {
		time.Sleep(time.Second) // a remote Podman connects in the background
	}
	if !sb.Available() || !sb.Status().Ready {
		t.Skip("podman or the agent image is not available")
	}
	st, err := store.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: Config{ProjectRoot: root}, st: st, hub: NewHub(), sb: sb, rts: map[string]*Runtime{}, ctrActive: map[string]time.Time{}}
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
