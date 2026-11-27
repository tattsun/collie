package session

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
)

// With a remote Podman (Podman machine on a Mac) the containers run in a
// VM that can't reach the tools socket on the host. Instead, Collie keeps
// a relay running in each container with `podman exec` (relay/relay.py):
// it listens on a socket inside the container, where the agents' bridges
// (relay/bridge.py) connect, and carries each connection over its own
// stdio. Collie serves every relayed connection like one on its socket.

var (
	//go:embed relay/relay.py
	relaySrc string
	//go:embed relay/bridge.py
	bridgeSrc string
)

// Inside the container: the relay's socket, and the bridge it writes.
const (
	relaySock   = sandbox.CollieRunDir + "/" + toolSockName
	relayBridge = sandbox.CollieRunDir + "/bridge.py"
)

const (
	maxRelayFrame = 32 << 20 // a frame carries at most 64 KB of data; this is for lines gone wrong
	relayBacklog  = 256      // chunks waiting for a connection's reader before it is dropped
	maxRelayConns = 256      // open connections per relay; more are refused
)

type relayFrame struct {
	C     int64  `json:"c,omitempty"`
	Open  int    `json:"o,omitempty"`
	Data  []byte `json:"d,omitempty"`
	Close int    `json:"x,omitempty"`
	Ping  int64  `json:"p,omitempty"`
	Ready int    `json:"ready,omitempty"`
}

type toolRelay struct {
	m     *Manager
	sc    toolScope
	cmd   *exec.Cmd
	kill  func(sig string)
	proxy string // the allowlist proxy's socket on the host, if the container has one

	wmu   sync.Mutex // writes to stdin
	stdin io.WriteCloser

	mu    sync.Mutex
	conns map[int64]*relayConn
	ping  int64
	pong  chan int64

	done   chan struct{}
	stderr strings.Builder // (only read after done)
}

// ensureRelay makes sure the relay for the user's container of the project
// is running.
func (m *Manager) ensureRelay(p model.Project, uid string) error {
	sc := toolScope{uid: uid}
	if !p.Home {
		sc.project = p.ID
	}
	m.relayMu.Lock()
	defer m.relayMu.Unlock()
	if m.relays == nil {
		m.relays = map[string]*toolRelay{}
	}
	if r := m.relays[sc.key()]; r != nil {
		if r.alive() {
			return nil
		}
		// e.g. the container was re-created and took the old relay along
		r.stop()
	}
	r, err := m.startRelay(sc, p, uid)
	if err != nil {
		return err
	}
	m.relays[sc.key()] = r
	return nil
}

// relayCommand returns the argv that runs cmd (the relay) in the user's
// container for the project, and a function that signals it. (Tests run
// it on this machine instead.)
var relayCommand = func(m *Manager, projectID, uid string, cmd []string) ([]string, func(string)) {
	return m.sb.Exec(projectID, uid, "/", "collie-tools-"+model.NewID(), false, nil, cmd)
}

func (m *Manager) startRelay(sc toolScope, p model.Project, uid string) (*toolRelay, error) {
	cmd := []string{"python3", "-u", "-c", relaySrc, relaySock, bridgeSrc}
	r := &toolRelay{m: m, sc: sc, conns: map[int64]*relayConn{}, pong: make(chan int64, 1), done: make(chan struct{})}
	if p.Allowlisted() {
		// The container's only way out (see sandbox/network.go).
		r.proxy = m.sb.ProxySocket(p.ID, uid)
		cmd = append(cmd, sandbox.ProxyAddr)
	}
	argv, kill := relayCommand(m, p.ID, uid, cmd)
	r.kill = kill
	r.cmd = exec.Command(argv[0], argv[1:]...)
	stdin, err := r.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := r.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	r.stdin = stdin
	r.cmd.Stderr = &limitedWriter{b: &r.stderr, n: 4 << 10}
	if err := r.cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the tools relay: %w", err)
	}
	ready := make(chan struct{})
	go r.read(stdout, ready)
	select {
	case <-ready:
		return r, nil
	case <-r.done:
		return nil, fmt.Errorf("the tools relay exited: %s", strings.TrimSpace(r.stderr.String()))
	case <-time.After(30 * time.Second):
		r.stop()
		return nil, errors.New("the tools relay did not start")
	}
}

func (r *toolRelay) read(out io.Reader, ready chan struct{}) {
	defer r.finish()
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64<<10), maxRelayFrame)
	started := false
	for sc.Scan() {
		var f relayFrame
		if json.Unmarshal(sc.Bytes(), &f) != nil {
			continue
		}
		switch {
		case f.Ready == 1:
			if !started {
				started = true
				close(ready)
			}
		case f.Ping != 0:
			select {
			case r.pong <- f.Ping:
			default:
			}
		case f.Open == 1 || f.Open == 2:
			r.open(f.C, f.Open == 2)
		default:
			r.mu.Lock()
			rc := r.conns[f.C]
			r.mu.Unlock()
			if rc == nil {
				continue
			}
			if len(f.Data) > 0 {
				rc.deliver(f.Data)
			}
			if f.Close == 1 {
				rc.eof()
			}
		}
	}
	if err := sc.Err(); err != nil {
		log.Printf("tools relay %s: %v", r.sc.key(), err)
	}
}

