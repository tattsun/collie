package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/model"
)

func newHomeTestManager(t *testing.T) *Manager {
	t.Helper()
	m, _ := newTestManager(t)
	m.toolLn = map[string]net.Listener{}
	return m
}

// rpc sends one JSON-RPC request through handleRPC, on the user's Home
// socket, and decodes the answer.
func rpc(t *testing.T, m *Manager, uid, method string, params any) map[string]any {
	t.Helper()
	return rpcOn(t, m, &toolConn{scope: toolScope{uid: uid}}, method, params)
}

func rpcOn(t *testing.T, m *Manager, tc *toolConn, method string, params any) map[string]any {
	t.Helper()
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp := m.handleRPC(tc, line)
	b, _ := json.Marshal(resp)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// call runs a tool and returns its text and whether it failed.
func call(t *testing.T, m *Manager, uid, tool string, args any) (string, bool) {
	t.Helper()
	return callOn(t, m, &toolConn{scope: toolScope{uid: uid}}, tool, args)
}

func callOn(t *testing.T, m *Manager, tc *toolConn, tool string, args any) (string, bool) {
	t.Helper()
	out := rpcOn(t, m, tc, "tools/call", map[string]any{"name": tool, "arguments": args})
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no result: %v", tool, out)
	}
	content := res["content"].([]any)[0].(map[string]any)
	isErr, _ := res["isError"].(bool)
	return content["text"].(string), isErr
}

func TestHomeIsPerUserAndFixed(t *testing.T) {
	m := newHomeTestManager(t)
	// Without podman a Home would run on the host: admins only.
	if _, err := m.EnsureHome(Actor{ID: "alice"}); !errors.Is(err, errHomeUnavailable) {
		t.Fatalf("member without podman: %v", err)
	}
	root := Actor{ID: "root", Admin: true}
	h, err := m.EnsureHome(root)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := m.EnsureHome(root)
	if again.ID != h.ID || !h.Home || h.OwnerUser != "root" {
		t.Fatalf("EnsureHome is not idempotent: %+v vs %+v", h, again)
	}
	if fi, err := os.Stat(h.Path); err != nil || !fi.IsDir() {
		t.Fatalf("workspace not created: %v", err)
	}
	if m.ProjectVisible("alice", h) {
		t.Error("someone else's Home is visible")
	}
	if err := m.DeleteProject(root, h.ID); !errors.Is(err, errHomeFixed) {
		t.Errorf("deleting Home: %v", err)
	}
	name := "x"
	if _, err := m.PatchProject(root, h.ID, PatchProjectReq{Name: &name}); !errors.Is(err, errHomeFixed) {
		t.Errorf("renaming Home: %v", err)
	}
}

func TestHomeToolsSeeOnlyTheUsersWork(t *testing.T) {
	m := newHomeTestManager(t)
	m.st.PutProject(model.Project{ID: "pa", Name: "alpha", Path: "/p/alpha", OwnerUser: "alice", Runtime: "container"})
	m.st.PutProject(model.Project{ID: "pt", Name: "teamproj", Path: "/p/team", OwnerGroup: "team", Runtime: "container"})
	m.st.PutProject(model.Project{ID: "pb", Name: "bobs", Path: "/p/bob", OwnerUser: "bob", Runtime: "container"})
	m.st.PutProject(model.Project{ID: "ha", Name: "Home", Path: "/h/alice", OwnerUser: "alice", Home: true, Runtime: "container"})
	m.st.PutSession(model.Session{ID: "a1", ProjectID: "pa", Owner: "alice", Title: "fix login", Status: model.StatusIdle, UpdatedAt: model.NowMs()})
	m.st.PutSession(model.Session{ID: "t1", ProjectID: "pt", Owner: "bob", Title: "bob private", Status: model.StatusIdle})
	m.st.PutSession(model.Session{ID: "h1", ProjectID: "ha", Owner: "alice", Title: "chat"})

	init := rpc(t, m, "alice", "initialize", map[string]any{"protocolVersion": "2025-03-26"})
	if v := init["result"].(map[string]any)["protocolVersion"]; v != "2025-03-26" {
		t.Errorf("protocol version = %v", v)
	}
	tools := rpc(t, m, "alice", "tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, x := range tools {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	for _, want := range []string{"list_projects", "list_sessions", "read_session", "start_session", "send_message", "show_media"} {
		if !slices.Contains(names, want) {
			t.Errorf("tool %s missing: %v", want, names)
		}
	}
	if out := rpc(t, m, "alice", "nope", nil); out["error"] == nil {
		t.Error("unknown method accepted")
	}
	if m.handleRPC(&toolConn{scope: toolScope{uid: "alice"}}, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)) != nil {
		t.Error("answered a notification")
	}

	text, isErr := call(t, m, "alice", "list_projects", nil)
	if isErr || !strings.Contains(text, "alpha") || !strings.Contains(text, "teamproj") {
		t.Errorf("list_projects: %s", text)
	}
	if strings.Contains(text, "bobs") || strings.Contains(text, `"Home"`) {
		t.Errorf("list_projects shows bob's project or a Home: %s", text)
	}
	text, _ = call(t, m, "alice", "list_sessions", nil)
	if !strings.Contains(text, "fix login") || strings.Contains(text, "bob private") || !strings.Contains(text, `"chat"`) {
		t.Errorf("list_sessions: %s", text)
	}
	if text, isErr = call(t, m, "alice", "list_sessions", map[string]any{"project": "nosuch"}); !isErr {
		t.Errorf("unknown project accepted: %s", text)
	}
	if text, isErr = call(t, m, "alice", "read_session", map[string]any{"session": "t1"}); !isErr {
		t.Errorf("read another member's private session: %s", text)
	}
	if text, isErr = call(t, m, "alice", "send_message", map[string]any{"session": "h1", "text": "hi"}); !isErr {
		t.Errorf("Home messaged a Home conversation: %s", text)
	}
	if text, isErr = call(t, m, "alice", "start_session", map[string]any{"project": "alpha", "prompt": "go", "mode": "bypassPermissions"}); !isErr {
		t.Errorf("Home started a session without permission checks: %s", text)
	}
	if text, isErr = call(t, m, "alice", "start_session", map[string]any{"project": "Home", "prompt": "go"}); !isErr {
		t.Errorf("Home started a session in a Home: %s", text)
	}
}

