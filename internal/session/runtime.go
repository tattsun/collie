package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/tattsun/collie/internal/agent"
	"github.com/tattsun/collie/internal/model"
)

// Runtime is the live state of one session: its timeline (loaded lazily)
// and, while running, the agent driver.
type Runtime struct {
	m  *Manager
	id string

	mu       sync.Mutex
	items    []*model.Item
	idx      map[string]int
	loaded   bool
	drv      agent.Driver
	cur      *sink // the driver's
	gen      int
	startGen int
	closing  bool
	// stopWhy is why Collie is stopping the agent with its container, for
	// the session to tell if it was at work (see interruptedText).
	stopWhy    string
	oom        oomBase // the container's OOM kill count when the agent started
	lastActive time.Time
	caps       model.Caps

	queue chan queued
	once  sync.Once
}

// queued is input waiting for the agent, with the files attached to it
// (copied for the agent on delivery, see stageInput).
type queued struct {
	agent.Input
	files []model.File
}

func newRuntime(m *Manager, id string) *Runtime {
	return &Runtime{m: m, id: id, idx: map[string]int{}, lastActive: time.Now(), queue: make(chan queued, 64)}
}

// ensureLoaded must be called with r.mu held.
func (r *Runtime) ensureLoaded() {
	if r.loaded {
		return
	}
	items, lines, err := r.m.st.LoadItems(r.id)
	if err != nil {
		log.Printf("session %s: load timeline: %v", r.id, err)
	}
	r.items = items
	r.idx = make(map[string]int, len(items))
	for i, it := range items {
		r.idx[it.ID] = i
		if r.drv == nil {
			finalize(it)
		}
	}
	r.loaded = true
	if lines > 2*len(items)+64 {
		if err := r.m.st.CompactItems(r.id, items); err != nil {
			log.Printf("session %s: compact: %v", r.id, err)
		}
	}
}

// finalize closes out an item whose producer is gone.
func finalize(it *model.Item) bool {
	changed := false
	if it.Ask != nil && it.Ask.State == "pending" {
		it.Ask.State = "cancelled"
		changed = true
	}
	if it.Tool != nil && it.Tool.Status == "running" {
		it.Tool.Status = "interrupted"
		changed = true
	}
	if !it.Done {
		it.Done = true
		changed = true
	}
	return changed
}

func persistable(it *model.Item) bool {
	switch it.Kind {
	case model.KindText, model.KindThinking:
		return it.Done
	}
	return true
}

func (r *Runtime) publishItem(it *model.Item) {
	b, _ := json.Marshal(map[string]any{"t": "item", "sid": r.id, "item": it})
	r.m.hub.publish(r.id, b)
}

// put must be called with r.mu held.
func (r *Runtime) put(it *model.Item) {
	if it.TS == 0 {
		it.TS = model.NowMs()
	}
	if i, ok := r.idx[it.ID]; ok {
		r.items[i] = it
	} else {
		r.idx[it.ID] = len(r.items)
		r.items = append(r.items, it)
	}
	r.lastActive = time.Now()
	if persistable(it) {
		if err := r.m.st.AppendItem(r.id, it); err != nil {
			log.Printf("session %s: append: %v", r.id, err)
		}
	}
	r.publishItem(it)
	if it.Kind == model.KindResult || it.Kind == model.KindError {
		r.m.noteRunItem(r.id, it)
	}
}

func (r *Runtime) snapshot(limit int) map[string]any {
	start := 0
	if len(r.items) > limit {
		start = len(r.items) - limit
	}
	return map[string]any{"t": "snap", "sid": r.id, "items": r.items[start:], "start": start, "caps": r.caps}
}

func (r *Runtime) page(before, limit int) ([]*model.Item, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureLoaded()
	if before <= 0 || before > len(r.items) {
		before = len(r.items)
	}
	start := before - limit
	if start < 0 {
		start = 0
	}
	out := make([]*model.Item, before-start)
	copy(out, r.items[start:before])
	return out, start
}

// worker delivers user input to the agent strictly in order, starting (or
// resuming) the agent process when needed.
func (r *Runtime) worker() {
	for q := range r.queue {
		d, err := r.ensureDriver()
		if err != nil {
			r.fail(err)
			continue
		}
		in, err := r.m.stageInput(r.id, q)
		if err != nil {
			r.fail(err)
			continue
		}
		if err := d.Send(in); err != nil {
			r.fail(err)
		}
	}
}

func (r *Runtime) enqueue(in agent.Input, files []model.File) {
	r.once.Do(func() { go r.worker() })
	r.queue <- queued{in, files}
}

