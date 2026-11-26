// Package term provides persistent PTY shells that browsers can attach to
// (and re-attach to after a disconnect, with scrollback replay).
package term

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/tattsun/collie/internal/model"
)

const scrollback = 512 << 10

type Info struct {
	ID        string `json:"id"`
	Cwd       string `json:"cwd"`
	Title     string `json:"title"`
	Owner     string `json:"owner,omitempty"`   // project or session id
	Project   string `json:"project,omitempty"` // project id
	User      string `json:"user,omitempty"`    // the user who opened it (only they see it)
	Isolated  bool   `json:"isolated,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

// Spec describes the shell to start.
type Spec struct {
	Cwd, Title, Owner, Project, User string
	Cols, Rows                       int
	// Argv overrides the local login shell (e.g. a shell inside a
	// container); Kill then signals what it started.
	Argv []string
	Kill func(sig string)
	id   string
}

type Term struct {
	Info
	kill func(sig string)
	cmd  *exec.Cmd
	pty  *os.File
	mu   sync.Mutex
	buf  []byte
	subs map[chan []byte]struct{}
	done chan struct{}
}

type Manager struct {
	mu       sync.Mutex
	terms    map[string]*Term
	OnChange func()
}

func NewManager() *Manager { return &Manager{terms: map[string]*Term{}} }

func (m *Manager) changed() {
	if m.OnChange != nil {
		go m.OnChange()
	}
}

func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.terms))
	for _, t := range m.terms {
		out = append(out, t.Info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

func (m *Manager) Get(id string) (*Term, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.terms[id]
	return t, ok
}

// InUse reports whether the user has a terminal open in the project.
func (m *Manager) InUse(projectID, userID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.terms {
		if t.Project == projectID && t.User == userID {
			return true
		}
	}
	return false
}

func (m *Manager) Create(sp Spec) (*Term, error) {
	cwd, cols, rows := sp.Cwd, sp.Cols, sp.Rows
	st, err := os.Stat(cwd)
	if err != nil || !st.IsDir() {
		return nil, errors.New("directory not found: " + cwd)
	}
	argv := sp.Argv
	if len(argv) == 0 {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/bash"
		}
		argv = []string{shell, "-l"}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
	if cols <= 0 || rows <= 0 {
		cols, rows = 100, 30
	}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	t := &Term{
		Info: Info{ID: sp.ID(), Cwd: cwd, Title: sp.Title, Owner: sp.Owner, Project: sp.Project, User: sp.User,
			Isolated: len(sp.Argv) > 0, CreatedAt: model.NowMs()},
		kill: sp.Kill, cmd: cmd, pty: f, subs: map[chan []byte]struct{}{}, done: make(chan struct{}),
	}
	m.mu.Lock()
	m.terms[t.ID] = t
	m.mu.Unlock()
	go t.readLoop()
	go func() {
		_ = cmd.Wait()
		<-t.done
		m.mu.Lock()
		delete(m.terms, t.ID)
		m.mu.Unlock()
		m.changed()
	}()
	m.changed()
	return t, nil
}

func (t *Term) readLoop() {
	defer close(t.done)
	b := make([]byte, 32<<10)
	for {
		n, err := t.pty.Read(b)
		if n > 0 {
			chunk := append([]byte(nil), b[:n]...)
			t.mu.Lock()
			t.buf = append(t.buf, chunk...)
			if len(t.buf) > scrollback {
				cut := len(t.buf) - scrollback
				if i := bytes.IndexByte(t.buf[cut:], '\n'); i >= 0 && i < 4096 {
					cut += i + 1
				}
				t.buf = append([]byte(nil), t.buf[cut:]...)
			}
			for ch := range t.subs {
				select {
				case ch <- chunk:
				default:
					// Too slow: drop the subscriber; it will re-attach and replay.
					delete(t.subs, ch)
					close(ch)
				}
			}
			t.mu.Unlock()
		}
		if err != nil {
			t.mu.Lock()
			for ch := range t.subs {
				delete(t.subs, ch)
				close(ch)
			}
			t.mu.Unlock()
			return
		}
	}
}

// Attach returns the scrollback and a channel of subsequent output. The
// channel is closed when the shell exits or the subscriber falls behind.
func (t *Term) Attach() ([]byte, chan []byte, func()) {
	ch := make(chan []byte, 512)
	t.mu.Lock()
	replay := append([]byte(nil), t.buf...)
	select {
	case <-t.done:
		close(ch)
	default:
		t.subs[ch] = struct{}{}
	}
	t.mu.Unlock()
	return replay, ch, func() {
		t.mu.Lock()
		if _, ok := t.subs[ch]; ok {
			delete(t.subs, ch)
			close(ch)
		}
		t.mu.Unlock()
	}
}

func (t *Term) Write(b []byte) error {
	_, err := t.pty.Write(b)
	return err
}

func (t *Term) Resize(cols, rows int) {
	if cols > 0 && rows > 0 && cols < 1000 && rows < 1000 {
		_ = pty.Setsize(t.pty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	}
}

func (t *Term) Done() <-chan struct{} { return t.done }

// ID returns the id the terminal will get (fixed up front so a container
// shell can be tagged with it).
func (sp *Spec) ID() string {
	if sp.id == "" {
		sp.id = model.NewID()
	}
	return sp.id
}

func (t *Term) Close() {
	if t.kill != nil {
		go func() {
			t.kill("HUP")
			select {
			case <-t.done:
			case <-time.After(3 * time.Second):
				t.kill("KILL")
			}
		}()
	}
	if t.cmd.Process != nil {
		pid := t.cmd.Process.Pid
		_ = syscall.Kill(-pid, syscall.SIGHUP)
		_ = t.cmd.Process.Signal(syscall.SIGHUP)
		go func() {
			select {
			case <-t.done:
			case <-time.After(3 * time.Second):
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				_ = t.cmd.Process.Kill()
			}
		}()
	}
	_ = t.pty.Close()
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	ts := make([]*Term, 0, len(m.terms))
	for _, t := range m.terms {
		ts = append(ts, t)
	}
	m.mu.Unlock()
	for _, t := range ts {
		t.Close()
	}
}
