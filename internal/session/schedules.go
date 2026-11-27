package session

import (
	"cmp"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	_ "time/tzdata" // schedules name time zones; don't depend on the server having them

	"github.com/tattsun/collie/internal/agent"
	"github.com/tattsun/collie/internal/cron"
	"github.com/tattsun/collie/internal/model"
)

// Schedules start a session with the same instructions at set times (a
// cron expression read in a time zone). Each belongs to the user who made
// it: nobody else sees or changes it, and its sessions are theirs.
//
//   - Runs never overlap: a planned time that comes while the previous run
//     is still going (working, waiting for the user, or with background work
//     left) is skipped and noted in the history.
//   - Planned times that pass while Collie is not running are made up for
//     with a single run when it starts again, however many were missed.
//   - A run is over when its session's first turn has ended; it failed if
//     the turn ended with an error or the agent stopped before finishing.

const (
	maxSchedules    = 50               // per user
	maxScheduleRuns = 30               // history kept per schedule
	minScheduleGap  = 5 * time.Minute  // runs at most this often
	maxPromptLen    = 20000            // characters
	catchUpAfter    = 2 * time.Minute  // a run this late is a catch-up
	startTimeout    = 10 * time.Minute // a session that never got going
	scheduleTick    = 15 * time.Second
)

var (
	errScheduleNever    = errors.New("the schedule never runs")
	errScheduleOften    = errors.New("a schedule can run at most every 5 minutes")
	errScheduleBusy     = errors.New("the previous run is still running")
	errTooManySchedules = fmt.Errorf("you can have at most %d schedules", maxSchedules)
	errNoInstructions   = errors.New("the instructions are empty")
)

// ScheduleReq creates a schedule (ProjectID, Prompt and Cron are required)
// or changes one (nil fields are left as they are).
type ScheduleReq struct {
	ProjectID *string `json:"projectId"`
	Title     *string `json:"title"`
	Prompt    *string `json:"prompt"`
	Agent     *string `json:"agent"`
	Model     *string `json:"model"`
	Effort    *string `json:"effort"`
	Mode      *string `json:"mode"`
	Worktree  *bool   `json:"worktree"`
	Shared    *bool   `json:"shared"`
	Cron      *string `json:"cron"`
	TZ        *string `json:"tz"`
	Paused    *bool   `json:"paused"`
}

func (m *Manager) now() time.Time {
	if m.clock != nil {
		return m.clock()
	}
	return time.Now()
}

// Schedules lists the user's schedules.
func (m *Manager) Schedules(uid string) []model.Schedule {
	out := []model.Schedule{}
	for _, sc := range m.st.Schedules() {
		if sc.Owner == uid {
			out = append(out, sc)
		}
	}
	return out
}

// ownSchedule: other people's schedules do not exist for the user.
func (m *Manager) ownSchedule(uid, id string) (model.Schedule, error) {
	sc, ok := m.st.Schedule(strings.TrimSpace(id))
	if !ok || sc.Owner != uid {
		return model.Schedule{}, ErrNotFound
	}
	return sc, nil
}

func (m *Manager) CreateSchedule(a Actor, req ScheduleReq) (model.Schedule, error) {
	m.schedMu.Lock()
	defer m.schedMu.Unlock()
	if len(m.Schedules(a.ID)) >= maxSchedules {
		return model.Schedule{}, errTooManySchedules
	}
	now := m.now()
	sc := model.Schedule{ID: model.NewID(), Owner: a.ID, Agent: "claude", TZ: m.defaultTZ(a.ID),
		CreatedAt: now.UnixMilli(), UpdatedAt: now.UnixMilli()}
	if req.ProjectID == nil || req.Prompt == nil || req.Cron == nil {
		return model.Schedule{}, errors.New("a schedule needs a project, instructions and a time")
	}
	if err := m.applySchedule(a, &sc, req, true); err != nil {
		return model.Schedule{}, err
	}
	m.st.PutSchedule(sc)
	m.pushSchedule(sc)
	return sc, nil
}

