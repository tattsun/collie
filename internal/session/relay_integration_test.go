package session

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
	"github.com/tattsun/collie/internal/store"
)

// testPodman is the podman the integration tests use. Point
// COLLIE_TEST_PODMAN at a remote client to take the Podman machine path
// on Linux, e.g. a script running
//
//	exec podman --remote --url unix:///tmp/x/podman.sock "$@"
//
// next to `podman system service --time=0 unix:///tmp/x/podman.sock`.
func testPodman() string {
	if p := os.Getenv("COLLIE_TEST_PODMAN"); p != "" {
		return p
	}
	return "podman"
}

// Runs a real container: COLLIE_INTEGRATION=1 COLLIE_TEST_PODMAN=<remote
// client> go test ./internal/session -run Integration
// remoteTestManager is a Manager on a remote Podman with the agent image,
// or skips the test.
func remoteTestManager(t *testing.T) *Manager {
	t.Helper()
	if os.Getenv("COLLIE_INTEGRATION") == "" {
		t.Skip("set COLLIE_INTEGRATION=1 (needs podman and the agent image)")
	}
	data := t.TempDir()
	sb := sandbox.New(sandbox.Config{Podman: testPodman(), DataDir: data})
	// (Telling remote from local asks the service, which can be stuck
	// for a while behind another podman's lock on a busy machine.)
	for i := 0; i < 3 && sb.Available() && !sb.Remote() && testPodman() != "podman"; i++ {
		sb = sandbox.New(sandbox.Config{Podman: testPodman(), DataDir: data})
	}
	if !sb.Available() || !sb.Remote() {
		t.Skip("needs a remote Podman (see testPodman)")
	}
	for i := 0; !sb.Status().Ready; i++ {
		if i == 120 {
			t.Skip("the agent image is not available")
		}
		time.Sleep(time.Second)
	}
	st, err := store.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{st: st, hub: NewHub(), sb: sb, rts: map[string]*Runtime{}, ctrActive: map[string]time.Time{},
		toolLn: map[string]net.Listener{}, relays: map[string]*toolRelay{}}
	m.SetAccess(fakeAccess{})
	sb.SetNetRules(st.Project)
	return m
}

// putTestProject stores an isolated project of alice's with a session,
// removed (with its containers) after the test.
func putTestProject(t *testing.T, m *Manager, name string, edit func(*model.Project)) (model.Project, model.Session) {
	id := name + strconv.Itoa(os.Getpid())
	p := model.Project{ID: id, Name: name, Path: t.TempDir(), Runtime: "container", OwnerUser: "alice"}
	if edit != nil {
		edit(&p)
	}
	s := model.Session{ID: id + "s", ProjectID: p.ID, Owner: "alice", Cwd: p.Path}
	m.st.PutProject(p)
	m.st.PutSession(s)
	t.Cleanup(func() {
		m.stopRelays(p.ID)
		m.sb.Remove(p.ID)
	})
	return p, s
}

func TestIntegrationRemoteTools(t *testing.T) {
	m := remoteTestManager(t)
	sb := m.sb
	p, s := putTestProject(t, m, "relayint", nil)
	if err := os.WriteFile(p.Path+"/hello.txt", []byte("hi from the host"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.ensureContainer(p, "alice", nil); err != nil {
		t.Fatal(err)
	}
	// The agent's MCP server, as the agent would start it in the container.
	cmd, args, err := m.collieTools(p, s)
	if err != nil {
		t.Fatal(err)
	}
	if cmd != "python3" {
		t.Fatalf("remote tools command: %s %q", cmd, args)
	}
	argv, _ := sb.Exec(p.ID, "alice", p.Path, p.ID+"-bridge", false, nil, append([]string{cmd}, args...))
	bridge := exec.Command(argv[0], argv[1:]...)
	stdin, _ := bridge.StdinPipe()
	stdout, _ := bridge.StdoutPipe()
	var stderr strings.Builder
	bridge.Stderr = &stderr
	if err := bridge.Start(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	fmt.Fprintln(stdin, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	r := bufio.NewReader(stdout)
	for _, want := range []string{`"serverInfo":{"name":"collie"`, `"name":"show_media"`} {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("no answer through the relay: %v (bridge: %s)", err, stderr.String())
		}
		if !strings.Contains(line, want) {
			t.Fatalf("answer %.200s lacks %s", line, want)
		}
	}
	stdin.Close()
	exited := make(chan error, 1)
	go func() { exited <- bridge.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("bridge: %v (%s)", err, stderr.String())
		}
	case <-time.After(20 * time.Second):
		_ = bridge.Process.Kill()
		t.Fatal("bridge did not exit")
	}

	// Files are read the way the agent sees them.
	b, err := sb.ReadFile(p.ID, "alice", p.Path+"/hello.txt", 1<<20)
	if err != nil || string(b) != "hi from the host" {
		t.Fatalf("ReadFile: %q %v", b, err)
	}
}

// On an allowlist, the container's only way out is the proxy, which the
// relay offers in the container.
func TestIntegrationRemoteAllowlist(t *testing.T) {
	m := remoteTestManager(t)
	p, _ := putTestProject(t, m, "relaynet", func(p *model.Project) {
		p.Network, p.NetAllow = model.NetAllowlist, []string{"github.com"}
	})
	if err := m.ensureContainer(p, "alice", nil); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		argv, _ := m.sb.Exec(p.ID, "alice", p.Path, "relaynet-"+model.NewID(), false, nil, args)
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := run("curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "20", "https://github.com/"); err != nil || (out != "200" && out != "301") {
		t.Fatalf("allowed host through the proxy: %q %v", out, err)
	}
	if out, err := run("curl", "-sS", "-o", "/dev/null", "--max-time", "20", "https://example.com/"); err == nil {
		t.Fatalf("a host off the allowlist was reached: %q", out)
	} else {
		t.Logf("off the allowlist: %s", out)
	}
	if out, err := run("curl", "-sS", "-o", "/dev/null", "--noproxy", "*", "--max-time", "10", "https://github.com/"); err == nil {
		t.Fatalf("reached the internet around the proxy: %q", out)
	} else {
		t.Logf("around the proxy: %s", out)
	}
	if log := m.sb.NetworkLog(p.ID); len(log) == 0 {
		t.Error("the proxy logged nothing")
	}
}
