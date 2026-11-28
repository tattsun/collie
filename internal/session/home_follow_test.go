package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/agent"
	"github.com/tattsun/collie/internal/gitutil"
	"github.com/tattsun/collie/internal/model"
)

// fakeDriver stands in for an agent process: it records what it is sent.
type fakeDriver struct {
	sent    chan agent.Input
	onClose func()
}

func newFakeDriver() *fakeDriver { return &fakeDriver{sent: make(chan agent.Input, 16)} }

func (f *fakeDriver) Start() error                    { return nil }
func (f *fakeDriver) Send(in agent.Input) error       { f.sent <- in; return nil }
func (f *fakeDriver) Interrupt() error                { return nil }
func (f *fakeDriver) Reply(string, agent.Reply) error { return nil }
func (f *fakeDriver) SetMode(string) error            { return nil }
func (f *fakeDriver) Done() <-chan struct{}           { return nil }
func (f *fakeDriver) Close() {
	if f.onClose != nil {
		f.onClose()
	}
}

// attach gives a session a fake running agent.
func attach(t *testing.T, m *Manager, sid string) *fakeDriver {
	t.Helper()
	r, err := m.runtime(sid)
	if err != nil {
		t.Fatal(err)
	}
	d := newFakeDriver()
	r.mu.Lock()
	r.drv = d
	r.mu.Unlock()
	return d
}

// fastNotes makes notifications quick for a test.
func fastNotes(t *testing.T, settle, gap time.Duration, limit int) {
	t.Helper()
	s, g, c := noteSettle, noteGap, noteCap
	noteSettle, noteGap, noteCap = settle, gap, limit
	t.Cleanup(func() { noteSettle, noteGap, noteCap = s, g, c })
}

func received(t *testing.T, d *fakeDriver, within time.Duration) agent.Input {
	t.Helper()
	select {
	case in := <-d.sent:
		return in
	case <-time.After(within):
		t.Fatal("no notification arrived")
	}
	return agent.Input{}
}

func nothing(t *testing.T, d *fakeDriver, within time.Duration) {
	t.Helper()
	select {
	case in := <-d.sent:
		t.Fatalf("unexpected notification: %s", in.Text)
	case <-time.After(within):
	}
}

// homeFixture: alice's personal project and Home, a team project (alice
// owns the group, bob is a member), and their sessions.
func homeFixture(t *testing.T) *Manager {
	t.Helper()
	m := newHomeTestManager(t)
	m.st.PutProject(model.Project{ID: "pa", Name: "alpha", Path: t.TempDir(), OwnerUser: "alice", Runtime: "container"})
	m.st.PutProject(model.Project{ID: "pt", Name: "teamproj", Path: t.TempDir(), OwnerGroup: "team", Runtime: "container"})
	m.st.PutProject(model.Project{ID: "ha", Name: "Home", Path: t.TempDir(), OwnerUser: "alice", Home: true, Runtime: "container"})
	m.st.PutProject(model.Project{ID: "hb", Name: "Home", Path: t.TempDir(), OwnerUser: "bob", Home: true, Runtime: "container"})
	for _, s := range []model.Session{
		{ID: "a1", ProjectID: "pa", Owner: "alice", Title: "fix login", Status: model.StatusIdle},
		{ID: "a2", ProjectID: "pa", Owner: "alice", Title: "add tests", Status: model.StatusIdle},
		{ID: "t-alice", ProjectID: "pt", Owner: "alice", Title: "alice shared", Shared: true, Status: model.StatusIdle},
		{ID: "t-bob", ProjectID: "pt", Owner: "bob", Title: "bob shared", Shared: true, Status: model.StatusIdle},
		{ID: "t-bob-private", ProjectID: "pt", Owner: "bob", Title: "bob private", Status: model.StatusIdle},
		{ID: "h1", ProjectID: "ha", Owner: "alice", Title: "chat", Status: model.StatusIdle},
		{ID: "h2", ProjectID: "ha", Owner: "alice", Title: "other chat", Status: model.StatusIdle},
		{ID: "hb1", ProjectID: "hb", Owner: "bob", Title: "bob chat", Status: model.StatusIdle},
	} {
		m.st.PutSession(s)
	}
	return m
}

func homeConn(uid, sid string) *toolConn { return &toolConn{scope: toolScope{uid: uid}, session: sid} }