func (m *Manager) PatchSchedule(a Actor, id string, req ScheduleReq) (model.Schedule, error) {
	m.schedMu.Lock()
	defer m.schedMu.Unlock()
	sc, err := m.ownSchedule(a.ID, id)
	if err != nil {
		return sc, err
	}
	if err := m.applySchedule(a, &sc, req, false); err != nil {
		return model.Schedule{}, err
	}
	sc.UpdatedAt = m.now().UnixMilli()
	// Keep runs noted since sc was read.
	sc, _ = m.st.UpdateSchedule(sc.ID, func(x *model.Schedule) bool {
		runs := x.Runs
		*x = sc
		x.Runs = runs
		return true
	})
	m.pushSchedule(sc)
	return sc, nil
}

func (m *Manager) DeleteSchedule(a Actor, id string) error {
	m.schedMu.Lock()
	defer m.schedMu.Unlock()
	sc, err := m.ownSchedule(a.ID, id)
	if err != nil {
		return err
	}
	m.deleteSchedule(sc)
	return nil
}

func (m *Manager) deleteSchedule(sc model.Schedule) {
	m.st.DeleteSchedule(sc.ID)
	b := mustJSON(map[string]any{"t": "schedule_del", "id": sc.ID})
	m.hub.each(func(c *Client) {
		if c.UserID == sc.Owner {
			c.enqueue(b)
		}
	})
}

// deleteSchedulesWhere removes the schedules of a deleted project or user.
// The sessions they started are not touched.
func (m *Manager) deleteSchedulesWhere(match func(model.Schedule) bool) {
	m.schedMu.Lock()
	defer m.schedMu.Unlock()
	for _, sc := range m.st.Schedules() {
		if match(sc) {
			m.deleteSchedule(sc)
		}
	}
}

// RunScheduleNow starts a run right away (paused schedules too), unless the
// previous one is still running.
func (m *Manager) RunScheduleNow(a Actor, id string) (model.ScheduleRun, error) {
	m.schedMu.Lock()
	defer m.schedMu.Unlock()
	sc, err := m.ownSchedule(a.ID, id)
	if err != nil {
		return model.ScheduleRun{}, err
	}
	if m.settle(sc.ID, false) {
		return model.ScheduleRun{}, errScheduleBusy
	}
	run := m.launch(sc, model.RunManual, 0, 0)
	if run.Status == model.RunError {
		return run, errors.New(run.Error)
	}
	return run, nil
}

