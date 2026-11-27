package session

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/store"
)

var tokyo, _ = time.LoadLocation("Asia/Tokyo")

// testClock is the manager's clock in tests; runs read it from their own
// goroutines while the test moves it.
type testClock struct{ ns atomic.Int64 }

func (c *testClock) now() time.Time      { return time.Unix(0, c.ns.Load()).In(tokyo) }
func (c *testClock) set(t time.Time)     { c.ns.Store(t.UnixNano()) }
func (c *testClock) add(d time.Duration) { c.ns.Add(int64(d)) }

// newScheduleTestManager has alice's isolated project "alpha", the team's
// "teamproj", and a clock the test sets.
func newScheduleTestManager(t *testing.T, at string) (*Manager, *testClock) {
	t.Helper()
	m := newHomeTestManager(t)
	m.st.PutProject(model.Project{ID: "pa", Name: "alpha", Path: t.TempDir(), OwnerUser: "alice", Runtime: "container"})
	m.st.PutProject(model.Project{ID: "pt", Name: "teamproj", Path: t.TempDir(), OwnerGroup: "team", Runtime: "container"})
	clk := &testClock{}
	clk.set(clockAt(t, at))
	m.clock = clk.now
	return m, clk
}

func clockAt(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", s, tokyo)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

var alice = Actor{ID: "alice", Name: "alice"}

func newSchedule(t *testing.T, m *Manager, a Actor, project, expr string) model.Schedule {
	t.Helper()
	sc, err := m.CreateSchedule(a, ScheduleReq{ProjectID: ptr(project), Prompt: ptr("check the dependencies"), Cron: ptr(expr), TZ: ptr("Asia/Tokyo")})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// waitRuns waits until none of the schedule's runs is still starting (a
// run's session fails on its own here: there is no podman).
func waitRuns(t *testing.T, m *Manager, id string) model.Schedule {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sc, _ := m.st.Schedule(id)
		running := false
		for _, r := range sc.Runs {
			running = running || r.Status == model.RunRunning
		}
		if !running {
			return sc
		}
		if time.Now().After(deadline) {
			t.Fatalf("runs still running: %+v", sc.Runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fmtMs(ms int64) string { return time.UnixMilli(ms).In(tokyo).Format("2006-01-02 15:04") }

func TestScheduleTimesValidation(t *testing.T) {
	from := clockAt(t, "2026-11-27 10:00")
	times, err := ScheduleTimes("0 18 * * 5", "Asia/Tokyo", from, 2)
	if err != nil || len(times) != 2 || fmtMs(times[0].UnixMilli()) != "2026-11-27 18:00" || fmtMs(times[1].UnixMilli()) != "2026-12-04 18:00" {
		t.Errorf("Fridays at 18:00: %v %v", times, err)
	}
	// The same wall-clock time elsewhere is another instant.
	times, _ = ScheduleTimes("0 9 * * *", "America/New_York", from, 1)
	if got := times[0].In(time.UTC).Format("15:04"); got != "14:00" {
		t.Errorf("9:00 in New York is %s UTC", got)
	}
	for expr, want := range map[string]error{
		"0 0 30 2 *":      errScheduleNever,
		"* * * * *":       errScheduleOften,
		"*/2 9 * * *":     errScheduleOften,
		"0,3 9 31 12 *":   errScheduleOften,
		"*/5 * * * *":     nil,
		"0 9 * * MON-FRI": nil,
	} {
		if _, err := ScheduleTimes(expr, "Asia/Tokyo", from, 1); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", expr, err, want)
		}
	}
	for _, tz := range []string{"", "Local", "Mars/Olympus"} {
		if _, err := ScheduleTimes("0 9 * * *", tz, from, 1); err == nil {
			t.Errorf("time zone %q accepted", tz)
		}
	}
	if _, err := ScheduleTimes("0 25 * * *", "Asia/Tokyo", from, 1); err == nil || !strings.HasPrefix(err.Error(), "invalid cron expression") {
		t.Errorf("bad hour: %v", err)
	}
}

func TestScheduleCreateAndEdit(t *testing.T) {
	m, now := newScheduleTestManager(t, "2026-11-27 08:00")
	sc := newSchedule(t, m, alice, "pa", "0 9 * * *")
	if sc.Owner != "alice" || sc.Agent != "claude" || sc.Mode == "" || fmtMs(sc.NextAt) != "2026-11-27 09:00" || sc.Shared {
		t.Fatalf("created %+v", sc)
	}
	// Pausing drops the planned time; resuming plans from now, not from
	// the times that passed meanwhile.
	sc, err := m.PatchSchedule(alice, sc.ID, ScheduleReq{Paused: ptr(true)})
	if err != nil || sc.NextAt != 0 || !sc.Paused {
		t.Fatalf("paused: %+v %v", sc, err)
	}
	now.set(clockAt(t, "2026-11-30 12:00"))
	if m.runDue(now.now()); len(m.Schedules("alice")[0].Runs) != 0 {
		t.Fatal("a paused schedule ran")
	}
	sc, _ = m.PatchSchedule(alice, sc.ID, ScheduleReq{Paused: ptr(false)})
	if fmtMs(sc.NextAt) != "2026-12-01 09:00" {
		t.Errorf("resumed: next %s", fmtMs(sc.NextAt))
	}
	// Changing the time re-plans; other fields keep it.
	sc, _ = m.PatchSchedule(alice, sc.ID, ScheduleReq{Cron: ptr("30  7 * * *")})
	if sc.Cron != "30 7 * * *" || fmtMs(sc.NextAt) != "2026-12-01 07:30" {
		t.Errorf("new time: %q next %s", sc.Cron, fmtMs(sc.NextAt))
	}
	sc, _ = m.PatchSchedule(alice, sc.ID, ScheduleReq{Agent: ptr("codex"), Title: ptr("  Deps  ")})
	if sc.Agent != "codex" || sc.Mode != "auto" || sc.Title != "Deps" || fmtMs(sc.NextAt) != "2026-12-01 07:30" {
		t.Errorf("switched agent: %+v", sc)
	}
	if _, err := m.PatchSchedule(alice, sc.ID, ScheduleReq{Mode: ptr("yolo")}); err == nil {
		t.Error("an unknown mode was accepted")
	}
	if _, err := m.PatchSchedule(alice, sc.ID, ScheduleReq{Prompt: ptr("  ")}); !errors.Is(err, errNoInstructions) {
		t.Errorf("empty instructions: %v", err)
	}
	if _, err := m.PatchSchedule(alice, sc.ID, ScheduleReq{Worktree: ptr(true)}); err == nil {
		t.Error("a worktree outside a git repository was accepted")
	}
	// A new schedule takes the time zone of the user's others.
	sc2, err := m.CreateSchedule(alice, ScheduleReq{ProjectID: ptr("pt"), Prompt: ptr("x"), Cron: ptr("0 9 * * *")})
	if err != nil || sc2.TZ != "Asia/Tokyo" || !sc2.Shared {
		t.Errorf("second schedule: %+v %v", sc2, err)
	}
}

func TestScheduleAccess(t *testing.T) {
	m, clk := newScheduleTestManager(t, "2026-11-27 08:00")
	bob := Actor{ID: "bob", Name: "bob"}
	mine := newSchedule(t, m, alice, "pa", "0 9 * * *")
	team := newSchedule(t, m, alice, "pt", "0 18 * * 5")

	// Nobody else sees or touches alice's schedules, in her project or the group's.
	if got := m.Schedules("bob"); len(got) != 0 {
		t.Errorf("bob sees %v", got)
	}
	for _, id := range []string{mine.ID, team.ID} {
		if _, err := m.PatchSchedule(bob, id, ScheduleReq{Paused: ptr(true)}); !errors.Is(err, ErrNotFound) {
			t.Errorf("bob pausing %s: %v", id, err)
		}
		if _, err := m.RunScheduleNow(bob, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("bob running %s: %v", id, err)
		}
		if err := m.DeleteSchedule(bob, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("bob deleting %s: %v", id, err)
		}
		if err := m.DeleteSchedule(Actor{ID: "root", Admin: true}, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("an admin deleting %s: %v", id, err)
		}
	}
	if text, _ := call(t, m, "bob", "list_schedules", map[string]any{}); strings.Contains(text, mine.ID) || strings.Contains(text, team.ID) {
		t.Errorf("bob's Home lists alice's schedules: %s", text)
	}
	if _, isErr := call(t, m, "bob", "delete_schedule", map[string]any{"schedule": team.ID}); !isErr {
		t.Error("bob's Home deleted alice's schedule")
	}
	// Schedules go only where one could start a session.
	if _, err := m.CreateSchedule(bob, ScheduleReq{ProjectID: ptr("pa"), Prompt: ptr("x"), Cron: ptr("0 9 * * *"), TZ: ptr("UTC")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob scheduling in alice's project: %v", err)
	}
	if _, err := m.PatchSchedule(alice, mine.ID, ScheduleReq{ProjectID: ptr("nope")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("moving to an unknown project: %v", err)
	}
	m.st.PutProject(model.Project{ID: "ph", Name: "hostproj", Path: t.TempDir(), OwnerGroup: "team", Runtime: "host"})
	if _, err := m.CreateSchedule(bob, ScheduleReq{ProjectID: ptr("ph"), Prompt: ptr("x"), Cron: ptr("0 9 * * *"), TZ: ptr("UTC")}); !errors.Is(err, errHostOnly) {
		t.Errorf("a member scheduling on the host: %v", err)
	}
	if _, err := m.CreateSchedule(bob, ScheduleReq{ProjectID: ptr("pt"), Prompt: ptr("x"), Cron: ptr("0 9 * * *"), TZ: ptr("UTC")}); err != nil {
		t.Errorf("bob scheduling in the group's project: %v", err)
	}
	if len(m.Schedules("bob")) != 1 || len(m.Schedules("alice")) != 2 {
		t.Errorf("schedules: bob %d, alice %d", len(m.Schedules("bob")), len(m.Schedules("alice")))
	}

	// A run whose owner lost access to the project fails instead of running.
	m.acc = fakeAccess{"team": {"bob": "owner"}}
	clk.set(clockAt(t, "2026-11-27 18:00"))
	m.runDue(clk.now())
	waitRuns(t, m, mine.ID)
	sc := waitRuns(t, m, team.ID)
	if len(sc.Runs) != 1 || sc.Runs[0].Status != model.RunError || sc.Runs[0].SessionID != "" || !strings.Contains(sc.Runs[0].Error, "no longer have access") {
		t.Errorf("run without access: %+v", sc.Runs)
	}
	// ...and so does a disabled user's (skipped).
	m.UserActive = func(uid string) bool { return uid != "alice" }
	clk.set(clockAt(t, "2026-11-28 09:00"))
	m.runDue(clk.now())
	if sc, _ := m.st.Schedule(mine.ID); len(sc.Runs) == 0 || sc.Runs[0].Status != model.RunSkipped {
		t.Errorf("disabled user's run: %+v", sc.Runs)
	}

	// Deleting the project or the user deletes their schedules.
	m.deleteProject("pt")
	if len(m.Schedules("bob")) != 0 || len(m.Schedules("alice")) != 1 {
		t.Errorf("after deleting the project: bob %d, alice %d", len(m.Schedules("bob")), len(m.Schedules("alice")))
	}
	m.PurgeUser("alice")
	if len(m.Schedules("alice")) != 0 {
		t.Error("a deleted user's schedules are left")
	}
}

func TestScheduleMissedTimesRunOnce(t *testing.T) {
	m, now := newScheduleTestManager(t, "2026-11-27 08:00")
	sc := newSchedule(t, m, alice, "pa", "0 9 * * *")

	// On time: one run.
	now.set(clockAt(t, "2026-11-27 09:00"))
	if next := m.runDue(now.now()); fmtMs(next.UnixMilli()) != "2026-11-28 09:00" {
		t.Errorf("next planned: %s", fmtMs(next.UnixMilli()))
	}
	got := waitRuns(t, m, sc.ID)
	if len(got.Runs) != 1 || got.Runs[0].Trigger != model.RunScheduled || got.Runs[0].Missed != 0 || got.Runs[0].SessionID == "" {
		t.Fatalf("on time: %+v", got.Runs)
	}
	// Its session is marked, and its first message is from the schedule.
	s, ok := m.st.Session(got.Runs[0].SessionID)
	if !ok || s.Schedule != sc.ID || s.Owner != "alice" || s.ProjectID != "pa" {
		t.Fatalf("session %+v", s)
	}
	items, _, _ := m.Items(alice, s.ID, 0, 10)
	if len(items) == 0 || items[0].Via != "schedule" || items[0].Text != "check the dependencies" {
		t.Errorf("first message: %+v", items)
	}
	// It failed on its own (no podman here), and the run says why.
	if got.Runs[0].Status != model.RunError || !strings.Contains(got.Runs[0].Error, "podman") {
		t.Errorf("run without podman: %+v", got.Runs[0])
	}

	// Collie was down for three planned times: they make one run.
	now.set(clockAt(t, "2026-12-01 10:30"))
	m.runDue(now.now())
	got = waitRuns(t, m, sc.ID)
	if len(got.Runs) != 2 {
		t.Fatalf("after the gap: %d runs", len(got.Runs))
	}
	if r := got.Runs[0]; r.Trigger != model.RunCatchUp || r.Missed != 4 || fmtMs(r.Planned) != "2026-11-28 09:00" {
		t.Errorf("catch-up: %+v (planned %s)", r, fmtMs(r.Planned))
	}
	if fmtMs(got.NextAt) != "2026-12-02 09:00" {
		t.Errorf("next after the gap: %s", fmtMs(got.NextAt))
	}
	// Nothing more until then.
	now.set(clockAt(t, "2026-12-02 08:59"))
	if m.runDue(now.now()); len(waitRuns(t, m, sc.ID).Runs) != 2 {
		t.Error("ran again before the next planned time")
	}
	// A single planned time missed by more than a moment is a catch-up too.
	now.set(clockAt(t, "2026-12-02 09:20"))
	m.runDue(now.now())
	if r := waitRuns(t, m, sc.ID).Runs[0]; r.Trigger != model.RunCatchUp || r.Missed != 0 {
		t.Errorf("late run: %+v", r)
	}
}

// putRun pretends a run of sc is under way in a session.
func putRun(m *Manager, sc model.Schedule, sid, status string) {
	m.st.PutSession(model.Session{ID: sid, ProjectID: sc.ProjectID, Owner: sc.Owner, Agent: "claude", Status: status, Schedule: sc.ID})
	m.addRun(sc.ID, model.ScheduleRun{Trigger: model.RunScheduled, At: m.now().UnixMilli(), SessionID: sid, Status: model.RunRunning})
}

func TestScheduleRunsNeverOverlap(t *testing.T) {
	m, now := newScheduleTestManager(t, "2026-11-27 08:00")
	sc := newSchedule(t, m, alice, "pa", "0 * * * *")
	putRun(m, sc, "s1", model.StatusRunning)

	for _, at := range []string{"2026-11-27 08:00", "2026-11-27 09:00", "2026-11-27 10:00"} {
		now.set(clockAt(t, at))
		m.runDue(now.now())
	}
	got, _ := m.st.Schedule(sc.ID)
	// One entry for the skips (folded), then the run still going.
	if len(got.Runs) != 2 || got.Runs[0].Status != model.RunSkipped || got.Runs[0].Count != 2 || got.Runs[1].Status != model.RunRunning {
		t.Fatalf("while running: %+v", got.Runs)
	}
	if _, err := m.RunScheduleNow(alice, sc.ID); !errors.Is(err, errScheduleBusy) {
		t.Errorf("run now while running: %v", err)
	}
	if fmtMs(got.NextAt) != "2026-11-27 11:00" {
		t.Errorf("skipped times still move on: next %s", fmtMs(got.NextAt))
	}

	// Waiting for the user, or background work left, is still running.
	for _, s := range []model.Session{{Status: model.StatusWaiting}, {Status: model.StatusIdle, Tasks: []model.Task{{ID: "t"}}}} {
		m.updateSession("s1", func(x *model.Session) { x.Status, x.Tasks = s.Status, s.Tasks })
		if !m.settle(sc.ID, false) {
			t.Errorf("not busy while %s with %d tasks", s.Status, len(s.Tasks))
		}
	}
	// The turn is over: the run ends, and the next time runs.
	m.updateSession("s1", func(x *model.Session) { x.Tasks = nil })
	got, _ = m.st.Schedule(sc.ID)
	if r := got.Runs[1]; r.Status != model.RunOK || r.EndedAt == 0 {
		t.Errorf("finished run: %+v", r)
	}
	now.set(clockAt(t, "2026-11-27 11:00"))
	m.runDue(now.now())
	got = waitRuns(t, m, sc.ID)
	if len(got.Runs) != 3 || got.Runs[0].Trigger != model.RunScheduled || got.Runs[0].SessionID == "" {
		t.Errorf("after the run ended: %+v", got.Runs)
	}

	// Run now works when nothing runs (paused too), and leaves the plan alone.
	m.PatchSchedule(alice, sc.ID, ScheduleReq{Paused: ptr(true)})
	run, err := m.RunScheduleNow(alice, sc.ID)
	got = waitRuns(t, m, sc.ID)
	if run.Trigger != model.RunManual || run.SessionID == "" || got.Runs[0].ID != run.ID || got.NextAt != 0 {
		t.Errorf("run now: %+v %v; %+v", run, err, got)
	}
}

func TestScheduleRunOutcomes(t *testing.T) {
	m, _ := newScheduleTestManager(t, "2026-11-27 08:00")
	sc := newSchedule(t, m, alice, "pa", "0 9 * * *")
	status := func(sid string) model.ScheduleRun {
		got, _ := m.st.Schedule(sc.ID)
		for _, r := range got.Runs {
			if r.SessionID == sid {
				return r
			}
		}
		t.Fatalf("no run for %s", sid)
		return model.ScheduleRun{}
	}
	set := func(sid, st string) { m.updateSession(sid, func(x *model.Session) { x.Status = st }) }

	// A turn that ends well; errors on the way it got past don't count.
	putRun(m, sc, "ok", model.StatusStopped)
	set("ok", model.StatusStarting)
	set("ok", model.StatusRunning)
	m.noteRunItem("ok", &model.Item{Kind: model.KindError, Text: "a hiccup"})
	if r := status("ok"); r.Status != model.RunRunning || r.Error != "a hiccup" {
		t.Errorf("mid-turn: %+v", r)
	}
	m.noteRunItem("ok", &model.Item{Kind: model.KindResult, Result: &model.Result{}})
	set("ok", model.StatusIdle)
	if r := status("ok"); r.Status != model.RunOK || r.Error != "" {
		t.Errorf("good turn: %+v", r)
	}
	// Later turns of the same session don't touch the run.
	set("ok", model.StatusRunning)
	set("ok", model.StatusError)
	if r := status("ok"); r.Status != model.RunOK {
		t.Errorf("a later turn changed the run: %+v", r)
	}

	// A turn that ends in an error.
	putRun(m, sc, "bad", model.StatusRunning)
	m.noteRunItem("bad", &model.Item{Kind: model.KindResult, Text: "usage limit reached\nmore", Result: &model.Result{IsError: true}})
	set("bad", model.StatusIdle)
	if r := status("bad"); r.Status != model.RunError || r.Error != "usage limit reached" {
		t.Errorf("failed turn: %+v", r)
	}
	// Stopped before the turn ended.
	putRun(m, sc, "cut", model.StatusRunning)
	set("cut", model.StatusStopped)
	if r := status("cut"); r.Status != model.RunError || r.Error == "" {
		t.Errorf("stopped mid-turn: %+v", r)
	}
	// The agent crashed.
	putRun(m, sc, "crash", model.StatusRunning)
	set("crash", model.StatusError)
	if r := status("crash"); r.Status != model.RunError {
		t.Errorf("crashed: %+v", r)
	}
	// The session was deleted while it ran.
	putRun(m, sc, "gone", model.StatusRunning)
	if err := m.deleteSession("gone", false); err != nil {
		t.Fatal(err)
	}
	if m.settle(sc.ID, false) {
		t.Error("a deleted session is still running")
	}
	if r := status("gone"); r.Status != model.RunError {
		t.Errorf("deleted: %+v", r)
	}

	// Collie restarted under two runs: one had finished its turn (only
	// background work was left), the other had not.
	putRun(m, sc, "done", model.StatusIdle)
	m.noteRunItem("done", &model.Item{Kind: model.KindResult, Result: &model.Result{}})
	m.updateSession("done", func(x *model.Session) { x.Tasks = []model.Task{{ID: "bg"}} })
	putRun(m, sc, "mid", model.StatusRunning)
	for _, sid := range []string{"done", "mid"} {
		m.st.UpdateSession(sid, func(x *model.Session) { x.Status, x.Tasks = model.StatusStopped, nil })
	}
	m.settleAll()
	if r := status("done"); r.Status != model.RunOK {
		t.Errorf("restart after the turn: %+v", r)
	}
	if r := status("mid"); r.Status != model.RunError || !strings.Contains(r.Error, "restarted") {
		t.Errorf("restart mid-turn: %+v", r)
	}
}

func TestScheduleHistoryIsBounded(t *testing.T) {
	m, _ := newScheduleTestManager(t, "2026-11-27 08:00")
	sc := newSchedule(t, m, alice, "pa", "0 9 * * *")
	for i := range maxScheduleRuns + 5 {
		m.addRun(sc.ID, model.ScheduleRun{Status: model.RunOK, At: int64(i)})
	}
	got, _ := m.st.Schedule(sc.ID)
	if len(got.Runs) != maxScheduleRuns || got.Runs[0].At != maxScheduleRuns+4 {
		t.Errorf("%d runs, newest %d", len(got.Runs), got.Runs[0].At)
	}
}

func TestSchedulesPersist(t *testing.T) {
	m, _ := newScheduleTestManager(t, "2026-11-27 08:00")
	sc := newSchedule(t, m, alice, "pa", "0 9 * * *")
	m.addRun(sc.ID, model.ScheduleRun{Status: model.RunOK, SessionID: "s1"})
	if err := m.st.Flush(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(m.st.Dir())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := st.Schedule(sc.ID)
	if !ok || got.Cron != "0 9 * * *" || got.NextAt != sc.NextAt || len(got.Runs) != 1 || got.Runs[0].SessionID != "s1" {
		t.Errorf("reloaded %+v", got)
	}
}

func TestHomeScheduleTools(t *testing.T) {
	m, _ := newScheduleTestManager(t, "2026-11-27 10:00")
	m.st.PutProject(model.Project{ID: "ha", Name: "Home", Path: t.TempDir(), OwnerUser: "alice", Home: true, Runtime: "container"})
	out := rpc(t, m, "alice", "tools/list", nil)
	b, _ := json.Marshal(out)
	for _, name := range []string{"create_schedule", "list_schedules", "delete_schedule"} {
		if !strings.Contains(string(b), `"`+name+`"`) {
			t.Errorf("Home tools lack %s", name)
		}
	}
	text, isErr := call(t, m, "alice", "create_schedule", map[string]any{
		"project": "alpha", "prompt": "Sum up this week's merges", "cron": "0 18 * * 5", "timezone": "Asia/Tokyo", "title": "Weekly"})
	if isErr || !strings.Contains(text, "Fri 2026-11-27 18:00 JST") {
		t.Fatalf("create: %s", text)
	}
	list := m.Schedules("alice")
	if len(list) != 1 || list[0].Title != "Weekly" || list[0].Mode != "auto" {
		t.Fatalf("created %+v", list)
	}
	// Here in Home, too.
	if text, isErr := call(t, m, "alice", "create_schedule", map[string]any{"project": "home", "prompt": "x", "cron": "0 9 * * 1"}); isErr {
		t.Errorf("Home schedule: %s", text)
	}
	if sc := m.Schedules("alice"); len(sc) != 2 || !slices.ContainsFunc(sc, func(x model.Schedule) bool { return x.ProjectID == "ha" }) {
		t.Errorf("Home schedule went to %+v", sc)
	}
	// Not without permission checks, and not too often.
	if _, isErr := call(t, m, "alice", "create_schedule", map[string]any{"project": "alpha", "prompt": "x", "cron": "0 9 * * *", "mode": "bypassPermissions"}); !isErr {
		t.Error("Home scheduled a session without permission checks")
	}
	if text, isErr := call(t, m, "alice", "create_schedule", map[string]any{"project": "alpha", "prompt": "x", "cron": "* * * * *"}); !isErr || !strings.Contains(text, "5 minutes") {
		t.Errorf("every minute: %s", text)
	}
	if text, _ := call(t, m, "alice", "list_schedules", map[string]any{}); !strings.Contains(text, list[0].ID) || !strings.Contains(text, "0 18 * * 5") {
		t.Errorf("list: %s", text)
	}
	if text, isErr := call(t, m, "alice", "delete_schedule", map[string]any{"schedule": list[0].ID}); isErr {
		t.Errorf("delete: %s", text)
	}
	if len(m.Schedules("alice")) != 1 {
		t.Errorf("after delete: %+v", m.Schedules("alice"))
	}
	if !strings.Contains(homeInstructions, "create_schedule") {
		t.Error("Home's instructions don't mention schedules")
	}
}

func TestScheduleUsesTheOwnersDefaults(t *testing.T) {
	m, now := newScheduleTestManager(t, "2026-11-27 08:00")
	if _, err := m.PatchSessionPrefs("alice", SessionPrefsPatch{
		Agents:       map[string]AgentDefaults{"claude": {Model: "opus", Effort: "high"}},
		Instructions: ptr("Write PR titles in English."),
	}); err != nil {
		t.Fatal(err)
	}
	// Left empty: the owner's defaults at the time of each run.
	sc := newSchedule(t, m, alice, "pa", "0 9 * * *")
	if sc.Model != "" || sc.Effort != "" {
		t.Fatalf("stored %q/%q, want the defaults left open", sc.Model, sc.Effort)
	}
	runSession := func() model.Session {
		t.Helper()
		now.add(24 * time.Hour)
		m.runDue(now.now())
		got := waitRuns(t, m, sc.ID)
		s, ok := m.st.Session(got.Runs[0].SessionID)
		if !ok {
			t.Fatalf("no session for %+v", got.Runs[0])
		}
		return s
	}
	if s := runSession(); s.Model != "opus" || s.Effort != "high" || s.Owner != "alice" {
		t.Errorf("run with the defaults: %s/%s owned by %s", s.Model, s.Effort, s.Owner)
	}
	// It follows changed defaults; the session runs for the schedule's owner,
	// so their instructions reach its agent (Runtime.startDriver).
	m.PatchSessionPrefs("alice", SessionPrefsPatch{Agents: map[string]AgentDefaults{"claude": {Model: "sonnet"}}})
	if s := runSession(); s.Model != "sonnet" || s.Effort != "" || m.SessionPrefs(s.Owner).Instructions != "Write PR titles in English." {
		t.Errorf("run after the defaults changed: %s/%s owned by %s", s.Model, s.Effort, s.Owner)
	}
	// Set on the schedule, or "default" for the agent's own, they win.
	m.PatchSchedule(alice, sc.ID, ScheduleReq{Model: ptr("default"), Effort: ptr("low")})
	if s := runSession(); s.Model != "" || s.Effort != "low" {
		t.Errorf("run with its own settings: %q/%q", s.Model, s.Effort)
	}
	// An effort the agent does not take is refused up front.
	if _, err := m.PatchSchedule(alice, sc.ID, ScheduleReq{Effort: ptr("extreme")}); err == nil {
		t.Error("an unknown effort was accepted")
	}
	// Switching agents drops the old agent's settings (codex has no "max").
	m.PatchSchedule(alice, sc.ID, ScheduleReq{Effort: ptr("max")})
	got, err := m.PatchSchedule(alice, sc.ID, ScheduleReq{Agent: ptr("codex")})
	if err != nil || got.Model != "" || got.Effort != "" {
		t.Errorf("switched to codex: %+v %v", got, err)
	}
}
