package session

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tattsun/collie/internal/agent"
	"github.com/tattsun/collie/internal/gitutil"
	"github.com/tattsun/collie/internal/model"
)

// Following work through: the Home agent can stop and archive the sessions
// it coordinates, read what they changed, and watch them. When a session a
// Home conversation watches finishes its turn, starts waiting for the user
// or fails, Collie tells that conversation in a message that wakes its
// agent — after its current turn. Home conversations are never watched
// themselves, so a notification cannot set off another one.

// Pacing of the notifications to one Home conversation (variables for the
// tests).
var (
	noteSettle = 5 * time.Second  // changes within this time go out together
	noteGap    = 30 * time.Second // at least this long between two notifications
	// After this many notifications about one session, Collie stops
	// watching it, so that a back-and-forth cannot go on unnoticed.
	noteCap = 30
)

const maxWatching = 20 // sessions one Home conversation watches at most

// noteBox holds what is to be told to one Home conversation.
type noteBox struct {
	order  []string          // watched sessions that changed, first change first
	events map[string]string // ...and their latest change (see noteEvent)
	timer  *time.Timer
	last   time.Time      // when the last notification went out
	sent   map[string]int // notifications per watched session
}

// box must be called with m.noteMu held.
func (m *Manager) box(home string) *noteBox {
	if m.notes == nil {
		m.notes = map[string]*noteBox{}
	}
	b := m.notes[home]
	if b == nil {
		b = &noteBox{events: map[string]string{}, sent: map[string]int{}}
		m.notes[home] = b
	}
	return b
}

// noteEvent names a status change worth telling a watcher about.
func noteEvent(prev, cur string) string {
	switch cur {
	case model.StatusIdle:
		if prev == model.StatusRunning || prev == model.StatusWaiting {
			return "done"
		}
	case model.StatusWaiting:
		return "waiting"
	case model.StatusError:
		return "error"
	}
	return ""
}

// settled: a Home conversation in this status is not in a turn, so a
// notification may start one.
func settled(status string) bool {
	return status == model.StatusIdle || status == model.StatusStopped || status == model.StatusError
}

// noteChange is called on every session update (under m.bmu, so in order;
// it only takes notes and sets timers).
func (m *Manager) noteChange(before, s model.Session) {
	if before.Status == s.Status {
		return
	}
	m.noteMu.Lock()
	defer m.noteMu.Unlock()
	// A Home conversation's turn ended: deliver what was held for it.
	if b := m.notes[s.ID]; b != nil && len(b.order) > 0 && b.timer == nil && settled(s.Status) {
		m.scheduleNote(s.ID, b)
	}
	ev := noteEvent(before.Status, s.Status)
	if ev == "" {
		return
	}
	if p, ok := m.st.Project(s.ProjectID); !ok || p.Home {
		return
	}
	for _, h := range m.st.Sessions() {
		if !slices.Contains(h.Watching, s.ID) {
			continue
		}
		b := m.box(h.ID)
		if _, ok := b.events[s.ID]; !ok {
			b.order = append(b.order, s.ID)
		}
		b.events[s.ID] = ev
		if b.timer == nil {
			m.scheduleNote(h.ID, b)
		}
	}
}

// scheduleNote must be called with m.noteMu held.
func (m *Manager) scheduleNote(home string, b *noteBox) {
	d := noteSettle
	if wait := noteGap - time.Since(b.last); wait > d {
		d = wait
	}
	b.timer = time.AfterFunc(d, func() { m.flushNotes(home) })
}

