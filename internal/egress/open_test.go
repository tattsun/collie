package egress

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// echoServer answers every line it gets with the same line.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

// openTunnel opens a CONNECT tunnel through the proxy's socket.
func openTunnel(t *testing.T, sock, host string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", host, host)
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s: %v %v", host, res, err)
	}
	return c, br
}

// alive reports whether a tunnel still carries data.
func alive(c net.Conn, br *bufio.Reader) bool {
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprint(c, "ping\n"); err != nil {
		return false
	}
	line, err := br.ReadString('\n')
	return err == nil && strings.TrimSpace(line) == "ping"
}

// Open tunnels last only while the list allows them: an edit ends them at
// once (Recheck), and the periodic check catches what no edit reported.
func TestTunnelsEndWhenTheListNoLongerAllows(t *testing.T) {
	port := echoServer(t)
	p := NewProxy()
	p.anyAddr = true
	var mu sync.Mutex
	allow := []string{"localhost", "127.0.0.1"}
	p.Rules = func(string) ([]string, bool) { mu.Lock(); defer mu.Unlock(); return allow, true }
	_, sock := testProxy(t, p, "p1")
	set := func(l ...string) { mu.Lock(); allow = l; mu.Unlock() }

	byName, nbr := openTunnel(t, sock, "localhost:"+port)
	byAddr, abr := openTunnel(t, sock, "127.0.0.1:"+port)
	if !alive(byName, nbr) || !alive(byAddr, abr) {
		t.Fatal("tunnels don't work")
	}
	// The owner removes the name: its tunnel ends, the other one stays.
	set("127.0.0.1")
	p.Recheck("p1")
	if alive(byName, nbr) {
		t.Error("tunnel to a removed host still open")
	}
	if !alive(byAddr, abr) {
		t.Error("tunnel to a host still on the list ended")
	}
	// Recheck for another project leaves this one's alone.
	set()
	p.Recheck("p2")
	if !alive(byAddr, abr) {
		t.Error("another project's edit ended this project's tunnel")
	}
	// The periodic check ends it without an edit being reported.
	deadline := time.Now().Add(3 * recheckEvery)
	for alive(byAddr, abr) {
		if time.Now().After(deadline) {
			t.Fatal("the periodic check left a tunnel open that the list no longer allows")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Open connections are checked as new ones are: the address they went to
// counts, not just the name.
func TestOpenConnectionsCheckTheAddress(t *testing.T) {
	p := NewProxy()
	p.Rules = func(string) ([]string, bool) { return []string{"nas.example", "192.168.1.20", "example.com"}, true }
	check := func(host, addr string) bool {
		c := &conn{projectID: "p1", t: newTarget(host, 443)}
		p.setAddr(c, &net.TCPAddr{IP: net.ParseIP(addr), Port: 443})
		return p.stillAllowed(c)
	}
	if !check("192.168.1.20", "192.168.1.20") {
		t.Error("a private address listed by IP")
	}
	if check("nas.example", "192.168.1.20") {
		t.Error("a private address reached by name")
	}
	if check("example.com", "127.0.0.1") {
		t.Error("the loopback")
	}
	if !check("example.com", "93.184.215.14") {
		t.Error("a listed host's public address")
	}
}

// Closing a container's proxy (its project left the allowlist, the
// container stopped) ends its tunnels and forwarded requests too.
func TestCloseEndsOpenConnections(t *testing.T) {
	port := echoServer(t)
	p := NewProxy()
	p.anyAddr = true
	p.Rules = func(string) ([]string, bool) { return []string{"localhost"}, true }
	_, sock := testProxy(t, p, "p1")
	c, br := openTunnel(t, sock, "localhost:"+port)
	if !alive(c, br) {
		t.Fatal("tunnel doesn't work")
	}
	p.Close("p1", "")
	if alive(c, br) {
		t.Error("tunnel open after Close")
	}
}

// A forwarded plain-HTTP request (a stream, a WebSocket) ends too.
func TestForwardedStreamsEndWithTheList(t *testing.T) {
	started := make(chan struct{})
	backend := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go backend.Serve(ln)
	defer backend.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	p := NewProxy()
	p.anyAddr = true
	var mu sync.Mutex
	allow := []string{"localhost"}
	p.Rules = func(string) ([]string, bool) { mu.Lock(); defer mu.Unlock(); return allow, true }
	client, _ := testProxy(t, p, "p1")
	client.Timeout = 0
	res, err := client.Get("http://localhost:" + port + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	<-started
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, res.Body); done <- err }()
	mu.Lock()
	allow = nil
	mu.Unlock()
	p.Recheck("p1")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a forwarded stream outlived the host's removal")
	}
}