func TestHomeStopAndArchiveOnlyTheUsersSessions(t *testing.T) {
	m := homeFixture(t)
	alice, bob := homeConn("alice", "h1"), homeConn("bob", "hb1")

	// A running session of alice's: stopping it ends its agent.
	m.setStatus("a1", model.StatusRunning)
	d := attach(t, m, "a1")
	d.onClose = func() { go m.setStatus("a1", model.StatusStopped) }
	if text, isErr := callOn(t, m, alice, "stop_session", map[string]any{"session": "a1"}); isErr || !strings.Contains(text, "Stopped") {
		t.Errorf("stop_session: %s", text)
	}
	if s, _ := m.st.Session("a1"); s.Status != model.StatusStopped {
		t.Errorf("a1 is %s after stop_session", s.Status)
	}
	if text, isErr := callOn(t, m, alice, "stop_session", map[string]any{"session": "a1"}); isErr || !strings.Contains(text, "not running") {
		t.Errorf("stopping a stopped session: %s", text)
	}

	refused := []struct {
		tc   *toolConn
		sid  string
		why  string
		want string
	}{
		// alice owns the group, so Collie would let her stop it herself.
		{alice, "t-bob", "another member's shared session", "someone else"},
		{bob, "t-alice", "another member's shared session", "someone else"},
		{alice, "t-bob-private", "an invisible session", "no session"},
		{alice, "nosuch", "an unknown session", "no session"},
		{alice, "hb1", "someone else's Home conversation", "no session"},
	}
	for _, tool := range []string{"stop_session", "archive_session"} {
		for _, c := range refused {
			if text, isErr := callOn(t, m, c.tc, tool, map[string]any{"session": c.sid}); !isErr || !strings.Contains(text, c.want) {
				t.Errorf("%s on %s: %v %s", tool, c.why, isErr, text)
			}
		}
	}
	// Home conversations of one's own are archived (see
	// TestHomeReachesOnlyTheUsersOwnHomeConversations), never stopped.
	if text, isErr := callOn(t, m, alice, "stop_session", map[string]any{"session": "h2"}); !isErr || !strings.Contains(text, "Home conversation") {
		t.Errorf("stop_session on a Home conversation: %v %s", isErr, text)
	}
	for _, id := range []string{"t-bob", "t-alice", "hb1"} {
		if s, _ := m.st.Session(id); s.Archived {
			t.Errorf("%s was archived", id)
		}
	}

	// Archiving one's own session (and unarchiving it).
	if _, err := watchAs(m, "h1", "a2"); err != nil {
		t.Fatal(err)
	}
	if text, isErr := callOn(t, m, alice, "archive_session", map[string]any{"session": "a2"}); isErr || !strings.Contains(text, "Archived") {
		t.Errorf("archive_session: %s", text)
	}
	if s, _ := m.st.Session("a2"); !s.Archived {
		t.Error("a2 not archived")
	}
	if h, _ := m.st.Session("h1"); slices.Contains(h.Watching, "a2") {
		t.Error("archived session still watched")
	}
	if text, isErr := callOn(t, m, alice, "archive_session", map[string]any{"session": "a2", "archived": false}); isErr {
		t.Errorf("unarchive: %s", text)
	}
	if s, _ := m.st.Session("a2"); s.Archived {
		t.Error("a2 still archived")
	}
}