// flushNotes tells the Home conversation what changed, unless it is in a
// turn: then it waits for noteChange to see the turn end.
func (m *Manager) flushNotes(home string) {
	m.noteMu.Lock()
	b := m.notes[home]
	if b != nil {
		b.timer = nil
	}
	m.noteMu.Unlock()
	if b == nil {
		return
	}
	h, ok := m.st.Session(home)
	if !ok || h.Archived {
		// Gone, or put away by the user: nothing to wake.
		m.noteMu.Lock()
		delete(m.notes, home)
		m.noteMu.Unlock()
		return
	}
	if !settled(h.Status) {
		return
	}
	m.noteMu.Lock()
	order, events := b.order, b.events
	b.order, b.events = nil, map[string]string{}
	m.noteMu.Unlock()

	var notes []model.Note
	for _, sid := range order {
		if !slices.Contains(h.Watching, sid) {
			continue // unwatched in the meantime
		}
		if n, ok := m.noteFor(h.Owner, sid, events[sid]); ok {
			notes = append(notes, n)
		}
	}
	if len(notes) == 0 {
		return
	}
	var capped []string
	m.noteMu.Lock()
	b.last = time.Now()
	for _, n := range notes {
		if b.sent[n.Session]++; b.sent[n.Session] >= noteCap {
			delete(b.sent, n.Session)
			capped = append(capped, n.Session)
		}
	}
	m.noteMu.Unlock()
	if len(capped) > 0 {
		m.unwatch(home, func(id string) bool { return slices.Contains(capped, id) })
		for _, n := range slices.Clone(notes) {
			if slices.Contains(capped, n.Session) {
				notes = append(notes, model.Note{Session: n.Session, Title: n.Title, Project: n.Project, Event: "unwatched"})
			}
		}
	}
	if err := m.deliverNotes(h, notes); err != nil {
		log.Printf("home %s: notify: %v", home, err)
	}
}

// noteFor describes a watched session's change as it stands now, or
// reports false when there is nothing to tell any more: the session moved
// on (its next change will be told), or the user can no longer see it.
func (m *Manager) noteFor(uid, sid, ev string) (model.Note, bool) {
	s, err := m.visibleSession(uid, sid)
	if err != nil {
		return model.Note{}, false
	}
	p, _ := m.st.Project(s.ProjectID)
	n := model.Note{Session: s.ID, Title: s.Title, Project: p.Name}
	r, err := m.runtime(sid)
	if err != nil {
		return model.Note{}, false
	}
	items, _ := r.page(0, 60)
	switch s.Status {
	case model.StatusWaiting:
		n.Event, n.Detail = pendingAsk(items)
	case model.StatusError:
		n.Event, n.Detail = "error", lastError(items)
	case model.StatusIdle, model.StatusStopped:
		if ev != "done" {
			return model.Note{}, false // e.g. it waited, then the user stopped it
		}
		n.Event, n.Detail = turnOutcome(items)
	default:
		return model.Note{}, false
	}
	return n, true
}

func pendingAsk(items []*model.Item) (event, detail string) {
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it.Ask == nil || it.Ask.State != "pending" {
			continue
		}
		if it.Kind == model.KindQuestion {
			if len(it.Ask.Questions) > 0 {
				return "question", clip(it.Ask.Questions[0].Question, 300)
			}
			return "question", ""
		}
		what := it.Ask.Tool
		switch {
		case it.Ask.Title != "" && what != "":
			what += ": " + it.Ask.Title
		case it.Ask.Title != "":
			what = it.Ask.Title
		case len(it.Ask.Input) > 0:
			what += " " + string(it.Ask.Input)
		}
		return "waiting", clip(what, 300)
	}
	return "waiting", ""
}

func lastError(items []*model.Item) string {
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Kind == model.KindError {
			return clip(items[i].Text, 300)
		}
	}
	return ""
}

// turnOutcome reads how the last turn ended: its result, and the agent's
// last words.
func turnOutcome(items []*model.Item) (event, detail string) {
	var said string
	for i := len(items) - 1; i >= 0 && items[i].Kind != model.KindUser; i-- {
		it := items[i]
		switch {
		case it.Kind == model.KindResult && it.Result != nil:
			st := it.Result.Status
			interrupted := st == "interrupted" || st == "error_during_execution"
			if !interrupted && (it.Result.IsError || (st != "" && st != "success" && st != "completed")) {
				return "failed", clip(cmp.Or(it.Text, st), 300)
			}
		case it.Kind == model.KindText && it.Parent == "" && said == "":
			said = strings.TrimSpace(it.Text)
		}
	}
	return "done", clip(said, 500)
}