// applySchedule checks a request and lays it over sc.
func (m *Manager) applySchedule(a Actor, sc *model.Schedule, req ScheduleReq, create bool) error {
	if req.ProjectID != nil {
		p, err := m.visibleProject(a.ID, strings.TrimSpace(*req.ProjectID))
		if err != nil {
			return err
		}
		if !p.Isolated() && !a.Admin {
			return errHostOnly
		}
		if sc.ProjectID != p.ID {
			sc.ProjectID = p.ID
			if req.Shared == nil {
				// The same default as a session started by hand.
				sc.Shared = p.OwnerGroup != ""
			}
		}
	}
	p, ok := m.st.Project(sc.ProjectID)
	if !ok {
		return ErrNotFound
	}
	if req.Title != nil {
		sc.Title = clip(strings.TrimSpace(*req.Title), 100)
	}
	if req.Prompt != nil {
		prompt := strings.TrimSpace(*req.Prompt)
		if prompt == "" {
			return errNoInstructions
		}
		if len([]rune(prompt)) > maxPromptLen {
			return fmt.Errorf("the instructions are too long (at most %d characters)", maxPromptLen)
		}
		sc.Prompt = prompt
	}
	if req.Agent != nil {
		if *req.Agent != "claude" && *req.Agent != "codex" {
			return errors.New("agent must be claude or codex")
		}
		if *req.Agent != sc.Agent {
			sc.Agent = *req.Agent
			if req.Mode == nil && !validMode(sc.Agent, sc.Mode) {
				sc.Mode = ""
			}
			// Models and efforts are the agent's own: back to the user's defaults.
			if req.Model == nil {
				sc.Model = ""
			}
			if req.Effort == nil {
				sc.Effort = ""
			}
		}
	}
	if req.Mode != nil {
		sc.Mode = strings.TrimSpace(*req.Mode)
	}
	if sc.Mode == "" {
		sc.Mode = agent.DefaultMode(sc.Agent)
	}
	if !validMode(sc.Agent, sc.Mode) {
		return fmt.Errorf("unknown mode %q", sc.Mode)
	}
	if req.Model != nil {
		sc.Model = strings.TrimSpace(*req.Model)
	}
	if req.Effort != nil {
		sc.Effort = strings.TrimSpace(*req.Effort)
	}
	if err := checkEffort(sc.Agent, pickSetting(sc.Effort, "")); err != nil {
		return err
	}
	if req.Worktree != nil {
		sc.Worktree = *req.Worktree
	}
	if sc.Worktree && (req.Worktree != nil || req.ProjectID != nil) {
		if err := checkWorktrees(p.Path); err != nil {
			return err
		}
	}
	if req.Shared != nil {
		sc.Shared = *req.Shared
	}
	if p.OwnerGroup == "" {
		sc.Shared = false
	}
	retime := create
	if req.Cron != nil {
		sc.Cron = strings.Join(strings.Fields(*req.Cron), " ")
		retime = true
	}
	if req.TZ != nil {
		sc.TZ = strings.TrimSpace(*req.TZ)
		retime = true
	}
	if req.Paused != nil && *req.Paused != sc.Paused {
		sc.Paused = *req.Paused
		retime = true
	}
	if retime {
		times, err := ScheduleTimes(sc.Cron, sc.TZ, m.now(), 1)
		if err != nil {
			return err
		}
		sc.NextAt = times[0].UnixMilli()
		if sc.Paused {
			sc.NextAt = 0
		}
	}
	return nil
}

// parseSchedule reads a cron expression in a time zone.
func parseSchedule(expr, tz string) (*cron.Expr, *time.Location, error) {
	e, err := cron.Parse(expr)
	if err != nil {
		return nil, nil, err
	}
	if tz == "" || strings.EqualFold(tz, "local") {
		return nil, nil, errors.New("a schedule needs a time zone")
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, nil, fmt.Errorf("unknown time zone %q", tz)
	}
	return e, loc, nil
}

// ScheduleTimes lists the next n times a schedule would run after from, and
// refuses schedules that never run or run more often than every five
// minutes.
func ScheduleTimes(expr, tz string, from time.Time, n int) ([]time.Time, error) {
	e, loc, err := parseSchedule(expr, tz)
	if err != nil {
		return nil, err
	}
	var out []time.Time
	t := from.In(loc)
	// Runs that come too close together tend to come in bursts: look at
	// enough of them to see one.
	for i := 0; i < max(n, 48); i++ {
		next := e.Next(t)
		if next.IsZero() {
			break
		}
		if i > 0 && next.Sub(t) < minScheduleGap {
			return nil, errScheduleOften
		}
		if len(out) < n {
			out = append(out, next)
		}
		t = next
	}
	if len(out) == 0 {
		return nil, errScheduleNever
	}
	return out, nil
}

// defaultTZ is the time zone of the user's latest schedule, else the
// server's.
func (m *Manager) defaultTZ(uid string) string {
	var tz string
	var at int64
	for _, sc := range m.Schedules(uid) {
		if sc.UpdatedAt > at && sc.TZ != "" {
			tz, at = sc.TZ, sc.UpdatedAt
		}
	}
	if tz != "" {
		return tz
	}
	return localTZ()
}

// localTZ names the server's time zone.
func localTZ() string {
	valid := func(name string) bool {
		_, err := time.LoadLocation(name)
		return name != "" && !strings.EqualFold(name, "local") && err == nil
	}
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); valid(tz) {
		return tz
	}
	if p, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(p, "zoneinfo/"); ok && valid(name) {
			return name
		}
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		if tz := strings.TrimSpace(string(b)); valid(tz) {
			return tz
		}
	}
	return "UTC"
}

