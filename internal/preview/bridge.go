package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The bridge reaches into a container. `collie port-bridge`, started in
// the container with `podman exec`, speaks HTTP/2 (without TLS) on its
// stdin and stdout: one process per container carries every connection
// Collie makes to the servers in it, each on its own stream, and says
// which ports are listening. It works the same whatever network the
// container has (none at all on an allowlist): it listens on nothing,
// only Collie can talk to it, and it connects only to servers listening
// in its own container (see dialAddr), so it opens no way in or out of
// the container for anyone else.

// ServeStdio is `collie port-bridge`: it serves Collie on stdin/stdout
// until Collie hangs up.
func ServeStdio(f Filter) error {
	for _, fd := range []int{0, 1} {
		// Pollable, so the HTTP/2 server's deadlines work on them.
		_ = syscall.SetNonblock(fd, true)
	}
	return Serve(&pipeConn{r: os.NewFile(0, "stdin"), w: os.NewFile(1, "stdout")}, f)
}

// Serve is the bridge on one connection.
func Serve(c net.Conn, f Filter) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ports", func(w http.ResponseWriter, r *http.Request) {
		ports, err := Listening(f)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ports)
	})
	mux.HandleFunc("POST /dial/{port}", func(w http.ResponseWriter, r *http.Request) {
		port, err := strconv.Atoi(r.PathValue("port"))
		if err != nil || !ValidPort(port) {
			http.Error(w, "bad port", http.StatusBadRequest)
			return
		}
		addr, err := DialAddr(port, f)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		up, err := net.DialTimeout("tcp", addr, 10*time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		pipe(w, r, up)
	})
	return serveOne(c, mux)
}

// pipe copies a stream's request body to the connection and the
// connection to the response, until either side is done.
func pipe(w http.ResponseWriter, r *http.Request, up net.Conn) {
	defer up.Close()
	rc := http.NewResponseController(w)
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = up.Close()
	}()
	go func() {
		_, _ = io.Copy(up, r.Body)
		if tc, ok := up.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := up.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil || rc.Flush() != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// connWindow is how much may wait unread on one bridge connection: every
// connection to a container's servers shares it, so one server that
// doesn't read what it is sent must not hold up the others (each stream
// still has HTTP/2's own, smaller window).
const connWindow = 64 << 20

// serveOne serves HTTP/2 without TLS on a single connection.
func serveOne(c net.Conn, h http.Handler) error {
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: h, Protocols: &p, ErrorLog: log.New(os.Stderr, "collie port-bridge: ", 0),
		HTTP2: &http.HTTP2Config{MaxReceiveBufferPerConnection: connWindow}}
	ln := &oneConn{c: c, done: make(chan struct{})}
	err := srv.Serve(ln)
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// oneConn is a listener that hands out one connection and closes once
// that is closed.
type oneConn struct {
	c    net.Conn
	once sync.Once
	took bool
	done chan struct{}
}

func (l *oneConn) Accept() (net.Conn, error) {
	if !l.took {
		l.took = true
		return &closeNotify{Conn: l.c, l: l}, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConn) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *oneConn) Addr() net.Addr { return pipeAddr{} }

type closeNotify struct {
	net.Conn
	l *oneConn
}

func (c *closeNotify) Close() error {
	err := c.Conn.Close()
	_ = c.l.Close()
	return err
}

// pipeConn is a net.Conn over a pair of pipes (a process's stdio).
type pipeConn struct {
	r, w *os.File
	once sync.Once
	// The process at the other end (Collie's side only) and when it is gone.
	proc   *os.Process
	exited chan struct{}
}

func (c *pipeConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *pipeConn) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c *pipeConn) Close() error {
	c.once.Do(func() {
		_ = c.w.Close()
		_ = c.r.Close()
		if c.proc != nil {
			// The bridge exits when its stdin closes; don't wait for long.
			select {
			case <-c.exited:
			case <-time.After(5 * time.Second):
				_ = c.proc.Kill()
			}
		}
	})
	return nil
}
func (c *pipeConn) LocalAddr() net.Addr  { return pipeAddr{} }
func (c *pipeConn) RemoteAddr() net.Addr { return pipeAddr{} }
func (c *pipeConn) SetDeadline(t time.Time) error {
	_ = c.r.SetReadDeadline(t)
	_ = c.w.SetWriteDeadline(t)
	return nil
}
func (c *pipeConn) SetReadDeadline(t time.Time) error {
	_ = c.r.SetReadDeadline(t)
	return nil
}
func (c *pipeConn) SetWriteDeadline(t time.Time) error {
	_ = c.w.SetWriteDeadline(t)
	return nil
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "bridge" }