// deliverNotes puts the notification on the Home conversation's timeline
// and hands it to its agent, like a message from the user (which wakes the
// agent if it was stopped).
func (m *Manager) deliverNotes(h model.Session, notes []model.Note) error {
	if !m.sessionDrivable(h) {
		return errOwnerGone
	}
	if err := m.hostCheck(Actor{ID: h.Owner, Admin: m.isAdmin(h.Owner)}, h); err != nil {
		return err
	}
	r, err := m.runtime(h.ID)
	if err != nil {
		return err
	}
	text := noteText(notes)
	it := &model.Item{ID: "u-" + model.NewID(), Kind: model.KindUser, Text: text, Done: true, Via: "collie", Notes: notes}
	r.mu.Lock()
	r.ensureLoaded()
	r.put(it)
	r.mu.Unlock()
	m.updateSession(h.ID, func(s *model.Session) { s.UpdatedAt = model.NowMs() })
	r.enqueue(agent.Input{ItemID: it.ID, Text: text}, nil)
	return nil
}

// noteText is the notification as the Home agent reads it.
func noteText(notes []model.Note) string {
	var b strings.Builder
	b.WriteString("[Collie] Update on sessions you watch (an automatic notification, not a message from the user):\n")
	for _, n := range notes {
		fmt.Fprintf(&b, "- %q (session %s, project %s) ", n.Title, n.Session, n.Project)
		switch n.Event {
		case "done":
			b.WriteString("finished its turn.")
			if n.Detail != "" {
				fmt.Fprintf(&b, " Its last message: %q", n.Detail)
			}
		case "failed":
			fmt.Fprintf(&b, "ended its turn with an error: %q", n.Detail)
		case "waiting":
			fmt.Fprintf(&b, "is waiting for the user to approve: %s", n.Detail)
		case "question":
			fmt.Fprintf(&b, "has a question for the user: %q", n.Detail)
		case "error":
			fmt.Fprintf(&b, "stopped with an error: %q", n.Detail)
		case "unwatched":
			fmt.Fprintf(&b, "is no longer watched: Collie stops after %d notifications about one session. Use watch_session if you still need to follow it.", noteCap)
		}
		b.WriteString("\n")
	}
	b.WriteString("Follow up as needed (read_session, read_diff), then tell the user briefly. Approvals and questions are for the user to answer in that session.")
	return b.String()
}

// ---- watch lists ----

// watch adds a session to what a Home conversation watches (and starts its
// notification count afresh).
func (m *Manager) watch(home, sid string) error {
	h, ok := m.st.Session(home)
	if !ok {
		return errSessionGone
	}
	// Make room by dropping sessions that are gone or archived.
	dead := map[string]bool{}
	for _, id := range h.Watching {
		if s, ok := m.st.Session(id); !ok || s.Archived {
			dead[id] = true
		}
	}
	full := false
	m.updateSession(home, func(s *model.Session) {
		if slices.Contains(s.Watching, sid) {
			return
		}
		list := slices.DeleteFunc(slices.Clone(s.Watching), func(id string) bool { return dead[id] })
		if len(list) >= maxWatching {
			full = true
			s.Watching = list
			return
		}
		s.Watching = append(list, sid)
	})
	if full {
		return fmt.Errorf("you already watch %d sessions, the most Collie allows; unwatch some first", maxWatching)
	}
	m.noteMu.Lock()
	if b := m.notes[home]; b != nil {
		delete(b.sent, sid)
	}
	m.noteMu.Unlock()
	return nil
}

// unwatch removes the sessions drop matches from a Home conversation's
// watch list, with anything still to be told about them.
func (m *Manager) unwatch(home string, drop func(string) bool) {
	m.updateSession(home, func(s *model.Session) {
		if slices.ContainsFunc(s.Watching, drop) {
			s.Watching = slices.DeleteFunc(slices.Clone(s.Watching), drop)
			if len(s.Watching) == 0 {
				s.Watching = nil
			}
		}
	})
	m.noteMu.Lock()
	if b := m.notes[home]; b != nil {
		b.order = slices.DeleteFunc(b.order, drop)
		for id := range b.events {
			if drop(id) {
				delete(b.events, id)
			}
		}
	}
	m.noteMu.Unlock()
}

// Unwatch stops a Home conversation's agent from being told about a
// session: the user's way to do it, next to the agent's unwatch_session.
func (m *Manager) Unwatch(a Actor, home, sid string) error {
	h, err := m.visibleSession(a.ID, home)
	if err != nil {
		return err
	}
	if !m.sessionAdmin(a.ID, h) {
		return ErrForbidden
	}
	m.unwatch(h.ID, func(id string) bool { return id == sid })
	return nil
}

// ---- tools ----

