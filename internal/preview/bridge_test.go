package preview

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pipeBridge runs the bridge's server side in this process, over pipes,
// as `podman exec … collie port-bridge` would in a container.
func pipeBridge(t *testing.T, f Filter) (*Bridge, *atomic.Int32) {
	t.Helper()
	var opens atomic.Int32
	b := &Bridge{}
	b.open = func() (net.Conn, error) {
		opens.Add(1)
		aR, aW, err := os.Pipe() // Collie -> bridge
		if err != nil {
			return nil, err
		}
		bR, bW, err := os.Pipe() // bridge -> Collie
		if err != nil {
			return nil, err
		}
		go func() { _ = Serve(&pipeConn{r: aR, w: bW}, f) }()
		return &pipeConn{r: bR, w: aW}, nil
	}
	b.init()
	t.Cleanup(b.Close)
	return b, &opens
}

// echoServer answers HTTP on a loopback port, and echoes lines after an
// "UPGRADE" request (a stand-in for a WebSocket).
func echoServer(t *testing.T) int {
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
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					fmt.Fprintf(c, "echo %s", line)
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestBridgeDialsListeningPorts(t *testing.T) {
	port := echoServer(t)
	b, opens := pipeBridge(t, Filter{UID: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ports, err := b.Ports(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range ports {
		found = found || p.Port == port
	}
	if !found {
		t.Fatalf("port %d not listed: %+v", port, ports)
	}

	// Several connections at once, both ways, over one bridge process.
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := b.Dial(ctx, port)
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			br := bufio.NewReader(c)
			for j := range 3 {
				fmt.Fprintf(c, "hello %d.%d\n", i, j)
				got, err := br.ReadString('\n')
				if want := fmt.Sprintf("echo hello %d.%d\n", i, j); err != nil || got != want {
					t.Errorf("got %q, %v; want %q", got, err, want)
					return
				}
			}
		}()
	}
	wg.Wait()
	if n := opens.Load(); n != 1 {
		t.Errorf("started %d bridge processes, want 1", n)
	}
}

func TestBridgeRefusesWhatDoesNotListen(t *testing.T) {
	b, _ := pipeBridge(t, Filter{UID: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A free port: nothing listens there.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	free := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if _, err := b.Dial(ctx, free); !errors.Is(err, ErrNotListening) {
		t.Errorf("dial a free port: %v, want ErrNotListening", err)
	}
	// Skipped ports can't be reached even though they listen.
	port := echoServer(t)
	b2, _ := pipeBridge(t, Filter{UID: -1, SkipPorts: []int{port}})
	if _, err := b2.Dial(ctx, port); !errors.Is(err, ErrNotListening) {
		t.Errorf("dial a skipped port: %v", err)
	}
	ports, _ := b2.Ports(ctx)
	for _, p := range ports {
		if p.Port == port {
			t.Errorf("skipped port %d listed", port)
		}
	}
}

// HTTP through the bridge, as the preview proxy uses it.
func TestBridgeCarriesHTTP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s %s %s", r.Method, r.URL.Path, body)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	b, _ := pipeBridge(t, Filter{UID: -1})
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return b.Dial(ctx, port) }}
	defer tr.CloseIdleConnections()
	cl := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	for i := range 3 {
		resp, err := cl.Post(fmt.Sprintf("http://localhost:%d/x", port), "text/plain", strings.NewReader(fmt.Sprint("body", i)))
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if want := fmt.Sprintf("POST /x body%d", i); string(got) != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