// ---- running ----

// runSchedules starts runs as they come due. Its first pass makes up for
// the times missed while Collie was not running.
func (m *Manager) runSchedules() {
	for {
		next := m.runDue(m.now())
		wait := scheduleTick
		if !next.IsZero() {
			wait = min(wait, max(time.Until(next), time.Second))
		}
		time.Sleep(wait)
	}
}

// runDue starts the runs that are due at now and returns the earliest
// planned time left.
func (m *Manager) runDue(now time.Time) time.Time {
	m.schedMu.Lock()
	defer m.schedMu.Unlock()
	var earliest time.Time
	for _, sc := range m.st.Schedules() {
		if sc.Paused || sc.NextAt == 0 {
			continue
		}
		if now.UnixMilli() >= sc.NextAt {
			sc = m.fire(sc, now)
		}
		if sc.NextAt != 0 && (earliest.IsZero() || sc.NextAt < earliest.UnixMilli()) {
			earliest = time.UnixMilli(sc.NextAt)
		}
	}
	return earliest
}

// fire handles a schedule whose planned time has come: it moves on to the
// next planned time and starts one run, however many planned times have
// passed (more than one only when Collie was not running).
func (m *Manager) fire(sc model.Schedule, now time.Time) model.Schedule {
	due := time.UnixMilli(sc.NextAt)
	e, loc, err := parseSchedule(sc.Cron, sc.TZ)
	var next time.Time
	missed := 1
	if err == nil {
		for t := e.Next(due.In(loc)); !t.IsZero() && !t.After(now) && missed < 1000; t = e.Next(t) {
			missed++
		}
		next = e.Next(now.In(loc))
	}
	sc, _ = m.st.UpdateSchedule(sc.ID, func(x *model.Schedule) bool {
		x.NextAt = 0
		if !next.IsZero() {
			x.NextAt = next.UnixMilli()
		}
		return true
	})
	trigger := model.RunScheduled
	if missed > 1 || now.Sub(due) > catchUpAfter {
		trigger = model.RunCatchUp
	}
	if missed == 1 {
		missed = 0
	}
	switch {
	case err != nil:
		m.addRun(sc.ID, model.ScheduleRun{Trigger: trigger, At: now.UnixMilli(), Planned: due.UnixMilli(), Status: model.RunError, Error: err.Error()})
	case m.UserActive != nil && !m.UserActive(sc.Owner):
		m.addRun(sc.ID, model.ScheduleRun{Trigger: trigger, At: now.UnixMilli(), Planned: due.UnixMilli(), Missed: missed,
			Status: model.RunSkipped, Error: "Your account is disabled."})
	case m.settle(sc.ID, false):
		m.addRun(sc.ID, model.ScheduleRun{Trigger: trigger, At: now.UnixMilli(), Planned: due.UnixMilli(), Missed: missed,
			Status: model.RunSkipped, Error: "The previous run was still running."})
	default:
		m.launch(sc, trigger, due.UnixMilli(), missed)
	}
	sc, _ = m.st.Schedule(sc.ID)
	return sc
}

// scheduleSessionReq is the session a run starts, for the schedule's owner.
// A model or effort left empty is the owner's default at the time of the
// run ("default": the agent's own), so schedules follow changed defaults.
// The owner's instructions for all sessions reach it as they reach any
// session they start (see Runtime.startDriver).
func (m *Manager) scheduleSessionReq(sc model.Schedule) CreateSessionReq {
	shared := sc.Shared
	req := CreateSessionReq{ProjectID: sc.ProjectID, Agent: sc.Agent, Mode: sc.Mode,
		Title: sc.Title, Prompt: sc.Prompt, Worktree: sc.Worktree, Shared: &shared}
	req.Model, req.Effort = m.ResolveModelEffort(sc.Owner, sc.Agent, sc.Model, sc.Effort)
	return req
}