func (r *Runtime) driver() agent.Driver {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drv
}

func (r *Runtime) ensureDriver() (agent.Driver, error) {
	if d := r.driver(); d != nil {
		return d, nil
	}
	d, err := r.startDriver(false)
	if err == nil {
		return d, nil
	}
	sess, _ := r.m.st.Session(r.id)
	var se setupError
	if sess.Agent != "claude" || errors.As(err, &se) {
		return nil, err
	}
	// Claude: the conversation file may be missing (never written) or
	// already exist (written by an earlier run); flip the mode once.
	log.Printf("session %s: claude start failed (%v); retrying", r.id, err)
	return r.startDriver(true)
}

func (r *Runtime) startDriver(flip bool) (agent.Driver, error) {
	sess, ok := r.m.st.Session(r.id)
	if !ok {
		return nil, errSessionGone
	}
	cfg := agent.Config{
		Cwd: sess.Cwd, Model: sess.Model, Effort: sess.Effort, Mode: sess.Mode,
		Ref: sess.AgentRef, Resume: sess.Started,
		// The agent runs for whoever started the session.
		Instructions: r.m.SessionPrefs(sess.Owner).Instructions,
	}

	if flip {
		if cfg.Resume {
			cfg.Resume = false
			cfg.Ref = model.NewUUID()
			r.sinkPut(&model.Item{ID: model.NewID(), Kind: model.KindNotice, Done: true,
				Text: "Could not resume the previous Claude conversation; started a fresh one."})
		} else {
			cfg.Resume = true
		}
		ref, resume := cfg.Ref, cfg.Resume
		r.m.updateSession(r.id, func(s *model.Session) { s.AgentRef = ref; s.Started = resume })
	}
	if sess.Agent == "claude" && cfg.Ref == "" {
		cfg.Ref = model.NewUUID()
		ref := cfg.Ref
		r.m.updateSession(r.id, func(s *model.Session) { s.AgentRef = ref })
	}

	if !r.m.sessionDrivable(sess) {
		// Whatever got here (a queued message, a schedule, Home): the
		// agent would run with the accounts of someone who lost them.
		return nil, setupError{errOwnerGone}
	}
	proj, _ := r.m.st.Project(sess.ProjectID)
	isolated := proj.Isolated()
	if !isolated && !r.m.isAdmin(sess.Owner) {
		// Defense in depth: a non-admin's agent never runs outside a container.
		return nil, setupError{errHostOnly}
	}
	if isolated {
		r.m.setStatus(r.id, model.StatusStarting)
		if err := r.m.ensureContainer(proj, sess.Owner, func(msg string) {
			r.sinkPut(&model.Item{ID: model.NewID(), Kind: model.KindNotice, Done: true, Text: msg})
		}); err != nil {
			return nil, setupError{err}
		}
		if sess.Agent == "claude" {
			// Before Claude Code starts: it loads plugins only at startup.
			r.m.syncPlugins(proj, sess.Owner, sess.Cwd, func(msg string) {
				r.sinkPut(&model.Item{ID: model.NewID(), Kind: model.KindNotice, Done: true, Text: msg})
			})
		}
	}

	// Collie's tools: show_media for every agent, and for Home the tools
	// that coordinate the projects. A container left running from before
	// it had them mounted lacks them (it is re-created once nothing is at
	// work in it).
	var toolCmd string
	var toolArgs []string
	allowed := mediaTools
	if proj.Home {
		writeHomeInstructions(proj.Path)
		allowed = append(slices.Clone(homeReadTools), mediaTools...)
	}
	if proj.Home || !isolated || !r.m.sb.ToolsMissing(proj.ID, sess.Owner) {
		c, args, err := r.m.collieTools(proj, sess)
		switch {
		case err == nil:
			toolCmd, toolArgs = c, args
		case proj.Home:
			return nil, setupError{fmt.Errorf("prepare Home tools: %w", err)}
		default:
			log.Printf("session %s: Collie's tools: %v", r.id, err)
		}
	}

	r.mu.Lock()
	r.gen++
	gen := r.gen
	r.startGen = gen
	r.closing, r.stopWhy = false, ""
	s := newSink(r, gen)
	r.cur = s
	if isolated {
		sb := r.m.sb
		execID := fmt.Sprintf("%s-%d", r.id, gen)
		owner := sess.Owner
		cfg.Wrap = func(argv []string, dir string) ([]string, func(string)) {
			return sb.Exec(proj.ID, owner, dir, execID, false, nil, argv)
		}
	}
	var d agent.Driver
	if sess.Agent == "codex" {
		cfg.Bin = r.m.cfg.CodexBin
		if isolated {
			cfg.Bin = "codex"
			if err := r.m.sb.PrepareCodex(proj.ID, sess.Owner); err != nil {
				log.Printf("session %s: prepare codex config: %v", r.id, err)
			}
		}
		if toolCmd != "" {
			cfg.ExtraArgs = collieCodexArgs(cfg.ExtraArgs, toolCmd, toolArgs)
		}
		d = agent.NewCodex(cfg, s)
	} else {
		cfg.Bin = r.m.cfg.ClaudeBin
		if isolated {
			cfg.Bin = "claude"
			cfg.ExtraArgs = r.m.sb.ClaudeArgs(sess.Owner)
		}
		if toolCmd != "" {
			cfg.ExtraArgs = collieClaudeArgs(cfg.ExtraArgs, toolCmd, toolArgs, allowed)
		}
		cfg.ExtraArgs = append(cfg.ExtraArgs, r.m.claudeStageArgs(proj, sess)...)
		d = agent.NewClaude(cfg, s)
	}
	r.drv = d
	r.mu.Unlock()

	r.m.setStatus(r.id, model.StatusStarting)
	err := d.Start()
	r.mu.Lock()
	if r.startGen == gen {
		r.startGen = 0
	}
	if err != nil && r.gen == gen {
		r.drv = nil
		r.gen++
	}
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if isolated {
		// To tell later whether the kernel killed it for want of memory.
		go r.noteOOMBase(gen, proj.ID, sess.Owner)
	}
	return d, nil
}

