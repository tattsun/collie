package session

import (
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/agent"
	"github.com/tattsun/collie/internal/model"
)

// running gives a session a fake agent process that, like a real one,
// reports its end to the session when it is closed.
func running(t *testing.T, m *Manager, sid string) *fakeDriver {
	t.Helper()
	r, err := m.runtime(sid)
	if err != nil {
		t.Fatal(err)
	}
	d := newFakeDriver()
	r.mu.Lock()
	r.gen++
	s := newSink(r, r.gen)
	r.cur, r.drv = s, d
	r.mu.Unlock()
	var once sync.Once
	d.onClose = func() { once.Do(func() { s.Exited(nil) }) }
	return d
}

func lastItem(t *testing.T, m *Manager, sid string) *model.Item {
	t.Helper()
	s, _ := m.st.Session(sid)
	items, _, err := m.Items(Actor{ID: s.Owner}, sid, 0, 100)
	if err != nil || len(items) == 0 {
		t.Fatalf("items of %s: %v %v", sid, items, err)
	}
	return items[len(items)-1]
}

func status(m *Manager, sid string) string {
	s, _ := m.st.Session(sid)
	return s.Status
}

// When Collie stops a project's containers, the sessions that were at work
// (a turn going on, or work in the background) say why, show up as
// needing the user, and the Home conversation watching them is told once.
// Idle ones just stop.
func TestStoppedContainersLeaveARecord(t *testing.T) {
	fastNotes(t, 100*time.Millisecond, 0, 30)
	fastBackground(t, 150*time.Millisecond, time.Hour)
	m := homeFixture(t)
	home := attach(t, m, "h1")
	for _, sid := range []string{"a1", "a2"} {
		if _, err := watchAs(m, "h1", sid); err != nil {
			t.Fatal(err)
		}
	}
	m.st.PutSession(model.Session{ID: "a3", ProjectID: "pa", Owner: "alice", Title: "idle", Status: model.StatusIdle})
	for _, sid := range []string{"a1", "a2", "a3"} {
		running(t, m, sid)
	}
	m.setStatus("a1", model.StatusRunning)
	// a2 ended its turn with work in the background: its turn end is held
	// back for Home (see holdBackground)...
	m.setStatus("a2", model.StatusRunning)
	m.updateSession("a2", func(s *model.Session) { s.Tasks = []model.Task{{ID: "t", Title: "npm run dev"}} })
	r2, _ := m.runtime("a2")
	r2.sinkPut(&model.Item{ID: "x2", Kind: model.KindText, Text: "The dev server is up.", Done: true})
	m.setStatus("a2", model.StatusIdle)
	nothing(t, home, 100*time.Millisecond)
	time.Sleep(50 * time.Millisecond) // the push of that turn's end
	var pushed []string
	var mu sync.Mutex
	m.Notify = func(s model.Session, ev string) { mu.Lock(); pushed = append(pushed, s.ID+":"+ev); mu.Unlock() }

	if err := m.StopProject(Actor{ID: "alice"}, "pa", false); err != nil {
		t.Fatal(err)
	}
	want := interruptedText(whyStop)
	for _, sid := range []string{"a1", "a2"} {
		if it := lastItem(t, m, sid); it.Kind != model.KindError || it.Code != "interrupted" || it.Text != want {
			t.Errorf("%s: last item %+v", sid, it)
		}
		if st := status(m, sid); st != model.StatusError {
			t.Errorf("%s: status %s", sid, st)
		}
	}
	if st := status(m, "a3"); st != model.StatusStopped {
		t.Errorf("idle session: status %s", st)
	}
	if items, _, _ := m.Items(Actor{ID: "alice"}, "a3", 0, 10); len(items) != 0 {
		t.Errorf("idle session got %+v", items)
	}
	// ...and Home hears of both once, as stopped, not also as finished.
	in := received(t, home, 2*time.Second)
	if strings.Count(in.Text, "stopped with an error") != 2 || !strings.Contains(in.Text, want) || strings.Contains(in.Text, "finished its turn") {
		t.Errorf("notification:\n%s", in.Text)
	}
	nothing(t, home, 400*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	slices.Sort(pushed)
	if !slices.Equal(pushed, []string{"a1:error", "a2:error"}) {
		t.Errorf("pushed %v", pushed)
	}
	mu.Unlock()
}

// While Collie shuts down, sessions keep their status; when it starts
// again, those that were at work say so, and are told about like failures
// once the server is set up.
func TestRestartLeavesARecord(t *testing.T) {
	fastNotes(t, 20*time.Millisecond, 0, 30)
	m := homeFixture(t)
	d := running(t, m, "a1")
	m.setStatus("a1", model.StatusRunning)
	m.shutdown.Store(true)
	d.Close()
	if st := status(m, "a1"); st != model.StatusRunning {
		t.Fatalf("status after the shutdown: %s", st)
	}
	m.st.UpdateSession("a2", func(s *model.Session) { s.Tasks = []model.Task{{ID: "t", Title: "build"}} })
	m.st.UpdateSession("t-alice", func(s *model.Session) { s.Status = model.StatusWaiting })

	// The next start.
	m.shutdown.Store(false)
	m.rts = map[string]*Runtime{}
	m.restarted = m.interruptedByRestart()
	slices.Sort(m.restarted)
	if !slices.Equal(m.restarted, []string{"a1", "a2", "t-alice"}) {
		t.Fatalf("cut off: %v", m.restarted)
	}
	for _, sid := range m.restarted {
		s, _ := m.st.Session(sid)
		if it := lastItem(t, m, sid); s.Status != model.StatusError || len(s.Tasks) != 0 || it.Text != interruptedText(whyRestart) || it.Code != "interrupted" {
			t.Errorf("%s: %+v, last item %+v", sid, s, it)
		}
	}
	if st := status(m, "h1"); st != model.StatusIdle && st != model.StatusStopped {
		t.Errorf("idle Home: %s", st)
	}

	home := attach(t, m, "h1")
	if _, err := watchAs(m, "h1", "a1"); err != nil {
		t.Fatal(err)
	}
	var pushed []string
	var mu sync.Mutex
	m.Notify = func(s model.Session, ev string) { mu.Lock(); pushed = append(pushed, s.ID+":"+ev); mu.Unlock() }
	m.ReportRestart()
	if in := received(t, home, 2*time.Second); !strings.Contains(in.Text, "stopped with an error") || !strings.Contains(in.Text, "Collie restarted") {
		t.Errorf("notification:\n%s", in.Text)
	}
	m.ReportRestart() // once only
	nothing(t, home, 200*time.Millisecond)
	mu.Lock()
	if len(pushed) != 3 {
		t.Errorf("pushed %v", pushed)
	}
	mu.Unlock()
}

// An agent killed by SIGKILL while its container's OOM kill count went up
// ran out of memory; other ends say what killed them.
func TestExitExplainsOutOfMemory(t *testing.T) {
	m, p, f := isolatedWithPodman(t, "running")
	m.st.PutSession(model.Session{ID: "s1", ProjectID: p.ID, Owner: "alice", Status: model.StatusRunning})
	f.set("oom", "1")
	f.set("inside", `case "$*" in
*memory.events*) printf 'low 0\nhigh 0\nmax 12\noom 1\noom_kill %s\noom_group_kill 0\n' "$(cat "$d/oom")"; exit 0 ;;
*memory.max*) echo 8589934592; exit 0 ;;
esac`)
	kill := &agent.ExitError{Code: 9, Signal: syscall.SIGKILL}
	text, code := m.explainExit("s1", kill, oomBase{}, 1)
	if code != "oom" || text != oomText(8<<30) || !strings.Contains(text, "(8 GB)") {
		t.Errorf("out of memory: %q %q", text, code)
	}
	// It was killed before this agent started.
	if text, code := m.explainExit("s1", kill, oomBase{gen: 1, n: 1}, 1); code != "" || text != kill.Error() {
		t.Errorf("an earlier OOM kill: %q %q", text, code)
	}
	if text, code := m.explainExit("s1", kill, oomBase{gen: 1, n: 0}, 1); code != "oom" {
		t.Errorf("an OOM kill since it started: %q %q", text, code)
	}
	// As the session tells it, when its agent ends.
	running(t, m, "s1")
	r, _ := m.runtime("s1")
	r.mu.Lock()
	s := r.cur
	r.mu.Unlock()
	s.Exited(kill)
	if it := lastItem(t, m, "s1"); it.Code != "oom" || it.Text != oomText(8<<30) || status(m, "s1") != model.StatusError {
		t.Errorf("session after an OOM kill: %s, %+v", status(m, "s1"), it)
	}
	term := &agent.ExitError{Code: 15, Signal: syscall.SIGTERM}
	if text, code := m.explainExit("s1", term, oomBase{}, 1); code != "" || text != "The agent process was killed by signal 15 (SIGTERM)." {
		t.Errorf("SIGTERM: %q %q", text, code)
	}
	f.set("state", "exited")
	if text, _ := m.explainExit("s1", kill, oomBase{gen: 1, n: 1}, 1); !strings.Contains(text, "container stopped") {
		t.Errorf("container gone: %q", text)
	}
	for n, want := range map[int64]string{4 << 30: "4 GB", 3 << 29: "1.5 GB", 512 << 20: "512 MB"} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// A container in use keeps its old settings until nothing is at work in
// it: then it stops (idle agents with it) and the next start makes a new
// one. Restart now does it at once, saying so in the sessions at work.
func TestPendingSettingsApplyWhenFree(t *testing.T) {
	m, p, f := isolatedWithPodman(t, "running")
	key := p.ID + "/alice"
	m.ctrEnsured[key] = time.Now().Add(-time.Minute) // not started just now
	m.st.PutSession(model.Session{ID: "s1", ProjectID: p.ID, Owner: "alice", Title: "work", Cwd: p.Path, Status: model.StatusIdle})
	m.st.PutSession(model.Session{ID: "s2", ProjectID: p.ID, Owner: "alice", Title: "idle", Cwd: p.Path, Status: model.StatusIdle})
	running(t, m, "s1")
	d2 := running(t, m, "s2")
	m.setStatus("s1", model.StatusRunning)
	terms := false
	m.TermsInUse = func(string, string) bool { return terms }

	pending := func() bool {
		p, _ := m.st.Project(p.ID)
		return slices.Contains(p.Pending, "alice")
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for start := time.Now(); !cond(); time.Sleep(10 * time.Millisecond) {
			if time.Since(start) > 5*time.Second {
				t.Fatalf("still not %s", what)
			}
		}
	}

	// Unchanged: nothing to do.
	m.applyPending(p, "alice")
	if pending() || f.count("stop") != 0 {
		t.Fatalf("pending %v without a change; log:\n%s", pending(), f.get("log"))
	}
	p, _ = m.st.UpdateProject(p.ID, func(x *model.Project) { x.Memory = "8g" })
	m.applyPending(p, "alice")
	if !pending() || f.count("stop") != 0 || f.get("state") != "running" {
		t.Fatalf("an agent at work: pending %v, state %s", pending(), f.get("state"))
	}
	if !m.sb.Stale(p.ID, "alice") {
		t.Error("the container is not reported outdated")
	}

	// The turn ends, but a terminal is open.
	terms = true
	m.setStatus("s1", model.StatusIdle)
	time.Sleep(200 * time.Millisecond)
	if !pending() || f.get("state") != "running" {
		t.Fatalf("with a terminal open: pending %v, state %s", pending(), f.get("state"))
	}
	// It closes: the container stops, and so do the idle agents, quietly.
	terms = false
	m.ApplyPending()
	waitFor("applied", func() bool { return !pending() && f.get("state") == "exited" })
	if r, _ := m.runtime("s2"); r.driver() != nil {
		t.Error("an idle agent kept running in the old container")
	}
	_ = d2
	for _, sid := range []string{"s1", "s2"} {
		if st := status(m, sid); st != model.StatusStopped {
			t.Errorf("%s: %s", sid, st)
		}
	}
	m.ctrMu.Lock()
	_, active := m.ctrActive[key]
	m.ctrMu.Unlock()
	if active {
		t.Error("the stopped container is still counted as running")
	}
	// The next start makes a new container.
	f.set("log", "")
	if err := m.ensureContainer(p, "alice", nil); err != nil {
		t.Fatal(err)
	}
	if f.count("rm") != 1 || f.count("run") != 1 || pending() {
		t.Errorf("next start: pending %v; log:\n%s", pending(), f.get("log"))
	}

	// Restart now: the agent at work stops and says why; terminals close.
	m.ctrEnsured[key] = time.Now().Add(-time.Minute)
	running(t, m, "s1")
	m.setStatus("s1", model.StatusRunning)
	p, _ = m.st.UpdateProject(p.ID, func(x *model.Project) { x.CPUs = "1" })
	m.applyPending(p, "alice")
	if !pending() {
		t.Fatal("not pending")
	}
	var closed []string
	m.CloseTerms = func(pid, uid string) { closed = append(closed, pid+"/"+uid) }
	if _, err := m.RestartContainers(Actor{ID: "carol"}, p.ID, ""); err == nil {
		t.Error("an outsider restarted the container")
	}
	users, err := m.RestartContainers(Actor{ID: "alice"}, p.ID, "alice")
	if err != nil || !slices.Equal(users, []string{"alice"}) {
		t.Fatalf("restart: %v %v", users, err)
	}
	if it := lastItem(t, m, "s1"); it.Text != interruptedText(whyApply) || status(m, "s1") != model.StatusError {
		t.Errorf("after the restart: %s, %+v", status(m, "s1"), it)
	}
	if pending() || f.get("state") != "exited" || !slices.Equal(closed, []string{key}) {
		t.Errorf("after the restart: pending %v, state %s, terminals closed %v", pending(), f.get("state"), closed)
	}
}

// Saving new limits leaves the agents at work alone (their container is
// pending); a new network setting can't wait and restarts them, saying so.
func TestSettingsWaitButTheNetworkCannot(t *testing.T) {
	m, p, f := isolatedWithPodman(t, "missing")
	p, _ = m.st.UpdateProject(p.ID, func(x *model.Project) { x.OwnerUser = "root" })
	root := Actor{ID: "root", Name: "root", Admin: true}
	if err := m.ensureContainer(p, "root", nil); err != nil {
		t.Fatal(err)
	}
	m.st.PutSession(model.Session{ID: "s1", ProjectID: p.ID, Owner: "root", Title: "work", Cwd: p.Path, Status: model.StatusIdle})
	d := running(t, m, "s1")
	closed := make(chan struct{})
	d.onClose = func() {
		r, _ := m.runtime("s1")
		r.mu.Lock()
		s := r.cur
		r.mu.Unlock()
		s.Exited(nil)
		close(closed)
	}
	m.setStatus("s1", model.StatusRunning)
	f.set("log", "")

	mem := "8g"
	if _, err := m.PatchProject(root, p.ID, PatchProjectReq{Memory: &mem}); err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); ; time.Sleep(10 * time.Millisecond) {
		if q, _ := m.st.Project(p.ID); slices.Contains(q.Pending, "root") {
			break
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("the container in use is not pending")
		}
	}
	select {
	case <-closed:
		t.Fatal("saving a memory limit stopped the agent at work")
	default:
	}
	if f.count("stop") != 0 || status(m, "s1") != model.StatusRunning {
		t.Errorf("status %s; log:\n%s", status(m, "s1"), f.get("log"))
	}

	allow := model.NetAllowlist
	if _, err := m.PatchProject(root, p.ID, PatchProjectReq{Network: &allow}); err != nil {
		t.Fatal(err)
	}
	<-closed
	if it := lastItem(t, m, "s1"); it.Text != interruptedText(whyNetwork) || status(m, "s1") != model.StatusError {
		t.Errorf("after the network change: %s, %+v", status(m, "s1"), it)
	}
	if q, _ := m.st.Project(p.ID); len(q.Pending) != 0 {
		t.Errorf("still pending: %v", q.Pending)
	}
}