// homeFollowTool reports whether a Home tool is served here, with the
// conversation calling it; start_session is callTool's plus watching what
// it started.
func homeFollowTool(name string) bool {
	switch name {
	case "start_session", "stop_session", "archive_session", "read_diff", "watch_session", "unwatch_session":
		return true
	}
	return false
}

var homeFollowToolDefs = []map[string]any{
	{
		"name": "read_diff",
		"description": "Read what a session changed, as its Changes tab shows it: for a session on its own branch (worktree), everything since the branch was made, committed or not; " +
			"otherwise the uncommitted changes in the project folder. New untracked files are included. Long diffs are cut short, and the answer says what was left out: narrow it with `files`.",
		"inputSchema": obj(map[string]any{
			"session":   prop("string", "Session id"),
			"files":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Only these files: paths, folders (web/src/) or glob patterns (*.go, internal/**/*_test.go)"},
			"max_chars": prop("integer", "About how long the answer may be (default 30000, at most 60000)"),
		}, "session"),
	},
	{
		"name": "watch_session",
		"description": "Follow a session: when it finishes its turn, starts waiting for the user (an approval or a question) or fails, Collie tells you in this conversation, after your current turn. " +
			"Sessions you start are watched already. Each notification costs you a turn: unwatch sessions you no longer need to follow. Without `session`, lists the sessions you watch.",
		"inputSchema": obj(map[string]any{
			"session": prop("string", "Session id; omit to list what you watch"),
		}),
	},
	{
		"name":        "unwatch_session",
		"description": "Stop Collie's notifications about a session, or about all of them.",
		"inputSchema": obj(map[string]any{
			"session": prop("string", "Session id"),
			"all":     prop("boolean", "Stop watching every session"),
		}),
	},
	{
		"name": "stop_session",
		"description": "Stop a session's agent: this cuts off its current turn, cancels its pending approvals and ends work it left running in the background. " +
			"The conversation is kept and a later message resumes it; its worktree and branch stay as they are. Only for sessions the user started.",
		"inputSchema": obj(map[string]any{
			"session": prop("string", "Session id"),
		}, "session"),
	},
	{
		"name": "archive_session",
		"description": "Archive a session whose work is done, so it leaves the user's session list (nothing is deleted: the conversation, worktree and branch stay), or bring it back with archived=false. " +
			"Archiving does not stop a running agent (stop_session does). Only for sessions the user started. " +
			"It also archives (or brings back) the user's other Home conversations, but not the one you are in.",
		"inputSchema": obj(map[string]any{
			"session":  prop("string", "Session id"),
			"archived": prop("boolean", "false to unarchive (default true)"),
		}, "session"),
	},
}

// stringList is a JSON string array that also takes a single string.
type stringList []string

func (l *stringList) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*l = []string{one}
		return nil
	}
	return json.Unmarshal(b, (*[]string)(l))
}