// TestHomeReachesOnlyTheUsersOwnHomeConversations: Home lists, reads and
// archives the user's other Home conversations (not the one calling), and
// nobody else's, admins' Homes included. It does nothing else to them.
func TestHomeReachesOnlyTheUsersOwnHomeConversations(t *testing.T) {
	m := homeFixture(t)
	// root is an admin (fakeAccess) whose Home runs on the host, so its
	// tools act with an admin's rights.
	m.st.PutProject(model.Project{ID: "hr", Name: "Home", Path: t.TempDir(), OwnerUser: "root", Home: true, Runtime: "host"})
	m.st.PutSession(model.Session{ID: "r1", ProjectID: "hr", Owner: "root", Title: "root chat", Status: model.StatusIdle})
	m.st.PutSession(model.Session{ID: "h3", ProjectID: "ha", Owner: "alice", Title: "old chat", Archived: true, Status: model.StatusIdle})
	if !m.toolActor("root").Admin {
		t.Fatal("root's Home tools do not act as an admin")
	}
	r, _ := m.runtime("h2")
	r.sinkPut(&model.Item{ID: "u1", Kind: model.KindUser, Text: "plan my trip", Done: true})
	r.sinkPut(&model.Item{ID: "x1", Kind: model.KindText, Text: "Here is the plan for the trip.", Done: true})
	r.sinkPut(&model.Item{ID: "n1", Kind: model.KindUser, Via: "collie", Text: "[Collie] Update on sessions you watch", Done: true})
	m.updateSession("h2", func(s *model.Session) { s.Watching = []string{"a1"} })

	// The bridge names the conversation calling; only one of the user's own
	// Home conversations is taken.
	alice, aliceUnknown := homeConn("alice", ""), homeConn("alice", "")
	for _, sid := range []string{"hb1", "r1", "a1", "h1"} {
		line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "collie/session", "params": map[string]string{"id": sid}})
		m.handleRPC(alice, line)
		if want := map[bool]string{true: "h1"}[sid == "h1"]; alice.session != want {
			t.Fatalf("after naming %s, alice's Home socket serves %q", sid, alice.session)
		}
	}
	bob, root := homeConn("bob", "hb1"), homeConn("root", "r1")

	type row struct {
		ID       string `json:"id"`
		Project  string `json:"project"`
		Home     bool   `json:"home"`
		This     bool   `json:"thisConversation"`
		Archived bool   `json:"archived"`
	}
	list := func(tc *toolConn, args map[string]any) map[string]row {
		t.Helper()
		text, isErr := callOn(t, m, tc, "list_sessions", args)
		var rows []row
		if isErr || json.Unmarshal([]byte(text), &rows) != nil {
			t.Fatalf("list_sessions %v: %s", args, text)
		}
		out := map[string]row{}
		for _, r := range rows {
			out[r.ID] = r
		}
		return out
	}
	ids := func(rows map[string]row) []string {
		out := slices.Collect(maps.Keys(rows))
		slices.Sort(out)
		return out
	}

	// Listing: with the projects' sessions, or alone.
	all := list(alice, nil)
	if got := ids(all); !slices.Equal(got, []string{"a1", "a2", "h1", "h2", "t-alice", "t-bob"}) {
		t.Errorf("alice's list: %v", got)
	}
	if h := all["h1"]; !h.Home || !h.This || h.Project != "Home" {
		t.Errorf("the calling conversation: %+v", h)
	}
	if h := all["h2"]; !h.Home || h.This {
		t.Errorf("another Home conversation: %+v", h)
	}
	if a := all["a1"]; a.Home || a.This || a.Project != "alpha" {
		t.Errorf("a project session: %+v", a)
	}
	if got := ids(list(alice, map[string]any{"project": "home"})); !slices.Equal(got, []string{"h1", "h2"}) {
		t.Errorf("alice's Home conversations: %v", got)
	}
	if got := ids(list(alice, map[string]any{"project": "Home", "include_archived": true})); !slices.Equal(got, []string{"h1", "h2", "h3"}) {
		t.Errorf("with the archived ones: %v", got)
	}
	if h := list(aliceUnknown, map[string]any{"project": "home"})["h1"]; h.This {
		t.Error("marked as this conversation without knowing which one calls")
	}
	if got := ids(list(bob, map[string]any{"project": "home"})); !slices.Equal(got, []string{"hb1"}) {
		t.Errorf("bob's Home conversations: %v", got)
	}
	if got := ids(list(root, nil)); !slices.Equal(got, []string{"r1"}) {
		t.Errorf("the admin's list: %v", got)
	}
	if got := ids(list(root, map[string]any{"project": "home", "include_archived": true})); !slices.Equal(got, []string{"r1"}) {
		t.Errorf("the admin's Home conversations: %v", got)
	}
	if text, isErr := callOn(t, m, root, "list_sessions", map[string]any{"project": "ha"}); !isErr {
		t.Errorf("the admin listed alice's Home by its id: %s", text)
	}

	// Reading.
	text, isErr := callOn(t, m, alice, "read_session", map[string]any{"session": "h2"})
	for _, want := range []string{`Home conversation "other chat" (h2)`, "### user\nplan my trip", "Here is the plan for the trip.", "### Collie (a notification"} {
		if isErr || !strings.Contains(text, want) {
			t.Errorf("read_session h2 lacks %q: %s", want, text)
		}
	}
	if strings.Contains(text, "in project") {
		t.Errorf("a Home conversation read as a project's: %s", text)
	}
	if text, isErr := callOn(t, m, alice, "read_session", map[string]any{"session": "h1"}); isErr || !strings.Contains(text, "This Home conversation (the one you are in)") {
		t.Errorf("read_session on the calling conversation: %s", text)
	}

	// Others' Home conversations: not found, for an admin too.
	for _, c := range []struct {
		tc  *toolConn
		sid string
	}{{alice, "hb1"}, {alice, "r1"}, {bob, "h2"}, {root, "h1"}, {root, "h2"}, {root, "hb1"}} {
		for _, tool := range []string{"read_session", "archive_session"} {
			if text, isErr := callOn(t, m, c.tc, tool, map[string]any{"session": c.sid}); !isErr || !strings.Contains(text, "no session") {
				t.Errorf("%s of %s by %s: %v %s", tool, c.sid, c.tc.scope.uid, isErr, text)
			}
		}
		if text, isErr := callOn(t, m, c.tc, "archive_session", map[string]any{"session": c.sid, "archived": false}); !isErr {
			t.Errorf("%s unarchived %s: %s", c.tc.scope.uid, c.sid, text)
		}
	}

	// Archiving: not the calling conversation, nor any while Collie can't
	// tell which one calls.
	if text, isErr := callOn(t, m, alice, "archive_session", map[string]any{"session": "h1"}); !isErr || !strings.Contains(text, "the one you are in") {
		t.Errorf("archived the calling conversation: %s", text)
	}
	if text, isErr := callOn(t, m, aliceUnknown, "archive_session", map[string]any{"session": "h2"}); !isErr || !strings.Contains(text, "cannot tell") {
		t.Errorf("archived without knowing which conversation calls: %s", text)
	}
	for _, id := range []string{"h1", "h2", "hb1", "r1"} {
		if s, _ := m.st.Session(id); s.Archived {
			t.Fatalf("%s was archived", id)
		}
	}
	text, isErr = callOn(t, m, alice, "archive_session", map[string]any{"session": "h2"})
	if isErr || !strings.Contains(text, "Archived the Home conversation") || !strings.Contains(text, "sessions it watches") {
		t.Errorf("archive_session h2: %s", text)
	}
	if s, _ := m.st.Session("h2"); !s.Archived {
		t.Error("h2 not archived")
	}
	if _, ok := list(alice, nil)["h2"]; ok {
		t.Error("an archived Home conversation is listed without include_archived")
	}
	if text, isErr := callOn(t, m, alice, "archive_session", map[string]any{"session": "h2", "archived": false}); isErr || !strings.Contains(text, "back") {
		t.Errorf("unarchive h2: %s", text)
	}
	if text, isErr := callOn(t, m, aliceUnknown, "archive_session", map[string]any{"session": "h3", "archived": false}); isErr {
		t.Errorf("unarchive h3: %s", text)
	}
	for _, id := range []string{"h2", "h3"} {
		if s, _ := m.st.Session(id); s.Archived {
			t.Errorf("%s still archived", id)
		}
	}

	// Nothing else reaches a Home conversation, so that two cannot keep
	// setting each other off.
	m.setStatus("h2", model.StatusRunning)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"send_message", map[string]any{"session": "h2", "text": "hi"}},
		{"stop_session", map[string]any{"session": "h2"}},
		{"watch_session", map[string]any{"session": "h2"}},
		{"read_diff", map[string]any{"session": "h2"}},
	} {
		if text, isErr := callOn(t, m, alice, c.tool, c.args); !isErr || !strings.Contains(text, "Home conversation") {
			t.Errorf("%s on a Home conversation: %v %s", c.tool, isErr, text)
		}
	}
	if text, isErr := callOn(t, m, alice, "start_session", map[string]any{"project": "home", "prompt": "go"}); !isErr {
		t.Errorf("start_session in Home: %s", text)
	}
	if h, _ := m.st.Session("h1"); len(h.Watching) > 0 {
		t.Errorf("a Home conversation is watched: %v", h.Watching)
	}
	if s, _ := m.st.Session("h2"); s.Status != model.StatusRunning {
		t.Errorf("h2 is %s", s.Status)
	}
}