// finish runs once the relay's output ends: its connections end too.
func (r *toolRelay) finish() {
	_ = r.stdin.Close()
	_ = r.cmd.Wait()
	r.mu.Lock()
	conns := r.conns
	r.conns = map[int64]*relayConn{}
	r.mu.Unlock()
	for _, rc := range conns {
		rc.eof()
	}
	close(r.done)
}

func (r *toolRelay) open(id int64, proxy bool) {
	r.mu.Lock()
	full := len(r.conns) >= maxRelayConns
	r.mu.Unlock()
	if full {
		_ = r.send(relayFrame{C: id, Close: 1})
		return
	}
	rc := &relayConn{r: r, id: id, in: make(chan []byte, relayBacklog)}
	rc.pr, rc.pw = io.Pipe()
	go func() {
		for b := range rc.in {
			if _, err := rc.pw.Write(b); err != nil {
				break
			}
		}
		_ = rc.pw.Close()
	}()
	r.mu.Lock()
	r.conns[id] = rc
	r.mu.Unlock()
	if proxy {
		go r.toProxy(rc)
	} else {
		go r.m.serveTools(&toolConn{scope: r.sc}, rc)
	}
}

// toProxy connects a relayed connection to the allowlist proxy.
func (r *toolRelay) toProxy(rc *relayConn) {
	if r.proxy == "" {
		rc.Close()
		return
	}
	u, err := net.Dial("unix", r.proxy)
	if err != nil {
		rc.Close()
		return
	}
	sent := make(chan struct{})
	go func() {
		_, _ = io.Copy(u, rc)
		_ = u.(*net.UnixConn).CloseWrite()
		close(sent)
	}()
	_, _ = io.Copy(rc, u)
	rc.Close()
	<-sent
	u.Close()
}

func (r *toolRelay) send(f relayFrame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	r.wmu.Lock()
	defer r.wmu.Unlock()
	_, err = r.stdin.Write(append(b, '\n'))
	return err
}

// alive pings the relay.
func (r *toolRelay) alive() bool {
	select {
	case <-r.done:
		return false
	default:
	}
	r.ping++
	if r.send(relayFrame{Ping: r.ping}) != nil {
		return false
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case p := <-r.pong:
			if p == r.ping {
				return true
			}
		case <-r.done:
			return false
		case <-timeout:
			return false
		}
	}
}

// stop lets the relay go: it exits once its stdin closes; if it doesn't,
// it is killed.
func (r *toolRelay) stop() {
	_ = r.stdin.Close()
	select {
	case <-r.done:
		return
	case <-time.After(2 * time.Second):
	}
	r.kill("KILL")
	if r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
}

// stopRelays stops the relays of a project's containers.
func (m *Manager) stopRelays(projectID string) {
	m.relayMu.Lock()
	var rs []*toolRelay
	for k, r := range m.relays {
		if strings.HasPrefix(k, projectID+"/") {
			rs = append(rs, r)
			delete(m.relays, k)
		}
	}
	m.relayMu.Unlock()
	for _, r := range rs {
		r.stop()
	}
}

// relayConn is one relayed connection, as serveTools sees it.
type relayConn struct {
	r  *toolRelay
	id int64
	pr *io.PipeReader
	pw *io.PipeWriter

	mu       sync.Mutex
	in       chan []byte // data from the relay, waiting for Read
	inClosed bool
	once     sync.Once
}

func (rc *relayConn) Read(p []byte) (int, error) { return rc.pr.Read(p) }

func (rc *relayConn) Write(p []byte) (int, error) {
	if err := rc.r.send(relayFrame{C: rc.id, Data: p}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (rc *relayConn) Close() error {
	rc.once.Do(func() {
		_ = rc.r.send(relayFrame{C: rc.id, Close: 1})
		rc.r.mu.Lock()
		delete(rc.r.conns, rc.id)
		rc.r.mu.Unlock()
		rc.eof()
		_ = rc.pr.Close()
	})
	return nil
}

func (rc *relayConn) deliver(b []byte) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.inClosed {
		return
	}
	select {
	case rc.in <- b:
	default:
		// Not being read: drop it rather than hold up the others.
		rc.closeIn()
		go rc.Close()
	}
}

// eof: the other end is done sending.
func (rc *relayConn) eof() {
	rc.mu.Lock()
	rc.closeIn()
	rc.mu.Unlock()
}

func (rc *relayConn) closeIn() {
	if !rc.inClosed {
		rc.inClosed = true
		close(rc.in)
	}
}

// limitedWriter keeps the first n bytes written to it.
type limitedWriter struct {
	mu sync.Mutex
	b  *strings.Builder
	n  int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if left := w.n - w.b.Len(); left > 0 {
		w.b.Write(p[:min(len(p), left)])
	}
	return len(p), nil
}