func (m *Manager) callFollowTool(tc *toolConn, name string, raw json.RawMessage) (string, error) {
	a := m.toolActor(tc.scope.uid)
	var p struct {
		Session  string     `json:"session"`
		All      bool       `json:"all"`
		Archived *bool      `json:"archived"`
		Watch    *bool      `json:"watch"`
		Files    stringList `json:"files"`
		MaxChars int        `json:"max_chars"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", err
	}
	switch name {
	case "start_session":
		text, err := m.callTool(tc, name, raw)
		if err != nil || (p.Watch != nil && !*p.Watch) || tc.session == "" {
			return text, err
		}
		return m.watchStarted(tc.session, text), nil
	case "read_diff":
		return m.toolReadDiff(a, p.Session, p.Files, p.MaxChars)
	case "stop_session":
		return m.toolStopSession(a, p.Session)
	case "archive_session":
		return m.toolArchiveSession(a, tc.session, p.Session, p.Archived == nil || *p.Archived)
	}
	// Watching is per Home conversation: the one calling.
	if tc.session == "" {
		return "", errors.New("Collie cannot tell which Home conversation this is; restart it to watch sessions")
	}
	switch name {
	case "watch_session":
		return m.toolWatch(a, tc.session, p.Session)
	case "unwatch_session":
		return m.toolUnwatch(a, tc.session, p.Session, p.All)
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

// watchStarted watches the session start_session started (its result
// names it) and says so in the result.
func (m *Manager) watchStarted(home, result string) string {
	var out map[string]any
	if json.Unmarshal([]byte(result), &out) != nil {
		return result
	}
	id, _ := out["id"].(string)
	if id == "" {
		return result
	}
	if err := m.watch(home, id); err != nil {
		out["watched"] = false
		out["watchError"] = err.Error()
	} else {
		out["watched"] = true
	}
	return pretty(out)
}

// ownSession: Home stops and archives only the user's own sessions, even
// where Collie lets them do it to other people's (a group's owners).
func ownSession(a Actor, s model.Session) error {
	if s.Owner != a.ID {
		return errors.New("that session was started by someone else; Home only stops or archives the user's own sessions")
	}
	return nil
}

func (m *Manager) toolStopSession(a Actor, sid string) (string, error) {
	s, p, err := m.workSession(a.ID, sid)
	if err != nil {
		return "", err
	}
	if err := ownSession(a, s); err != nil {
		return "", err
	}
	if s.Status == model.StatusStopped || s.Status == model.StatusError {
		return fmt.Sprintf("%q in %s is not running.", s.Title, p.Name), nil
	}
	if err := m.Stop(a, s.ID); err != nil {
		return "", err
	}
	// Stop returns at once; give the agent a moment to exit.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if cur, ok := m.st.Session(s.ID); !ok || cur.Status == model.StatusStopped || cur.Status == model.StatusError {
			return fmt.Sprintf("Stopped %q in %s. A message to it resumes the conversation.", s.Title, p.Name), nil
		}
	}
	return fmt.Sprintf("Stopping %q in %s: its agent is still shutting down.", s.Title, p.Name), nil
}

// toolArchiveSession archives a session, or one of the user's Home
// conversations other than home, the one calling.
func (m *Manager) toolArchiveSession(a Actor, home, sid string, archived bool) (string, error) {
	s, p, err := m.viewSession(a.ID, sid)
	if err != nil {
		return "", err
	}
	if err := ownSession(a, s); err != nil {
		return "", err
	}
	if p.Home {
		return m.archiveHomeConversation(a, home, s, archived)
	}
	if s.Archived != archived {
		if _, err := m.Patch(a, s.ID, PatchSessionReq{Archived: &archived}); err != nil {
			return "", err
		}
	}
	if !archived {
		return fmt.Sprintf("%q in %s is back in the session list.", s.Title, p.Name), nil
	}
	// Archived work is done: stop following it.
	if home != "" {
		m.unwatch(home, func(id string) bool { return id == s.ID })
	}
	msg := fmt.Sprintf("Archived %q in %s: it is out of the session list; the conversation", s.Title, p.Name)
	if s.Worktree {
		msg += ", worktree and branch " + s.Branch
	}
	msg += " are kept."
	switch cur, _ := m.st.Session(s.ID); cur.Status {
	case model.StatusStarting, model.StatusRunning, model.StatusWaiting:
		msg += " It is still working (stop_session stops it)."
	}
	return msg, nil
}

func (m *Manager) archiveHomeConversation(a Actor, home string, s model.Session, archived bool) (string, error) {
	if archived {
		switch {
		case s.ID == home:
			return "", errors.New("that is this Home conversation, the one you are in; Home does not archive it (the user can, in Collie)")
		case home == "":
			return "", errors.New("Collie cannot tell which Home conversation this is, so it does not archive Home conversations from here (it could be this one); restart it, or the user can archive them in Collie")
		}
	}
	if s.Archived != archived {
		if _, err := m.Patch(a, s.ID, PatchSessionReq{Archived: &archived}); err != nil {
			return "", err
		}
	}
	if !archived {
		return fmt.Sprintf("The Home conversation %q is back in the user's list in Home.", s.Title), nil
	}
	msg := fmt.Sprintf("Archived the Home conversation %q: it is out of the user's list in Home; the conversation is kept.", s.Title)
	if len(s.Watching) > 0 {
		msg += " While it is archived, Collie does not tell it about the sessions it watches."
	}
	switch cur, _ := m.st.Session(s.ID); cur.Status {
	case model.StatusStarting, model.StatusRunning, model.StatusWaiting:
		msg += " Its agent is still working (only the user can stop it, in Collie)."
	}
	return msg, nil
}

func (m *Manager) toolWatch(a Actor, home, sid string) (string, error) {
	if strings.TrimSpace(sid) == "" {
		return m.watchList(a, home), nil
	}
	s, p, err := m.workSession(a.ID, sid)
	if err != nil {
		return "", err
	}
	if err := m.watch(home, s.ID); err != nil {
		return "", err
	}
	return fmt.Sprintf("Watching %q in %s (now %s): Collie will tell you when it finishes its turn, needs the user, or fails.", s.Title, p.Name, s.Status), nil
}

func (m *Manager) toolUnwatch(a Actor, home, sid string, all bool) (string, error) {
	h, ok := m.st.Session(home)
	if !ok {
		return "", errSessionGone
	}
	if all {
		m.unwatch(home, func(string) bool { return true })
		return fmt.Sprintf("Stopped watching %d session(s).", len(h.Watching)), nil
	}
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return "", errors.New("name a session, or pass all: true")
	}
	if !slices.Contains(h.Watching, sid) {
		return fmt.Sprintf("You were not watching %s.", sid), nil
	}
	m.unwatch(home, func(id string) bool { return id == sid })
	return fmt.Sprintf("Stopped watching %s.", sid), nil
}

func (m *Manager) watchList(a Actor, home string) string {
	h, _ := m.st.Session(home)
	var b strings.Builder
	for _, id := range h.Watching {
		s, err := m.visibleSession(a.ID, id)
		if err != nil {
			continue
		}
		p, _ := m.st.Project(s.ProjectID)
		fmt.Fprintf(&b, "- %q (%s) in %s: %s, updated %s\n", s.Title, s.ID, p.Name, s.Status, ago(s.UpdatedAt))
	}
	if b.Len() == 0 {
		return "You watch no sessions."
	}
	return "You watch:\n" + b.String()
}

// ---- read_diff ----

func (m *Manager) toolReadDiff(a Actor, sid string, files []string, maxChars int) (string, error) {
	s, p, err := m.workSession(a.ID, sid)
	if err != nil {
		return "", err
	}
	ch, err := m.Changes(a, s.ID)
	if err != nil {
		return "", err
	}
	return formatDiff(s, p.Name, ch, files, maxChars), nil
}

const (
	diffDefault = 30000
	diffMax     = 60000
	diffFiles   = 200 // files listed at most
)

// formatDiff writes a session's changes for the Home agent: what they are
// against, the commits, the files with their size, then the diffs within
// about maxChars, saying what did not fit.
func formatDiff(s model.Session, project string, ch *gitutil.Changes, files []string, maxChars int) string {
	if maxChars <= 0 {
		maxChars = diffDefault
	}
	maxChars = min(max(maxChars, 2000), diffMax)
	var b strings.Builder
	if !ch.Repo {
		return fmt.Sprintf("%q works in %s, which is not a git repository: there is no diff to show.", s.Title, project)
	}
	if ch.Base != "" {
		fmt.Fprintf(&b, "Changes of %q (%s) in %s, on its branch %s since it was made from %s: its commits and uncommitted work.\n",
			s.Title, s.ID, project, ch.Branch, ch.Base[:min(len(ch.Base), 10)])
	} else {
		fmt.Fprintf(&b, "Uncommitted changes in the folder of %s (branch %s), where %q works. Other sessions working in that folder (not on their own branch) change it too.\n",
			project, cmp.Or(ch.Branch, "none"), s.Title)
	}
	if len(ch.Commits) > 0 {
		b.WriteString("Commits:\n")
		for i, c := range ch.Commits {
			if i == 30 {
				fmt.Fprintf(&b, "  … and %d more\n", len(ch.Commits)-i)
				break
			}
			b.WriteString("  " + c + "\n")
		}
	}
	if len(ch.Files) == 0 {
		b.WriteString("No changed files.")
		return b.String()
	}
	match := fileFilter(files)
	var shown []gitutil.FileChange
	for _, f := range ch.Files {
		if match(f.Path) {
			shown = append(shown, f)
		}
	}
	if len(shown) == 0 {
		fmt.Fprintf(&b, "None of the %d changed files matches %q. They are:\n", len(ch.Files), files)
		for i, f := range ch.Files {
			if i == diffFiles {
				fmt.Fprintf(&b, "  … and %d more\n", len(ch.Files)-i)
				break
			}
			b.WriteString("  " + f.Path + "\n")
		}
		return b.String()
	}
	if len(shown) < len(ch.Files) {
		fmt.Fprintf(&b, "Files (%d of %d changed):\n", len(shown), len(ch.Files))
	} else {
		fmt.Fprintf(&b, "Files (%d):\n", len(shown))
	}
	for i, f := range shown {
		if i == diffFiles {
			fmt.Fprintf(&b, "  … and %d more\n", len(shown)-i)
			break
		}
		fmt.Fprintf(&b, "  %-2s %s", f.Status, f.Path)
		switch add, del := diffStat(f.Diff); {
		case f.Binary:
			b.WriteString("  (binary)")
		case f.Status == "?":
			fmt.Fprintf(&b, "  (new, untracked) +%d", add)
		default:
			fmt.Fprintf(&b, "  +%d -%d", add, del)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")

	budget := maxChars - b.Len()
	perFile := max(maxChars/3, 2000)
	var left, cut []string
	for _, f := range shown {
		d := f.Diff
		if f.Binary || d == "" {
			continue
		}
		if f.Status == "?" {
			d = "diff --git a/" + f.Path + " b/" + f.Path + " (new, untracked)\n" + d
		}
		if !strings.HasSuffix(d, "\n") {
			d += "\n"
		}
		short := len(d) > perFile
		if short {
			d = cutLines(d, perFile) + "… (the rest of this file's diff is left out)\n"
		}
		if len(d) > budget {
			left = append(left, f.Path)
			continue
		}
		if short {
			cut = append(cut, f.Path)
		}
		b.WriteString(d)
		budget -= len(d)
	}
	if len(left) > 0 || len(cut) > 0 {
		fmt.Fprintf(&b, "\n[Cut short to about %d characters.", maxChars)
		if len(cut) > 0 {
			fmt.Fprintf(&b, " Shortened: %s.", listPaths(cut))
		}
		if len(left) > 0 {
			fmt.Fprintf(&b, " Left out: %s.", listPaths(left))
		}
		b.WriteString(" Read them with `files` (and a larger max_chars).]")
	}
	return b.String()
}

func listPaths(ps []string) string {
	if len(ps) > 20 {
		return strings.Join(ps[:20], ", ") + fmt.Sprintf(" and %d more", len(ps)-20)
	}
	return strings.Join(ps, ", ")
}

// diffStat counts the added and removed lines of a one-file diff.
func diffStat(d string) (add, del int) {
	hunk := false
	for _, l := range strings.Split(d, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			hunk = true
		case !hunk:
		case strings.HasPrefix(l, "+"):
			add++
		case strings.HasPrefix(l, "-"):
			del++
		}
	}
	return add, del
}

// cutLines shortens s to at most n bytes, at a line end when there is one.
func cutLines(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if i := strings.LastIndexByte(s[:n], '\n'); i > 0 {
		return s[:i+1]
	}
	for n > 0 && n < len(s) && s[n]&0xc0 == 0x80 { // not inside a UTF-8 character
		n--
	}
	return s[:n] + "\n"
}

// fileFilter matches paths against read_diff's files: paths, folders, and
// glob patterns (* and ? within a name, ** across folders; a pattern
// without a / matches file names in any folder).
func fileFilter(pats []string) func(string) bool {
	var plain []string
	var globs []*regexp.Regexp
	for _, p := range pats {
		p = strings.TrimPrefix(strings.TrimSpace(p), "./")
		switch {
		case p == "":
		case strings.ContainsAny(p, "*?"):
			globs = append(globs, globRe(p))
		default:
			plain = append(plain, strings.TrimSuffix(p, "/"))
		}
	}
	if len(plain) == 0 && len(globs) == 0 {
		return func(string) bool { return true }
	}
	return func(path string) bool {
		for _, p := range plain {
			if path == p || strings.HasPrefix(path, p+"/") {
				return true
			}
		}
		for _, re := range globs {
			if re.MatchString(path) {
				return true
			}
		}
		return false
	}
}

func globRe(p string) *regexp.Regexp {
	var b strings.Builder
	if strings.Contains(p, "/") {
		b.WriteString("^")
	} else {
		b.WriteString("(^|/)")
	}
	rs := []rune(p)
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; {
		case c == '*' && i+1 < len(rs) && rs[i+1] == '*':
			i++
			if i+1 < len(rs) && rs[i+1] == '/' {
				i++
				b.WriteString("(.*/)?")
			} else {
				b.WriteString(".*")
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}