// watchAs has alice's Home conversation watch a session, as the tool does.
func watchAs(m *Manager, home, sid string) (string, error) {
	return m.toolWatch(Actor{ID: "alice"}, home, sid)
}

func TestHomeIsToldWhenWatchedSessionsChange(t *testing.T) {
	fastNotes(t, 40*time.Millisecond, 0, 30)
	m := homeFixture(t)
	home := attach(t, m, "h1")
	tc := homeConn("alice", "h1")
	for _, sid := range []string{"a1", "a2"} {
		if text, isErr := callOn(t, m, tc, "watch_session", map[string]any{"session": sid}); isErr {
			t.Fatalf("watch_session: %s", text)
		}
	}
	if text, _ := callOn(t, m, tc, "watch_session", nil); !strings.Contains(text, "fix login") || !strings.Contains(text, "add tests") {
		t.Errorf("watch list: %s", text)
	}

	// a1 finishes its turn.
	put := func(sid string, it *model.Item) {
		r, _ := m.runtime(sid)
		r.sinkPut(it)
	}
	m.setStatus("a1", model.StatusRunning)
	put("a1", &model.Item{ID: "x1", Kind: model.KindText, Text: "All tests pass now.", Done: true})
	put("a1", &model.Item{ID: "r1", Kind: model.KindResult, Done: true, Result: &model.Result{Status: "success"}})
	m.setStatus("a1", model.StatusIdle)
	in := received(t, home, 2*time.Second)
	for _, want := range []string{"[Collie]", "not a message from the user", `"fix login" (session a1, project alpha) finished its turn`, "All tests pass now."} {
		if !strings.Contains(in.Text, want) {
			t.Errorf("notification lacks %q:\n%s", want, in.Text)
		}
	}
	items, _, _ := m.Items(Actor{ID: "alice"}, "h1", 0, 10)
	last := items[len(items)-1]
	if last.Kind != model.KindUser || last.Via != "collie" || len(last.Notes) != 1 || last.Notes[0].Event != "done" || last.Notes[0].Session != "a1" || last.By != "" {
		t.Errorf("timeline entry: %+v", last)
	}

	// Changes in quick succession go out together; one that is over by
	// then (a1 was approved at once) is left out.
	m.setStatus("a1", model.StatusRunning)
	m.setStatus("a1", model.StatusWaiting)
	m.setStatus("a1", model.StatusRunning)
	m.setStatus("a2", model.StatusRunning)
	put("a2", &model.Item{ID: "q1", Kind: model.KindQuestion, Ask: &model.Ask{State: "pending", Questions: []model.Question{{Question: "Which database?"}}}})
	m.setStatus("a2", model.StatusWaiting)
	in = received(t, home, 2*time.Second)
	if !strings.Contains(in.Text, `"add tests"`) || !strings.Contains(in.Text, "has a question for the user") || !strings.Contains(in.Text, "Which database?") {
		t.Errorf("question notification:\n%s", in.Text)
	}
	if strings.Contains(in.Text, "fix login") {
		t.Errorf("told about a wait that was already over:\n%s", in.Text)
	}

	// While Home is in a turn, notifications wait for it to end.
	m.setStatus("h1", model.StatusRunning)
	put("a1", &model.Item{ID: "e1", Kind: model.KindError, Text: "agent process exited: boom", Done: true})
	m.setStatus("a1", model.StatusError)
	nothing(t, home, 200*time.Millisecond)
	m.setStatus("h1", model.StatusIdle)
	in = received(t, home, 2*time.Second)
	if !strings.Contains(in.Text, "stopped with an error") || !strings.Contains(in.Text, "boom") {
		t.Errorf("error notification:\n%s", in.Text)
	}

	// Unwatched: nothing more.
	if text, isErr := callOn(t, m, tc, "unwatch_session", map[string]any{"all": true}); isErr || !strings.Contains(text, "2") {
		t.Errorf("unwatch all: %s", text)
	}
	m.setStatus("a2", model.StatusRunning)
	m.setStatus("a2", model.StatusIdle)
	nothing(t, home, 200*time.Millisecond)
	if h, _ := m.st.Session("h1"); len(h.Watching) != 0 {
		t.Errorf("still watching %v", h.Watching)
	}
}

