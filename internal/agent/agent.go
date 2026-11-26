// Package agent drives coding agent CLIs (Claude Code, Codex) over their
// machine-readable stdio protocols and translates their events into the
// normalized model.Item timeline.
package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tattsun/collie/internal/model"
)

// Sink receives timeline and status updates from a driver. Implementations
// must be safe for concurrent use.
type Sink interface {
	// Put inserts the item, or replaces an existing item with the same id.
	Put(it *model.Item)
	// Update mutates an existing item. Returns false if it does not exist.
	Update(id string, fn func(*model.Item)) bool
	// Append streams text onto an item field ("text" or "output").
	Append(id, field, text string)
	// Status reports the agent state (model.Status*).
	Status(status string)
	// Meta mutates session metadata (model, agent ref, cost...).
	Meta(fn func(*model.Session))
	// Caps reports slash commands / models the agent supports.
	Caps(c model.Caps)
	// Exited is called once when the agent process ends.
	Exited(err error)
}

type Config struct {
	Bin    string
	Cwd    string
	Model  string
	Effort string
	Mode   string
	Ref    string // agent conversation id to create / resume
	Resume bool
}

type Image struct {
	Path string
	Mime string
}

type Input struct {
	ItemID string // id of the user item already on the timeline
	Text   string
	Images []Image
}

type Reply struct {
	Option  string              `json:"option"`
	Answers map[string][]string `json:"answers"`
	Text    string              `json:"text"`
}

type Driver interface {
	Start() error
	Send(in Input) error
	Interrupt() error
	Reply(askID string, r Reply) error
	SetMode(mode string) error
	Close()
	Done() <-chan struct{}
}

// Modes lists the permission modes offered for each agent.
var Modes = map[string][]ModeInfo{
	"claude": {
		{ID: "default", Label: "Ask before edits & commands"},
		{ID: "acceptEdits", Label: "Auto-accept edits"},
		{ID: "plan", Label: "Plan mode"},
		{ID: "auto", Label: "Auto (classifier)"},
		{ID: "bypassPermissions", Label: "Bypass all permissions"},
	},
	"codex": {
		{ID: "auto", Label: "Auto (workspace write, ask to escalate)"},
		{ID: "untrusted", Label: "Ask for untrusted commands"},
		{ID: "read-only", Label: "Read only"},
		{ID: "full", Label: "Full access (no sandbox)"},
	},
}

type ModeInfo struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

func DefaultMode(agent string) string {
	if agent == "codex" {
		return "auto"
	}
	return "default"
}

// ---- process plumbing shared by the drivers ----

type proc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	wmu    sync.Mutex
	done   chan struct{}
	stderr *tailBuffer
	err    error
}

func startProc(bin string, args []string, cwd string, onLine func([]byte)) (*proc, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p := &proc{cmd: cmd, stdin: stdin, done: make(chan struct{}), stderr: &tailBuffer{max: 8 << 10}}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}
	go func() {
		r := bufio.NewReaderSize(stdout, 1<<16)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 1 {
				onLine(line)
			}
			if err != nil {
				break
			}
		}
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

func (p *proc) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	p.wmu.Lock()
	defer p.wmu.Unlock()
	select {
	case <-p.done:
		return errors.New("agent process has exited")
	default:
	}
	_, err = p.stdin.Write(b)
	return err
}

// close asks the process to exit by closing stdin, then escalates.
func (p *proc) close() {
	p.wmu.Lock()
	_ = p.stdin.Close()
	p.wmu.Unlock()
	select {
	case <-p.done:
		return
	case <-time.After(2 * time.Second):
	}
	if p.cmd.Process != nil {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	}
	select {
	case <-p.done:
		return
	case <-time.After(3 * time.Second):
	}
	if p.cmd.Process != nil {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
	<-p.done
}

func (p *proc) exitError() error {
	tail := strings.TrimSpace(p.stderr.String())
	if p.err == nil && tail == "" {
		return nil
	}
	msg := "agent process exited"
	if p.err != nil {
		msg += ": " + p.err.Error()
	}
	if tail != "" {
		msg += "\n" + lastLines(tail, 12)
	}
	return errors.New(msg)
}

type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(b), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ---- small helpers ----

const maxOutput = 48 << 10

// clip keeps the head and tail of very long tool output.
func clip(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	head := s[:maxOutput*2/3]
	tail := s[len(s)-maxOutput/3:]
	return head + fmt.Sprintf("\n\n… %d bytes omitted …\n\n", len(s)-len(head)-len(tail)) + tail
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func num(m map[string]any, k string) float64 {
	if v, ok := m[k].(float64); ok {
		return v
	}
	return 0
}

func obj(m map[string]any, k string) map[string]any {
	if v, ok := m[k].(map[string]any); ok {
		return v
	}
	return nil
}

func arr(m map[string]any, k string) []any {
	if v, ok := m[k].([]any); ok {
		return v
	}
	return nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