// launch starts a run: a new session of the schedule's owner. The run is
// noted first, with the session's id, so that the session's progress can
// find it from its very first update.
func (m *Manager) launch(sc model.Schedule, trigger string, planned int64, missed int) model.ScheduleRun {
	now := m.now().UnixMilli()
	run := model.ScheduleRun{ID: model.NewID(), Trigger: trigger, At: now, Planned: planned, Missed: missed,
		SessionID: model.NewID(), Status: model.RunRunning}
	m.addRun(sc.ID, run)
	req := m.scheduleSessionReq(sc)
	req.id, req.schedule = run.SessionID, sc.ID
	s, err := m.createSession(Actor{ID: sc.Owner, Admin: m.isAdmin(sc.Owner)}, req, "schedule")
	if err == nil {
		return run
	}
	if errors.Is(err, ErrNotFound) {
		err = errors.New("the project is gone, or you no longer have access to it")
	}
	log.Printf("schedule %s: start run: %v", sc.ID, err)
	run, _ = m.updateRun(sc.ID, run.ID, func(r *model.ScheduleRun) {
		if s.ID == "" {
			r.SessionID = "" // no session was created
		}
		r.Status, r.Error, r.EndedAt = model.RunError, err.Error(), now
	})
	return run
}

// addRun notes a run at the top of the history. A skip right after a skip
// for the same reason is folded into it.
func (m *Manager) addRun(id string, run model.ScheduleRun) {
	if run.ID == "" {
		run.ID = model.NewID()
	}
	sc, changed := m.st.UpdateSchedule(id, func(x *model.Schedule) bool {
		if run.Status == model.RunSkipped && len(x.Runs) > 0 {
			if top := &x.Runs[0]; top.Status == model.RunSkipped && top.Error == run.Error {
				top.Count = max(top.Count, 1) + 1
				top.At, top.Planned = run.At, run.Planned
				return true
			}
		}
		x.Runs = append([]model.ScheduleRun{run}, x.Runs...)
		if len(x.Runs) > maxScheduleRuns {
			x.Runs = x.Runs[:maxScheduleRuns]
		}
		return true
	})
	if changed {
		m.pushSchedule(sc)
	}
}

func (m *Manager) updateRun(id, runID string, fn func(*model.ScheduleRun)) (model.ScheduleRun, bool) {
	var run model.ScheduleRun
	sc, changed := m.st.UpdateSchedule(id, func(x *model.Schedule) bool {
		for i := range x.Runs {
			if x.Runs[i].ID == runID {
				fn(&x.Runs[i])
				run = x.Runs[i]
				return true
			}
		}
		return false
	})
	if changed {
		m.pushSchedule(sc)
	}
	return run, changed
}

// sessionBusy: the agent is at work, waiting for the user, or has
// background work left.
func sessionBusy(s model.Session) bool {
	switch s.Status {
	case model.StatusStarting, model.StatusRunning, model.StatusWaiting:
		return true
	}
	return len(s.Tasks) > 0
}

// finishRun closes a run; outcome "" takes how the first turn ended.
func finishRun(r *model.ScheduleRun, outcome, why string, now int64) {
	if outcome == "" {
		outcome = model.RunOK
		if r.Result == model.RunError {
			outcome = model.RunError
		}
	}
	r.Status, r.EndedAt = outcome, now
	switch {
	case outcome == model.RunOK:
		r.Error = "" // errors along the way that the turn got past
	case r.Error == "":
		r.Error = why
	}
}

