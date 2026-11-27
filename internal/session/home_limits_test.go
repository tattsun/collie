package session

import (
	"bufio"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/model"
)

// An admin's Home runs in a container and reads what other agents wrote:
// it can't make agents run on the server, nor pick a mode that never asks.
func TestHomeInAContainerCantDriveHostRunAgents(t *testing.T) {
	m := newHomeTestManager(t)
	m.SetAccess(allAdmins{})
	m.cfg.ClaudeBin = fakeAgent(t, "claude", filepath.Join(t.TempDir(), "started"))
	m.st.PutProject(model.Project{ID: "ha", Name: "Home", Path: t.TempDir(), OwnerUser: "alice", Home: true, Runtime: "container"})
	m.st.PutProject(model.Project{ID: "ph", Name: "server", Path: t.TempDir(), OwnerUser: "alice"})
	m.st.PutProject(model.Project{ID: "pc", Name: "boxed", Path: t.TempDir(), OwnerUser: "alice", Runtime: "container"})
	m.st.PutSession(model.Session{ID: "s1", ProjectID: "ph", Owner: "alice", Agent: "claude", Mode: "bypassPermissions", Title: "on the host", Status: model.StatusIdle})

	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"start_session", map[string]any{"project": "server", "prompt": "go"}},
		{"send_message", map[string]any{"session": "s1", "text": "run this"}},
		{"create_schedule", map[string]any{"project": "server", "prompt": "go", "cron": "0 9 * * *"}},
	} {
		if text, isErr := call(t, m, "alice", c.tool, c.args); !isErr || !strings.Contains(text, "Home can't") {
			t.Errorf("%s on a host-run project: %s", c.tool, text)
		}
	}
	for _, mode := range []string{" bypassPermissions", "dontAsk\n", "\tfull"} {
		if text, isErr := call(t, m, "alice", "create_schedule", map[string]any{"project": "boxed", "prompt": "go", "cron": "0 9 * * *", "mode": mode}); !isErr || !strings.Contains(text, "without permission checks") {
			t.Errorf("schedule in mode %q: %s", mode, text)
		}
		if text, isErr := call(t, m, "alice", "start_session", map[string]any{"project": "boxed", "prompt": "go", "mode": mode}); !isErr {
			t.Errorf("session in mode %q: %s", mode, text)
		}
	}
	if len(m.st.Schedules()) != 0 {
		t.Errorf("schedules made: %+v", m.st.Schedules())
	}
}

// One agent can't exhaust Collie through its tools socket: an endless line
// ends its connection, and only so many connections are served at once.
func TestToolsSocketLimits(t *testing.T) {
	m := newHomeTestManager(t)
	m.st.PutProject(model.Project{ID: "pc", Name: "boxed", Path: t.TempDir(), OwnerUser: "alice", Runtime: "container"})
	dir, err := m.toolSocket(toolScope{uid: "alice", project: "pc"})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, toolSockName)
	dial := func() net.Conn {
		t.Helper()
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	ping := func(c net.Conn) bool {
		c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")); err != nil {
			return false
		}
		_, err := bufio.NewReader(c).ReadString('\n')
		return err == nil
	}

	long := dial()
	chunk := []byte(strings.Repeat("x", 1<<20))
	long.SetDeadline(time.Now().Add(10 * time.Second))
	var werr error
	for i := 0; i < maxToolMessage>>20+2 && werr == nil; i++ {
		_, werr = long.Write(chunk)
	}
	if werr == nil && ping(long) {
		t.Error("a message longer than the limit is still being read")
	}

	time.Sleep(100 * time.Millisecond) // the long connection's slot is free again
	var held []net.Conn
	for range maxToolConns {
		c := dial()
		if !ping(c) {
			t.Fatal("a connection within the limit isn't served")
		}
		held = append(held, c)
	}
	if ping(dial()) {
		t.Error("served more connections than the limit")
	}
	held[0].Close()
	time.Sleep(100 * time.Millisecond)
	if !ping(dial()) {
		t.Error("a freed slot isn't reused")
	}
}

func TestDisabledUsersLoseTheirTools(t *testing.T) {
	m := newHomeTestManager(t)
	m.UserActive = func(uid string) bool { return uid != "alice" }
	if text, isErr := call(t, m, "alice", "list_projects", nil); !isErr || !strings.Contains(text, "no longer") {
		t.Errorf("a disabled user's tool call: %s", text)
	}
	if _, isErr := call(t, m, "bob", "list_projects", nil); isErr {
		t.Error("an active user's tool call failed")
	}
}