// setupError is a failure to prepare the environment (e.g. the container),
// as opposed to the agent itself failing to start.
type setupError struct{ err error }

func (e setupError) Error() string { return e.err.Error() }
func (e setupError) Unwrap() error { return e.err }

func (r *Runtime) sinkPut(it *model.Item) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureLoaded()
	r.put(it)
}

// pendingQuestion reports whether the latest open ask is a question (not
// an approval).
func (r *Runtime) pendingQuestion() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.items) - 1; i >= 0 && i >= len(r.items)-200; i-- {
		if it := r.items[i]; it.Ask != nil && it.Ask.State == "pending" {
			return it.Kind == model.KindQuestion
		}
	}
	return false
}

func (r *Runtime) fail(err error) {
	r.sinkPut(&model.Item{ID: model.NewID(), Kind: model.KindError, Done: true, Text: err.Error()})
	r.m.setStatus(r.id, model.StatusError)
}

// stop closes the agent process (if any) and waits for it to exit.
func (r *Runtime) stop() { r.interrupt("") }

// markStopping says that Collie is about to stop the agent (why, if it
// does so with the agent's container: see interruptedText), before its
// process may be killed with the container.
func (r *Runtime) markStopping(why string) {
	r.mu.Lock()
	r.closing = true
	if r.drv != nil && why != "" {
		r.stopWhy = why
	}
	r.mu.Unlock()
}

// interrupt is stop for when Collie stops the agent's container: if the
// session was at work, it then says why.
func (r *Runtime) interrupt(why string) {
	r.markStopping(why)
	if d := r.driver(); d != nil {
		d.Close()
	}
}

// ---- sink ----

// sink is how one run of an agent (one driver, one generation) writes to
// the timeline.
//
// The ids a driver uses come from the agent: its message and tool call
// ids, its request numbers. What an agent prints is not trusted (any
// process in its container can write to its stdout), so a driver only
// ever changes items it made itself in this run. An id that already names
// another item (a user's message, an approval of an earlier run, Collie's
// own notices) or that would change an item's kind gets a timeline id of
// its own instead, and the driver goes on using its id for that item.
type sink struct {
	r   *Runtime
	gen int
	// With r.mu held:
	ids     map[string]string // driver's id → timeline id, where they differ
	agentID map[string]string // the other way round
	mine    map[string]string // timeline ids this run made → their kind
}

func newSink(r *Runtime, gen int) *sink {
	return &sink{r: r, gen: gen, ids: map[string]string{}, agentID: map[string]string{}, mine: map[string]string{}}
}

func (s *sink) lock() bool {
	s.r.mu.Lock()
	if s.r.gen != s.gen {
		s.r.mu.Unlock()
		return false
	}
	s.r.ensureLoaded()
	return true
}

