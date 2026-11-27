package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/model"
)

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func waitDone(t *testing.T, p *proc) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent was not stopped")
	}
}

// A line longer than maxLine is never held whole: the agent is stopped.
func TestOverlongLineStopsTheAgent(t *testing.T) {
	bin := script(t, "head -c 40000000 /dev/zero | tr '\\0' a\necho\nsleep 30\n")
	var longest int
	p, err := startProc(Config{Cwd: t.TempDir()}, bin, nil, func(line []byte) { longest = max(longest, len(line)) })
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, p)
	if longest > maxLine {
		t.Errorf("a %d-byte line reached the parser", longest)
	}
	if err := p.exitError(); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Errorf("exit error: %v", err)
	}
}

// A line the driver chokes on stops that agent, not Collie.
func TestPanicInLineHandlerStopsOnlyTheAgent(t *testing.T) {
	bin := script(t, "echo '{}'\nsleep 30\n")
	p, err := startProc(Config{Cwd: t.TempDir()}, bin, nil, func([]byte) {
		var it *model.Item
		it.Tool.Output = "boom" // the nil dereference an agent could cause
	})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, p)
	if err := p.exitError(); err == nil || !strings.Contains(err.Error(), "could not handle") {
		t.Errorf("exit error: %v", err)
	}
}

// Lines up to the limit still arrive whole, in pieces larger than the
// reader's buffer.
func TestLongLinesUnderTheLimitArrive(t *testing.T) {
	bin := script(t, "head -c 200000 /dev/zero | tr '\\0' a\necho\necho short\n")
	var got []int
	p, err := startProc(Config{Cwd: t.TempDir()}, bin, nil, func(line []byte) { got = append(got, len(line)) })
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, p)
	if len(got) != 2 || got[0] != 200001 || got[1] != 6 {
		t.Errorf("lines: %v", got)
	}
	if err := p.exitError(); err != nil {
		t.Errorf("exit error: %v", err)
	}
}

// itemSink keeps the items a driver puts and applies its updates.
type itemSink struct {
	capsSink
	items map[string]*model.Item
}

func (s *itemSink) Put(it *model.Item) { s.items[it.ID] = it }
func (s *itemSink) Update(id string, fn func(*model.Item)) bool {
	it, ok := s.items[id]
	if ok {
		fn(it)
	}
	return ok
}

// What the user answers to a secret question goes to the agent, never onto
// the timeline.
func TestCodexSecretAnswersStayOffTheTimeline(t *testing.T) {
	s := &itemSink{items: map[string]*model.Item{}}
	c := NewCodex(Config{}, s)
	sent := filepath.Join(t.TempDir(), "sent")
	p, err := startProc(Config{Cwd: t.TempDir()}, script(t, "cat > '"+sent+"'\n"), nil, c.onLine)
	if err != nil {
		t.Fatal(err)
	}
	c.p = p
	c.pending["ask-7"] = &codexAsk{rpcID: []byte("7"), kind: "question", questions: []model.Question{
		{ID: "user", Question: "User name?"}, {ID: "pw", Question: "Password?", Secret: true}}}
	s.items["ask-7"] = &model.Item{ID: "ask-7", Kind: model.KindQuestion, Ask: &model.Ask{State: "pending"}}
	if err := c.Reply("ask-7", Reply{Answers: map[string][]string{"user": {"alice"}, "pw": {"hunter2"}}}); err != nil {
		t.Fatal(err)
	}
	p.close()
	if got := s.items["ask-7"].Ask.Answer; got != "alice / "+secretAnswer {
		t.Errorf("answer on the timeline: %q", got)
	}
	if b, _ := os.ReadFile(sent); !strings.Contains(string(b), "hunter2") {
		t.Errorf("the agent did not get the secret: %s", b)
	}
}