// fastBackground makes turn ends held back for background work come due
// quickly.
func fastBackground(t *testing.T, grace, stall time.Duration) {
	t.Helper()
	g, s := bgGrace, bgStall
	bgGrace, bgStall = grace, stall
	t.Cleanup(func() { bgGrace, bgStall = g, s })
}

// TestHomeWaitsForBackgroundWork: a turn that ends with work still running
// in the background is told once that work is over, not before; work that
// runs on is told once; approvals, questions and failures are told at
// once all the same.
func TestHomeWaitsForBackgroundWork(t *testing.T) {
	fastNotes(t, 20*time.Millisecond, 0, 30)
	fastBackground(t, 150*time.Millisecond, time.Hour)
	m := homeFixture(t)
	home := attach(t, m, "h1")
	if _, err := watchAs(m, "h1", "a1"); err != nil {
		t.Fatal(err)
	}
	r, _ := m.runtime("a1")
	n := 0
	put := func(it *model.Item) {
		n++
		it.ID, it.Done = fmt.Sprintf("i%d", n), true
		r.sinkPut(it)
	}
	tasks := func(titles ...string) {
		var list []model.Task
		for _, title := range titles {
			list = append(list, model.Task{ID: title, Kind: "local_bash", Title: title, Started: model.NowMs()})
		}
		m.updateSession("a1", func(s *model.Session) { s.Tasks = list })
	}
	// ask: the user's message; then the agent works, says said and ends
	// its turn (failed: with an error), leaving bg running.
	turn := func(ask, said string, failed bool, bg ...string) {
		if ask != "" {
			put(&model.Item{Kind: model.KindUser, Text: ask})
		}
		m.setStatus("a1", model.StatusRunning)
		if len(bg) > 0 {
			tasks(bg...)
		}
		put(&model.Item{Kind: model.KindText, Text: said})
		res := &model.Result{Status: "success"}
		if failed {
			res = &model.Result{Status: "error", IsError: true}
		}
		put(&model.Item{Kind: model.KindResult, Text: said, Result: res})
		m.setStatus("a1", model.StatusIdle)
	}

	// Waiting for CI: told after the turn the agent takes when it is over.
	turn("fix the build", "Pushed; waiting for CI.", false, "gh pr checks --watch")
	nothing(t, home, 300*time.Millisecond)
	tasks()
	put(&model.Item{Kind: model.KindNotice, Text: "Background task finished: gh pr checks --watch"})
	turn("", "CI is green.", false)
	in := received(t, home, 2*time.Second)
	if !strings.Contains(in.Text, "finished its turn") || !strings.Contains(in.Text, "CI is green.") || strings.Contains(in.Text, "background") {
		t.Errorf("notification:\n%s", in.Text)
	}
	nothing(t, home, 400*time.Millisecond)

	// The work ends but the agent does not take it up: the turn is told
	// after bgGrace.
	turn("lint it", "Linting in the background.", false, "npm run lint")
	nothing(t, home, 200*time.Millisecond)
	ended := time.Now()
	tasks()
	in = received(t, home, 2*time.Second)
	if since := time.Since(ended); since < bgGrace {
		t.Errorf("told %v after the work ended, before bgGrace", since)
	}
	if !strings.Contains(in.Text, "finished its turn. Its last message: \"Linting in the background.\"") {
		t.Errorf("notification:\n%s", in.Text)
	}

	// Questions, errors and failed turns do not wait for background work.
	put(&model.Item{Kind: model.KindUser, Text: "deploy"})
	m.setStatus("a1", model.StatusRunning)
	tasks("npm run build")
	put(&model.Item{Kind: model.KindQuestion, Ask: &model.Ask{State: "pending", Questions: []model.Question{{Question: "Staging or production?"}}}})
	m.setStatus("a1", model.StatusWaiting)
	if in := received(t, home, 2*time.Second); !strings.Contains(in.Text, "Staging or production?") {
		t.Errorf("question notification:\n%s", in.Text)
	}
	m.setStatus("a1", model.StatusRunning)
	put(&model.Item{Kind: model.KindError, Text: "agent process exited: boom"})
	m.setStatus("a1", model.StatusError)
	if in := received(t, home, 2*time.Second); !strings.Contains(in.Text, "stopped with an error") {
		t.Errorf("error notification:\n%s", in.Text)
	}
	turn("go on", "Prompt is too long", true, "npm run build")
	if in := received(t, home, 2*time.Second); !strings.Contains(in.Text, "ended its turn with an error") {
		t.Errorf("failure notification:\n%s", in.Text)
	}
	tasks()
	nothing(t, home, 400*time.Millisecond) // told already

	// Work that runs on (a dev server): told once, after bgStall.
	fastBackground(t, 150*time.Millisecond, 300*time.Millisecond)
	start := time.Now()
	turn("start the dev server", "The dev server is on port 5173.", false, "npm run dev")
	in = received(t, home, 3*time.Second)
	if since := time.Since(start); since < bgStall {
		t.Errorf("told %v after the turn ended, before bgStall", since)
	}
	for _, want := range []string{"still running in the background: npm run dev (started just now)", "Collie tells you again", "port 5173"} {
		if !strings.Contains(in.Text, want) {
			t.Errorf("notification lacks %q:\n%s", want, in.Text)
		}
	}
	items, _, _ := m.Items(Actor{ID: "alice"}, "h1", 0, 100)
	if last := items[len(items)-1]; len(last.Notes) != 1 || last.Notes[0].Event != "background" || len(last.Notes[0].Background) != 1 {
		t.Errorf("timeline entry: %+v", last.Notes)
	}
	nothing(t, home, 600*time.Millisecond)

	// While Home is in a turn, it comes due all the same, and is told once
	// Home's turn is over.
	m.setStatus("h1", model.StatusRunning)
	turn("restart it", "Restarted the dev server.", false, "npm run dev")
	nothing(t, home, 600*time.Millisecond)
	m.setStatus("h1", model.StatusIdle)
	if in := received(t, home, 2*time.Second); !strings.Contains(in.Text, "Restarted the dev server.") || !strings.Contains(in.Text, "still running in the background") {
		t.Errorf("notification:\n%s", in.Text)
	}

	// Stopped while its turn end is held (its work ends with its agent,
	// which drops its tasks first): the turn end is told then.
	turn("once more", "Running it again.", false, "npm run dev")
	nothing(t, home, 100*time.Millisecond)
	tasks()
	m.setStatus("a1", model.StatusStopped)
	if in := received(t, home, 2*time.Second); !strings.Contains(in.Text, "finished its turn. Its last message: \"Running it again.\"") {
		t.Errorf("notification:\n%s", in.Text)
	}
	nothing(t, home, 400*time.Millisecond)
	m.noteMu.Lock()
	defer m.noteMu.Unlock()
	if len(m.held) != 0 {
		t.Errorf("still held: %v", m.held)
	}
}

