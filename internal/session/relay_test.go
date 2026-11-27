package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
)

// The relay and the bridge run here with python3, as they would in a
// container: the bridge's connections reach Collie's tools through the
// relay's stdio.
func TestToolRelay(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("needs python3")
	}
	m, _ := newTestManager(t)
	m.relays = map[string]*toolRelay{}
	p1 := model.Project{ID: "p1", OwnerUser: "alice", Runtime: "container"}
	m.st.PutProject(p1)
	m.st.PutSession(model.Session{ID: "s1", ProjectID: "p1", Owner: "alice"})
	dir := t.TempDir()
	sock := filepath.Join(dir, toolSockName)
	old := relayCommand
	relayCommand = func(_ *Manager, _, _ string, cmd []string) ([]string, func(string)) {
		cmd = slices.Clone(cmd)
		i := slices.Index(cmd, relaySock)
		cmd[i] = sock
		return cmd, func(string) {}
	}
	t.Cleanup(func() { relayCommand = old; m.stopRelays("p1") })

	sc := toolScope{uid: "alice", project: "p1"}
	if err := m.ensureRelay(p1, "alice"); err != nil {
		t.Fatal(err)
	}
	first := m.relays[sc.key()]
	if err := m.ensureRelay(p1, "alice"); err != nil || m.relays[sc.key()] != first {
		t.Fatalf("a running relay was replaced (%v)", err)
	}

	// Two agents at once; one sends a request bigger than a chunk.
	big := strings.Repeat("x", 300<<10)
	done := make(chan error, 2)
	for _, id := range []string{`1`, `"` + big + `"`} {
		go func() { done <- callThroughBridge(dir, sock, id) }()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}

	// A relay that died (its container was re-created, say) is replaced.
	_ = first.cmd.Process.Kill()
	<-first.done
	if err := m.ensureRelay(p1, "alice"); err != nil {
		t.Fatal(err)
	}
	if m.relays[sc.key()] == first {
		t.Fatal("dead relay kept")
	}
	if err := callThroughBridge(dir, sock, `7`); err != nil {
		t.Fatal(err)
	}

	m.stopRelays("p1")
	select {
	case <-first.done:
	case <-time.After(10 * time.Second):
		t.Fatal("relay still running after stop")
	}
}

// callThroughBridge asks Collie's tools for their list through the bridge,
// using the given JSON-RPC id, and checks the answer.
func callThroughBridge(dir, sock, id string) error {
	cmd := exec.Command("python3", filepath.Join(dir, "bridge.py"), sock, "s1")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Fprintf(stdin, `{"jsonrpc":"2.0","id":%s,"method":"initialize","params":{}}`+"\n", id)
	fmt.Fprintf(stdin, `{"jsonrpc":"2.0","id":%s,"method":"tools/list"}`+"\n", id)
	r := bufio.NewReaderSize(stdout, 1<<20)
	var got []string
	for range 2 {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return fmt.Errorf("reading the answer: %v", err)
		}
		var resp struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				ServerInfo struct{ Name string }   `json:"serverInfo"`
				Tools      []struct{ Name string } `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal(line, &resp); err != nil {
			return err
		}
		if string(resp.ID) != id {
			return fmt.Errorf("answer for id %.20s, want %.20s", resp.ID, id)
		}
		if n := resp.Result.ServerInfo.Name; n != "" {
			got = append(got, n)
		}
		for _, t := range resp.Result.Tools {
			got = append(got, t.Name)
		}
	}
	if !slices.Equal(got, []string{"collie", "show_media"}) {
		return fmt.Errorf("answers: %v", got)
	}
	// Collie finishes once the agent closes its end; so does the bridge.
	stdin.Close()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		return err
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		return fmt.Errorf("bridge did not exit")
	}
}

// On an allowlist, the relay also offers the proxy in the container: its
// connections reach the proxy's socket on the host.
func TestToolRelayProxy(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("needs python3")
	}
	m, _ := newTestManager(t)
	m.relays = map[string]*toolRelay{}
	m.sb = sandbox.New(sandbox.Config{Podman: "/nonexistent/podman", DataDir: t.TempDir()})
	p := model.Project{ID: "p1", OwnerUser: "alice", Runtime: "container", Network: model.NetAllowlist, NetAllow: []string{"example.com"}}
	m.st.PutProject(p)

	// The host side: an echo server where the proxy would listen.
	ps := m.sb.ProxySocket(p.ID, "alice")
	if err := os.MkdirAll(filepath.Dir(ps), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", ps)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()

	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := free.Addr().String()
	free.Close()
	dir := t.TempDir()
	old := relayCommand
	relayCommand = func(_ *Manager, _, _ string, cmd []string) ([]string, func(string)) {
		cmd = slices.Clone(cmd)
		cmd[slices.Index(cmd, relaySock)] = filepath.Join(dir, toolSockName)
		cmd[slices.Index(cmd, sandbox.ProxyAddr)] = addr
		return cmd, func(string) {}
	}
	t.Cleanup(func() { relayCommand = old; m.stopRelays(p.ID) })
	if err := m.ensureRelay(p, "alice"); err != nil {
		t.Fatal(err)
	}

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msg := strings.Repeat("CONNECT example.com:443 HTTP/1.1\r\n", 5000) // several chunks
	go func() {
		_, _ = io.WriteString(c, msg)
		_ = c.(*net.TCPConn).CloseWrite()
	}()
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	got, err := io.ReadAll(c) // ends when the host side closes, after the echo
	if err != nil || string(got) != msg {
		t.Fatalf("through the relay: %d of %d bytes, %v", len(got), len(msg), err)
	}
}