// timelineID is the item a driver's id stands for. s.r.mu must be held.
func (s *sink) timelineID(id string) string {
	if t, ok := s.ids[id]; ok {
		return t
	}
	return id
}

// own is the timeline id of an item this run made. s.r.mu must be held.
func (s *sink) own(id string) (string, string, bool) {
	id = s.timelineID(id)
	kind, ok := s.mine[id]
	if _, there := s.r.idx[id]; !there {
		ok = false
	}
	return id, kind, ok
}

func (s *sink) Put(it *model.Item) {
	if !s.lock() {
		return
	}
	defer s.r.mu.Unlock()
	switch it.Kind {
	case model.KindUser, model.KindMedia:
		// Only Collie writes the user's messages and the files shown.
		log.Printf("session %s: ignored a %s item from the agent", s.r.id, it.Kind)
		return
	}
	c := *it
	c.By, c.Via, c.Notes, c.Media, c.Code = "", "", nil, nil, ""
	agentID := c.ID
	id := s.timelineID(agentID)
	kind, mine := s.mine[id]
	_, there := s.r.idx[id]
	switch {
	case there && !mine:
		// Someone else's item: from now on the agent's id means this one.
		id = agentID + "~" + model.NewID()
		s.ids[agentID] = id
		s.agentID[id] = agentID
	case mine && kind != c.Kind:
		// The agent used one of its ids again for something else (a tool
		// call named like a pending approval, say): the first item keeps
		// the id, this one is shown apart.
		log.Printf("session %s: the agent reused %q for a %s item", s.r.id, agentID, c.Kind)
		id = agentID + "~" + model.NewID()
		s.agentID[id] = agentID
	}
	c.ID = id
	s.mine[id] = c.Kind
	s.r.put(&c)
}

// Update changes an item this run made. For any other id it reports false,
// so that the driver puts a new item (which gets an id of its own).
func (s *sink) Update(id string, fn func(*model.Item)) bool {
	if !s.lock() {
		return true
	}
	defer s.r.mu.Unlock()
	id, kind, ok := s.own(id)
	if !ok {
		return false
	}
	it := s.r.items[s.r.idx[id]].Clone()
	if it.Kind == model.KindTool && it.Tool == nil {
		it.Tool = &model.Tool{Status: "running"}
	}
	if (it.Kind == model.KindApproval || it.Kind == model.KindQuestion) && it.Ask == nil {
		it.Ask = &model.Ask{}
	}
	if err := applyUpdate(it, fn); err != nil {
		// E.g. a tool's result for an id the agent gave a text block.
		log.Printf("session %s: ignored an update of %s (%s) from the agent: %v", s.r.id, id, kind, err)
		return true
	}
	it.ID, it.Kind, it.Code = id, kind, ""
	s.r.put(it)
	return true
}

// applyUpdate runs a driver's change on an item, which may be of another
// kind than the driver expects (the agent chose the id).
func applyUpdate(it *model.Item, fn func(*model.Item)) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("%v", e)
		}
	}()
	fn(it)
	return nil
}

// agentAskID is the id the running agent knows a timeline item by, if this
// run of it made the item.
func (r *Runtime) agentAskID(id string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.cur
	if s == nil || s.gen != r.gen {
		return "", false
	}
	if _, ok := s.mine[id]; !ok {
		return "", false
	}
	if a, ok := s.agentID[id]; ok {
		return a, true
	}
	return id, true
}

func (s *sink) Append(id, field, text string) {
	if text == "" || !s.lock() {
		return
	}
	defer s.r.mu.Unlock()
	id, _, ok := s.own(id)
	if !ok {
		return
	}
	i := s.r.idx[id]
	it := s.r.items[i].Clone()
	switch field {
	case "output":
		if it.Tool == nil {
			return
		}
		if len(it.Tool.Output) > 256<<10 {
			return
		}
		it.Tool.Output += text
	default:
		it.Text += text
	}
	s.r.items[i] = it
	s.r.lastActive = time.Now()
	b, _ := json.Marshal(map[string]any{"t": "delta", "sid": s.r.id, "id": id, "f": field, "d": text})
	s.r.m.hub.publish(s.r.id, b)
}

func (s *sink) Status(st string) {
	s.r.mu.Lock()
	ok := s.r.gen == s.gen
	s.r.lastActive = time.Now()
	if ok && st == model.StatusIdle && s.r.loaded {
		// The turn is over: anything still marked running was cut off
		// (e.g. by an interrupt) and will never report back.
		for i := len(s.r.items) - 1; i >= 0 && i >= len(s.r.items)-200; i-- {
			it := s.r.items[i]
			if it.Tool != nil && it.Tool.Status == "running" {
				c := it.Clone()
				c.Tool.Status = "interrupted"
				c.Done = true
				s.r.put(c)
			}
		}
	}
	s.r.mu.Unlock()
	if ok {
		s.r.m.setStatus(s.r.id, st)
	}
}