func TestHomeMountsLeaveCollieDataOut(t *testing.T) {
	m := newHomeTestManager(t)
	work := t.TempDir()
	m.st.PutProject(model.Project{ID: "p1", Path: work, OwnerUser: "root"})
	m.st.PutProject(model.Project{ID: "p2", Path: filepath.Dir(m.st.Dir()), OwnerUser: "root"}) // contains the data dir
	m.st.PutProject(model.Project{ID: "p3", Path: filepath.Join(work, "missing"), OwnerUser: "root"})
	got := m.homeMounts("root")
	if !slices.Equal(got, []string{work}) {
		t.Errorf("homeMounts = %v, want [%s]", got, work)
	}
}

func TestCollieToolArgs(t *testing.T) {
	allowed := append(slices.Clone(homeReadTools), mediaTools...)
	args := collieClaudeArgs([]string{"--mcp-config", "/profile/mcp.json", "--settings", "/s.json"}, "/bin/collie", []string{"mcp-bridge", "/run/collie/collie.sock", "s1"}, allowed)
	if args[0] != "--mcp-config" || args[1] != "/profile/mcp.json" || !strings.Contains(args[2], `"collie"`) || args[3] != "--settings" {
		t.Errorf("tools config not added to the existing --mcp-config: %q", args)
	}
	if i := slices.Index(args, "--allowedTools"); i < 0 || !slices.Contains(args[i:], "mcp__collie__read_session") ||
		!slices.Contains(args[i:], "mcp__collie__show_media") || slices.Contains(args[i:], "mcp__collie__start_session") {
		t.Errorf("allowed tools: %q", args)
	}
	if args := collieClaudeArgs(nil, "/bin/collie", nil, nil); slices.Contains(args, "--allowedTools") {
		t.Errorf("an empty --allowedTools: %q", args)
	}
	codex := collieCodexArgs(nil, "/bin/collie", []string{"mcp-bridge", "/s"})
	if !slices.Equal(codex, []string{"-c", `mcp_servers.collie.command="/bin/collie"`, "-c", `mcp_servers.collie.args=["mcp-bridge","/s"]`}) {
		t.Errorf("codex args: %q", codex)
	}
}

func TestHomeSocketServesMCP(t *testing.T) {
	m := newHomeTestManager(t)
	dir, err := m.homeSocket("alice")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := m.homeSocket("alice"); again != dir {
		t.Fatal("a second socket for the same user")
	}
	c, err := net.Dial("unix", filepath.Join(dir, toolSockName))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(`{"jsonrpc":"2.0","id":7,"method":"ping"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, `"id":7`) || !strings.Contains(line, `"result"`) {
		t.Errorf("ping answer: %s", line)
	}
	if fi, err := os.Stat(filepath.Join(dir, toolSockName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket permissions: %v %v", fi.Mode(), err)
	}
}