// settle closes the schedule's runs that are over but still marked
// running (their session went away, or Collie restarted under them), and
// reports whether one is still running. After a restart nothing is.
func (m *Manager) settle(id string, restarted bool) (busy bool) {
	sc, ok := m.st.Schedule(id)
	if !ok {
		return false
	}
	now := m.now().UnixMilli()
	type end struct{ outcome, why string }
	ends := map[string]end{}
	for _, r := range sc.Runs {
		if r.Status != model.RunRunning {
			continue
		}
		s, ok := m.st.Session(r.SessionID)
		var e end
		switch {
		case r.SessionID == "" || !ok:
			e = end{model.RunError, "The session was deleted before the run finished."}
		case restarted && r.Result == "":
			e = end{model.RunError, "Collie restarted before the run finished."}
		case restarted:
		case sessionBusy(s):
			busy = true
			continue
		case s.Status == model.StatusError:
			e = end{model.RunError, "The agent stopped with an error."}
		case r.Result != "" || s.Status == model.StatusIdle:
		case now-r.At > startTimeout.Milliseconds():
			e = end{model.RunError, "The session did not start."}
		default:
			busy = true // just started: its agent is on the way
			continue
		}
		ends[r.ID] = e
	}
	if len(ends) == 0 {
		return busy
	}
	sc, changed := m.st.UpdateSchedule(id, func(x *model.Schedule) bool {
		changed := false
		for i := range x.Runs {
			if e, ok := ends[x.Runs[i].ID]; ok && x.Runs[i].Status == model.RunRunning {
				finishRun(&x.Runs[i], e.outcome, e.why, now)
				changed = true
			}
		}
		return changed
	})
	if changed {
		m.pushSchedule(sc)
	}
	return busy
}

// settleAll closes every run left running by a previous Collie process.
func (m *Manager) settleAll() {
	for _, sc := range m.st.Schedules() {
		m.settle(sc.ID, true)
	}
}

// trackRun follows a scheduled session's status changes to close its run.
func (m *Manager) trackRun(before, s model.Session) {
	if s.Schedule == "" || s.Status == before.Status && len(s.Tasks) == len(before.Tasks) {
		return
	}
	var outcome, why string
	switch s.Status {
	case model.StatusError:
		outcome, why = model.RunError, "The agent stopped with an error."
	case model.StatusIdle:
		if len(s.Tasks) > 0 {
			return // the turn is over, its background work is not
		}
	case model.StatusStopped:
		switch before.Status {
		case model.StatusIdle:
		case model.StatusStarting, model.StatusRunning, model.StatusWaiting:
			outcome, why = model.RunError, "Stopped before it finished."
		default:
			return // just created
		}
	default:
		return
	}
	now := m.now().UnixMilli()
	sc, changed := m.st.UpdateSchedule(s.Schedule, func(x *model.Schedule) bool {
		for i := range x.Runs {
			if r := &x.Runs[i]; r.SessionID == s.ID && r.Status == model.RunRunning {
				finishRun(r, outcome, why, now)
				return true
			}
		}
		return false
	})
	if changed {
		m.pushSchedule(sc)
	}
}

// noteRunItem notes, on a scheduled session's run, how its first turn
// ended (a result) and what went wrong (an error). Called with the
// session's runtime locked.
func (m *Manager) noteRunItem(sid string, it *model.Item) {
	s, ok := m.st.Session(sid)
	if !ok || s.Schedule == "" || (it.Kind == model.KindResult && it.Result == nil) {
		return
	}
	text := clip(firstLineOf(it.Text), 300)
	sc, changed := m.st.UpdateSchedule(s.Schedule, func(x *model.Schedule) bool {
		for i := range x.Runs {
			r := &x.Runs[i]
			if r.SessionID != sid || r.Status != model.RunRunning || r.Result != "" {
				continue
			}
			switch {
			case it.Kind == model.KindError:
				if r.Error != "" || text == "" {
					return false
				}
				r.Error = text
			case it.Result.IsError:
				r.Result = model.RunError
				r.Error = cmp.Or(text, r.Error, "The turn ended with an error.")
			default:
				r.Result = model.RunOK
			}
			return true
		}
		return false
	})
	if changed {
		m.pushSchedule(sc)
	}
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// pushSchedule sends a schedule to its owner's clients (only theirs).
func (m *Manager) pushSchedule(sc model.Schedule) {
	if sc.ID == "" {
		return
	}
	b := mustJSON(map[string]any{"t": "schedule", "schedule": sc})
	m.hub.each(func(c *Client) {
		if c.UserID == sc.Owner {
			c.enqueue(b)
		}
	})
}