func TestHomeNotificationsCannotLoopOrFlood(t *testing.T) {
	fastNotes(t, 20*time.Millisecond, 150*time.Millisecond, 2)
	m := homeFixture(t)
	home := attach(t, m, "h1")
	tc := homeConn("alice", "h1")

	// Home conversations cannot be watched, by the tool or otherwise.
	if text, isErr := callOn(t, m, tc, "watch_session", map[string]any{"session": "h2"}); !isErr {
		t.Errorf("watched a Home conversation: %s", text)
	}
	if text, isErr := callOn(t, m, tc, "watch_session", map[string]any{"session": "t-bob-private"}); !isErr {
		t.Errorf("watched an invisible session: %s", text)
	}
	m.updateSession("h1", func(s *model.Session) { s.Watching = []string{"h1", "h2"} })
	m.setStatus("h2", model.StatusRunning)
	m.setStatus("h2", model.StatusIdle)
	m.setStatus("h1", model.StatusRunning)
	m.setStatus("h1", model.StatusIdle)
	nothing(t, home, 150*time.Millisecond)
	m.updateSession("h1", func(s *model.Session) { s.Watching = nil })

	// Notifications keep a gap, and after noteCap of them about one
	// session Collie stops watching it (and says so).
	if _, err := watchAs(m, "h1", "a1"); err != nil {
		t.Fatal(err)
	}
	finish := func() {
		m.setStatus("a1", model.StatusRunning)
		m.setStatus("a1", model.StatusIdle)
	}
	finish()
	received(t, home, 2*time.Second)
	first := time.Now()
	finish()
	in := received(t, home, 2*time.Second)
	if gap := time.Since(first); gap < 100*time.Millisecond {
		t.Errorf("second notification after %v, want about noteGap", gap)
	}
	if !strings.Contains(in.Text, "is no longer watched") {
		t.Errorf("cap not announced:\n%s", in.Text)
	}
	if h, _ := m.st.Session("h1"); slices.Contains(h.Watching, "a1") {
		t.Error("still watching after the cap")
	}
	finish()
	nothing(t, home, 300*time.Millisecond)

	// An archived Home conversation is not woken.
	if _, err := watchAs(m, "h1", "a1"); err != nil {
		t.Fatal(err)
	}
	m.updateSession("h1", func(s *model.Session) { s.Archived = true })
	finish()
	nothing(t, home, 300*time.Millisecond)
}