func (s *sink) Meta(fn func(*model.Session)) {
	s.r.mu.Lock()
	ok := s.r.gen == s.gen
	s.r.mu.Unlock()
	if ok {
		s.r.m.updateSession(s.r.id, fn)
	}
}

func (s *sink) Caps(c model.Caps) {
	c = cleanCaps(c)
	s.r.mu.Lock()
	if s.r.gen != s.gen {
		s.r.mu.Unlock()
		return
	}
	if c.Commands != nil {
		s.r.caps.Commands = c.Commands
	}
	if c.Models != nil {
		s.r.caps.Models = c.Models
	}
	caps := s.r.caps
	b, _ := json.Marshal(map[string]any{"t": "caps", "sid": s.r.id, "caps": caps})
	s.r.m.hub.publish(s.r.id, b)
	s.r.mu.Unlock()
	if sess, ok := s.r.m.st.Session(s.r.id); ok {
		// Its owner's: the agent runs with their accounts and lists
		// their commands.
		s.r.m.reportCaps(sess.Owner, sess.Agent, c)
	}
}

func (s *sink) Usage(u model.Usage) {
	s.r.mu.Lock()
	ok := s.r.gen == s.gen
	s.r.mu.Unlock()
	if !ok {
		return
	}
	if sess, found := s.r.m.st.Session(s.r.id); found {
		s.r.m.reportUsage(sess.Owner, sess.Agent, u)
	}
}

func (s *sink) Blob(data []byte) string { return s.r.m.blob(s.r.id, data) }

func (s *sink) Exited(err error) {
	r := s.r
	r.mu.Lock()
	if r.gen != s.gen {
		r.mu.Unlock()
		return
	}
	duringStart, closing, base := r.startGen == s.gen, r.closing, r.oom
	r.mu.Unlock()
	shutdown := r.m.shutdown.Load()
	var msg, code string
	if err != nil && !closing && !duringStart && !shutdown {
		// May ask the container (podman exec): not with r.mu held.
		msg, code = r.m.explainExit(r.id, err, base, s.gen)
	}
	r.mu.Lock()
	if r.gen != s.gen {
		r.mu.Unlock()
		return
	}
	r.drv = nil
	r.gen++
	duringStart = r.startGen == s.gen
	closing = r.closing
	why := r.stopWhy
	r.stopWhy = ""
	r.ensureLoaded()
	for _, it := range r.items {
		if it.Done && (it.Ask == nil || it.Ask.State != "pending") && (it.Tool == nil || it.Tool.Status != "running") {
			continue
		}
		c := it.Clone()
		if finalize(c) {
			r.put(c)
		}
	}
	if shutdown {
		// Collie is stopping: the session stays as it was, for the next
		// start to tell (see interruptedByRestart).
		r.mu.Unlock()
		return
	}
	sess, _ := r.m.st.Session(r.id)
	wasBusy := sess.Status == model.StatusRunning || sess.Status == model.StatusWaiting
	// Background tasks end with the agent too.
	atWork := wasBusy || len(sess.Tasks) > 0
	interrupted := closing && why != "" && atWork && !duringStart
	if interrupted {
		r.put(&model.Item{ID: model.NewID(), Kind: model.KindError, Done: true, Text: interruptedText(why), Code: "interrupted"})
	}
	if err != nil && !closing && !duringStart {
		if msg == "" {
			msg = err.Error()
		}
		var xe *agent.ExitError
		if !atWork && code == "" && errors.As(err, &xe) && xe.Signal != 0 {
			msg = "" // e.g. killed while idle: nothing was lost
		}
		if msg != "" {
			r.put(&model.Item{ID: model.NewID(), Kind: model.KindError, Done: true, Text: msg, Code: code})
		}
	}
	r.mu.Unlock()
	if len(sess.Tasks) > 0 {
		// Its background tasks ended with it.
		r.m.updateSession(r.id, func(s *model.Session) { s.Tasks = nil })
	}
	if duringStart {
		return
	}
	if interrupted || (err != nil && !closing && atWork) {
		r.m.setStatus(r.id, model.StatusError)
	} else {
		r.m.setStatus(r.id, model.StatusStopped)
	}
}
