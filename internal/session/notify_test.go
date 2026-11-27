package session

import (
	"testing"
	"time"

	"github.com/tattsun/collie/internal/model"
)

func TestNotifyOnStatusChanges(t *testing.T) {
	m, _ := newTestManager(t)
	m.st.PutProject(model.Project{ID: "pa", OwnerUser: "alice"})
	m.st.PutSession(model.Session{ID: "s1", ProjectID: "pa", Owner: "alice", Status: model.StatusStopped})
	got := make(chan string, 10)
	m.Notify = func(s model.Session, ev string) { got <- s.Status + ":" + ev }
	expect := func(want string) {
		t.Helper()
		select {
		case ev := <-got:
			if ev != want {
				t.Fatalf("event %q, want %q", ev, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no event, want %q", want)
		}
	}
	quiet := func() {
		t.Helper()
		select {
		case ev := <-got:
			t.Fatalf("unexpected event %q", ev)
		case <-time.After(50 * time.Millisecond):
		}
	}

	m.setStatus("s1", model.StatusStarting)
	m.setStatus("s1", model.StatusRunning)
	quiet()
	m.setStatus("s1", model.StatusWaiting)
	expect("waiting:approval")
	m.setStatus("s1", model.StatusWaiting)
	quiet()
	m.setStatus("s1", model.StatusRunning)
	m.setStatus("s1", model.StatusIdle)
	expect("idle:done")
	m.setStatus("s1", model.StatusStopped) // idle timeout: not news
	m.setStatus("s1", model.StatusIdle)
	quiet()

	// A question is told apart from an approval.
	r, _ := m.runtime("s1")
	r.mu.Lock()
	r.loaded = true
	r.items = []*model.Item{
		{ID: "a1", Kind: model.KindApproval, Ask: &model.Ask{State: "answered"}},
		{ID: "q1", Kind: model.KindQuestion, Ask: &model.Ask{State: "pending"}},
	}
	r.mu.Unlock()
	m.setStatus("s1", model.StatusRunning)
	m.setStatus("s1", model.StatusWaiting)
	expect("waiting:question")

	m.setStatus("s1", model.StatusError)
	expect("error:error")
	m.setStatus("s1", model.StatusError)
	quiet()
}

func TestForeground(t *testing.T) {
	h := NewHub()
	c := NewClient("alice", "h1", func() {})
	h.Add(c)
	if h.Foreground("alice", "p1") {
		t.Fatal("foreground before any report")
	}
	c.SetForeground(true, "p1")
	if !h.Foreground("alice", "p1") || h.Foreground("bob", "p1") || h.Foreground("alice", "p2") || h.Foreground("alice", "") {
		t.Fatal("foreground must match the user and the browser")
	}
	c.SetForeground(false, "p1")
	if h.Foreground("alice", "p1") {
		t.Fatal("hidden page counted as foreground")
	}
	c.SetForeground(true, "p1")
	c.mu.Lock()
	c.visAt = time.Now().Add(-foregroundTTL - time.Second) // a page frozen in the background stops reporting
	c.mu.Unlock()
	if h.Foreground("alice", "p1") {
		t.Fatal("a stale report counted as foreground")
	}
	c.SetForeground(true, "p1")
	h.Remove(c)
	if h.Foreground("alice", "p1") {
		t.Fatal("a closed connection counted as foreground")
	}
}