func TestHomeStartSessionWatchesByDefault(t *testing.T) {
	fastNotes(t, 20*time.Millisecond, 0, 30)
	m := homeFixture(t)
	home := attach(t, m, "h1")
	tc := homeConn("alice", "h1")

	text, isErr := callOn(t, m, tc, "start_session", map[string]any{"project": "alpha", "prompt": "fix the build"})
	if isErr {
		t.Fatalf("start_session: %s", text)
	}
	var out struct {
		ID      string `json:"id"`
		Watched bool   `json:"watched"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.ID == "" || !out.Watched {
		t.Fatalf("start_session result: %s", text)
	}
	if h, _ := m.st.Session("h1"); !slices.Contains(h.Watching, out.ID) {
		t.Errorf("not watching the new session: %v", h.Watching)
	}
	// There is no podman in the tests, so its agent fails to start: Home
	// hears of it.
	in := received(t, home, 5*time.Second)
	if !strings.Contains(in.Text, "fix the build") || !strings.Contains(in.Text, "stopped with an error") || !strings.Contains(in.Text, "podman") {
		t.Errorf("notification:\n%s", in.Text)
	}

	text, _ = callOn(t, m, tc, "start_session", map[string]any{"project": "alpha", "prompt": "quietly", "watch": false})
	_ = json.Unmarshal([]byte(text), &out)
	if h, _ := m.st.Session("h1"); slices.Contains(h.Watching, out.ID) || strings.Contains(text, "watched") {
		t.Errorf("watch: false still watches: %s", text)
	}
	// Its agent fails too (wait for that, so nothing writes after the
	// test), and Home is not told.
	deadline := time.Now().Add(5 * time.Second)
	for s, _ := m.st.Session(out.ID); s.Status != model.StatusError && time.Now().Before(deadline); s, _ = m.st.Session(out.ID) {
		time.Sleep(20 * time.Millisecond)
	}
	nothing(t, home, 300*time.Millisecond)

	// Without knowing the conversation, nothing can be watched.
	if text, isErr := callOn(t, m, homeConn("alice", ""), "watch_session", map[string]any{"session": "a1"}); !isErr {
		t.Errorf("watched without a conversation: %s", text)
	}
}

// A notification to a Home conversation whose agent is not running starts
// it the way a message from the user does (runtime.startDriver), so the
// user's instructions for all sessions reach it.
func TestNotificationStartsHomeLikeAMessage(t *testing.T) {
	fastNotes(t, 20*time.Millisecond, 0, 30)
	m := homeFixture(t)
	m.SetAccess(allAdmins{fakeAccess{"team": {"alice": "owner", "bob": "member"}}})
	log := filepath.Join(t.TempDir(), "started")
	m.cfg.ClaudeBin = fakeAgent(t, "claude", log)
	home, _ := m.st.UpdateProject("ha", func(p *model.Project) { p.Runtime = "host" })
	m.st.UpdateSession("h1", func(s *model.Session) { s.Agent, s.Cwd, s.Status = "claude", home.Path, model.StatusStopped })
	if _, err := m.PatchSessionPrefs("alice", SessionPrefsPatch{Instructions: ptr("ALICE: keep it short")}); err != nil {
		t.Fatal(err)
	}
	if _, err := watchAs(m, "h1", "a1"); err != nil {
		t.Fatal(err)
	}
	m.setStatus("a1", model.StatusRunning)
	m.setStatus("a1", model.StatusIdle)
	// The fake agent records its command line and fails.
	deadline := time.Now().Add(10 * time.Second)
	for s, _ := m.st.Session("h1"); s.Status != model.StatusError; s, _ = m.st.Session("h1") {
		if time.Now().After(deadline) {
			t.Fatalf("Home's agent was not started (status %s)", s.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the agent did not run: %v", err)
	}
	if !strings.Contains(string(b), "--append-system-prompt\nALICE: keep it short\n") || !strings.Contains(string(b), "mcp__collie__read_diff") {
		t.Errorf("Home's agent started with:\n%s", b)
	}
	items, _, _ := m.Items(Actor{ID: "alice"}, "h1", 0, 10)
	if !slices.ContainsFunc(items, func(it *model.Item) bool { return it.Via == "collie" && len(it.Notes) == 1 }) {
		t.Error("the notification is not on Home's timeline")
	}
}

func TestHomeListsTheNewToolsWithTheOthers(t *testing.T) {
	m := homeFixture(t)
	var names []string
	for _, x := range rpc(t, m, "alice", "tools/list", nil)["result"].(map[string]any)["tools"].([]any) {
		d := x.(map[string]any)
		names = append(names, d["name"].(string))
		if d["name"] == "start_session" {
			props := d["inputSchema"].(map[string]any)["properties"].(map[string]any)
			if props["watch"] == nil || props["model"] == nil {
				t.Errorf("start_session's parameters: %v", props)
			}
		}
	}
	for _, want := range []string{"start_session", "create_schedule", "read_diff", "watch_session", "unwatch_session", "stop_session", "archive_session", "show_media"} {
		if !slices.Contains(names, want) {
			t.Errorf("tool %s missing: %v", want, names)
		}
	}
	if !slices.Contains(homeReadTools, "read_diff") || slices.Contains(homeReadTools, "stop_session") {
		t.Errorf("tools allowed unasked: %v", homeReadTools)
	}
}

func TestUserCanStopHomeWatching(t *testing.T) {
	m := homeFixture(t)
	if _, err := watchAs(m, "h1", "a1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Unwatch(Actor{ID: "bob"}, "h1", "a1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob unwatching alice's Home: %v", err)
	}
	if err := m.Unwatch(Actor{ID: "alice"}, "h1", "a1"); err != nil {
		t.Fatal(err)
	}
	if h, _ := m.st.Session("h1"); len(h.Watching) != 0 {
		t.Errorf("still watching %v", h.Watching)
	}
}

func TestWatchListIsBounded(t *testing.T) {
	m := homeFixture(t)
	for i := range maxWatching {
		id := fmt.Sprintf("w%02d", i)
		m.st.PutSession(model.Session{ID: id, ProjectID: "pa", Owner: "alice", Title: id})
		if err := m.watch("h1", id); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.watch("h1", "a1"); err == nil {
		t.Fatal("watched more than maxWatching sessions")
	}
	// Archived and deleted sessions make room.
	m.updateSession("w00", func(s *model.Session) { s.Archived = true })
	m.st.DeleteSession("w01")
	if err := m.watch("h1", "a1"); err != nil {
		t.Fatal(err)
	}
	h, _ := m.st.Session("h1")
	if len(h.Watching) != maxWatching-1 || slices.Contains(h.Watching, "w00") || !slices.Contains(h.Watching, "a1") {
		t.Errorf("watching %v", h.Watching)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestReadDiffShowsTheChangesTab(t *testing.T) {
	m := homeFixture(t)
	gitInProcess(m)
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q")
	write := func(name, text string) {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o755)
		if err := os.WriteFile(filepath.Join(repo, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main\n")
	write("docs/readme.md", "hello\n")
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-qm", "init")
	base := gitutil.HeadCommit(repo)
	write("docs/readme.md", "hello world\n")
	gitIn(t, repo, "commit", "-qam", "say more")
	write("main.go", "package main\n\nfunc main() {}\n")
	write("web/src/app.ts", "export {}\n")
	m.st.PutSession(model.Session{ID: "d1", ProjectID: "pa", Owner: "alice", Title: "diffy", Cwd: repo, Base: base, Worktree: true, Branch: "collie/d1"})
	m.st.PutSession(model.Session{ID: "d-bob", ProjectID: "pt", Owner: "bob", Title: "bob's", Cwd: repo})

	alice := homeConn("alice", "h1")
	text, isErr := callOn(t, m, alice, "read_diff", map[string]any{"session": "d1"})
	if isErr {
		t.Fatalf("read_diff: %s", text)
	}
	for _, want := range []string{"say more", "M  main.go  +2 -0", "docs/readme.md", "?  web/src/app.ts  (new, untracked) +1", "+func main() {}", "+hello world", "+export {}"} {
		if !strings.Contains(text, want) {
			t.Errorf("read_diff lacks %q:\n%s", want, text)
		}
	}
	text, _ = callOn(t, m, alice, "read_diff", map[string]any{"session": "d1", "files": []string{"*.ts", "docs/"}})
	if strings.Contains(text, "func main") || !strings.Contains(text, "export {}") || !strings.Contains(text, "hello world") || !strings.Contains(text, "Files (2 of 3 changed)") {
		t.Errorf("filtered read_diff:\n%s", text)
	}
	text, _ = callOn(t, m, alice, "read_diff", map[string]any{"session": "d1", "files": "nosuch.go"})
	if !strings.Contains(text, "None of the 3 changed files") {
		t.Errorf("no match:\n%s", text)
	}

	// The same rights as the Changes tab: bob's private session is not
	// visible to alice, and Home conversations are not project sessions.
	for _, sid := range []string{"d-bob", "h2"} {
		if text, isErr := callOn(t, m, alice, "read_diff", map[string]any{"session": sid}); !isErr {
			t.Errorf("read_diff of %s: %s", sid, text)
		}
	}
}

func TestReadDiffCutsLongDiffsShort(t *testing.T) {
	big := func(path string, lines int) gitutil.FileChange {
		var b strings.Builder
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n@@ -0,0 +1,%d @@\n", path, path, lines)
		for i := range lines {
			fmt.Fprintf(&b, "+line %d of %s\n", i, path)
		}
		return gitutil.FileChange{Path: path, Status: "M", Diff: b.String()}
	}
	ch := &gitutil.Changes{Repo: true, Branch: "main", Files: []gitutil.FileChange{
		big("package-lock.json", 5000), big("a.go", 10), big("b.go", 400), big("c.go", 400), {Path: "logo.png", Status: "M", Binary: true},
	}}
	s := model.Session{ID: "s1", Title: "t"}
	out := formatDiff(s, "proj", ch, nil, 6000)
	if len(out) > 6600 {
		t.Errorf("answer is %d characters, asked for about 6000", len(out))
	}
	for _, want := range []string{"M  package-lock.json  +5000 -0", "logo.png  (binary)", "+line 9 of a.go", "Cut short", "Shortened: package-lock.json", "Left out:", "c.go", "`files`"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "line 4999 of package-lock.json") {
		t.Error("a long file's diff was not shortened")
	}
	// Everything fits: nothing is said to be cut.
	if out := formatDiff(s, "proj", &gitutil.Changes{Repo: true, Files: []gitutil.FileChange{big("a.go", 10)}}, nil, 0); strings.Contains(out, "Cut short") {
		t.Errorf("small diff said to be cut:\n%s", out)
	}
	if out := formatDiff(s, "proj", &gitutil.Changes{}, nil, 0); !strings.Contains(out, "not a git repository") {
		t.Errorf("no repo: %s", out)
	}
}

func TestReadDiffFileFilter(t *testing.T) {
	cases := []struct {
		pats []string
		path string
		want bool
	}{
		{nil, "x/y.go", true},
		{[]string{"x/y.go"}, "x/y.go", true},
		{[]string{"./x/"}, "x/y.go", true},
		{[]string{"x"}, "xy/z.go", false},
		{[]string{"*.go"}, "deep/in/a.go", true},
		{[]string{"*.go"}, "a.got", false},
		{[]string{"internal/*.go"}, "internal/a/b.go", false},
		{[]string{"internal/**/*_test.go"}, "internal/a/b_test.go", true},
		{[]string{"internal/**/*_test.go"}, "internal/b_test.go", true},
		{[]string{"web/**"}, "web/src/a.ts", true},
		{[]string{"日本/*.md"}, "日本/メモ.md", true},
		{[]string{"a?.txt"}, "ab.txt", true},
		{[]string{"a+b.txt"}, "a+b.txt", true},
	}
	for _, c := range cases {
		if got := fileFilter(c.pats)(c.path); got != c.want {
			t.Errorf("fileFilter(%q)(%q) = %v", c.pats, c.path, got)
		}
	}
}