// tailBuffer keeps the end of what a process wrote to stderr.
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 2048 {
		t.b = t.b[len(t.b)-2048:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(bytes.ToValidUTF8(t.b, nil)))
}

// ---- Collie's side ----

// Bridge is Collie's end of one container's bridge. The bridge process
// is started on first use and stops after a while without connections.
type Bridge struct {
	open func() (net.Conn, error)
	tr   *http.Transport

	mu      sync.Mutex
	lastErr string // what the last bridge process said before it went away
}

// NewBridge returns a bridge that runs argv (`podman exec -i … collie
// port-bridge`) to reach the container.
func NewBridge(argv []string) *Bridge {
	b := &Bridge{}
	b.open = func() (net.Conn, error) { return b.start(argv) }
	return b.init()
}

func (b *Bridge) init() *Bridge {
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	b.tr = &http.Transport{
		Protocols:       &p,
		DialContext:     func(context.Context, string, string) (net.Conn, error) { return b.open() },
		IdleConnTimeout: 3 * time.Minute,
		HTTP2: &http.HTTP2Config{
			// Notice a bridge that went away (its container stopped).
			SendPingTimeout: 30 * time.Second,
			PingTimeout:     15 * time.Second,
			// (The receive window keeps Go's default, which is larger than
			// connWindow: responses the browser stops reading must not hold
			// up the others either.)
		},
	}
	return b
}

func (b *Bridge) start(argv []string) (net.Conn, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	b.mu.Lock()
	b.lastErr = ""
	b.mu.Unlock()
	stderr := &tailBuffer{}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, stderr
	if err := cmd.Start(); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, err
	}
	inR.Close()
	outW.Close()
	c := &pipeConn{r: outR, w: inW, proc: cmd.Process, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		// Keep what it said for the error users see (e.g. a container
		// started before this version, without the bridge in it).
		if s := stderr.String(); s != "" {
			b.mu.Lock()
			b.lastErr = s
			b.mu.Unlock()
		}
		close(c.exited)
		// Unblock the reader if the process went away on its own.
		_ = outR.Close()
	}()
	return c, nil
}

func (b *Bridge) wrap(err error) error {
	b.mu.Lock()
	last := b.lastErr
	b.mu.Unlock()
	if last != "" {
		return fmt.Errorf("%w (%s)", err, last)
	}
	return err
}

// Ports lists what listens in the container.
func (b *Bridge) Ports(ctx context.Context) ([]Port, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://bridge/ports", nil)
	resp, err := b.tr.RoundTrip(req)
	if err != nil {
		return nil, b.wrap(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(strings.TrimSpace(string(body)))
	}
	var out []Port
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Dial connects to a port in the container. ctx limits only the
// connecting, not the connection.
func (b *Bridge) Dial(ctx context.Context, port int) (net.Conn, error) {
	pr, pw := io.Pipe()
	life, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(life, "POST", "http://bridge/dial/"+strconv.Itoa(port), pr)
	type result struct {
		resp *http.Response
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := b.tr.RoundTrip(req)
		ch <- result{resp, err}
	}()
	var res result
	select {
	case res = <-ch:
	case <-ctx.Done():
		cancel()
		_ = pw.Close()
		return nil, ctx.Err()
	}
	if res.err != nil {
		cancel()
		_ = pw.Close()
		return nil, b.wrap(res.err)
	}
	if res.resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.resp.Body, 4096))
		res.resp.Body.Close()
		cancel()
		_ = pw.Close()
		err := errors.New(strings.TrimSpace(string(msg)))
		if strings.Contains(err.Error(), ErrNotListening.Error()) {
			err = fmt.Errorf("%w (%d)", ErrNotListening, port)
		}
		return nil, err
	}
	return &streamConn{body: res.resp.Body, w: pw, cancel: cancel}, nil
}

// Close stops the bridge process (it starts again when needed).
func (b *Bridge) Close() { b.tr.CloseIdleConnections() }

// streamConn is one connection carried by the bridge.
type streamConn struct {
	body   io.ReadCloser
	w      *io.PipeWriter
	cancel context.CancelFunc
	once   sync.Once
}

func (c *streamConn) Read(p []byte) (int, error)  { return c.body.Read(p) }
func (c *streamConn) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c *streamConn) CloseWrite() error           { return c.w.Close() }
func (c *streamConn) Close() error {
	c.once.Do(func() {
		_ = c.w.Close()
		_ = c.body.Close()
		c.cancel()
	})
	return nil
}
func (c *streamConn) LocalAddr() net.Addr                { return pipeAddr{} }
func (c *streamConn) RemoteAddr() net.Addr               { return pipeAddr{} }
func (c *streamConn) SetDeadline(t time.Time) error      { return nil }
func (c *streamConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *streamConn) SetWriteDeadline(t time.Time) error { return nil }
