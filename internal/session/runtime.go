package session

import (
	"encoding/json"
	"log"
	"strings"
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

	mu         sync.Mutex
	items      []*model.Item
	idx        map[string]int
	loaded     bool
	drv        agent.Driver
	gen        int
	startGen   int
	closing    bool
	lastActive time.Time
	caps       model.Caps

	queue chan agent.Input
	once  sync.Once
}

func newRuntime(m *Manager, id string) *Runtime {
	return &Runtime{m: m, id: id, idx: map[string]int{}, lastActive: time.Now(), queue: make(chan agent.Input, 64)}
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
	for in := range r.queue {
		d, err := r.ensureDriver()
		if err != nil {
			r.fail(err)
			continue
		}
		if err := d.Send(in); err != nil {
			r.fail(err)
		}
	}
}

func (r *Runtime) enqueue(in agent.Input) {
	r.once.Do(func() { go r.worker() })
	r.queue <- in
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
	if sess.Agent != "claude" {
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

	r.mu.Lock()
	r.gen++
	gen := r.gen
	r.startGen = gen
	r.closing = false
	s := &sink{r: r, gen: gen}
	var d agent.Driver
	if sess.Agent == "codex" {
		cfg.Bin = r.m.cfg.CodexBin
		d = agent.NewCodex(cfg, s)
	} else {
		cfg.Bin = r.m.cfg.ClaudeBin
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
	return d, nil
}

func (r *Runtime) sinkPut(it *model.Item) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureLoaded()
	r.put(it)
}

func (r *Runtime) fail(err error) {
	r.sinkPut(&model.Item{ID: model.NewID(), Kind: model.KindError, Done: true, Text: err.Error()})
	r.m.setStatus(r.id, model.StatusError)
}

// stop closes the agent process (if any) and waits for it to exit.
func (r *Runtime) stop() {
	r.mu.Lock()
	d := r.drv
	r.closing = true
	r.mu.Unlock()
	if d != nil {
		d.Close()
	}
}

// ---- sink ----

type sink struct {
	r   *Runtime
	gen int
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

func (s *sink) Put(it *model.Item) {
	if !s.lock() {
		return
	}
	defer s.r.mu.Unlock()
	s.r.put(it)
}

func (s *sink) Update(id string, fn func(*model.Item)) bool {
	if !s.lock() {
		return true
	}
	defer s.r.mu.Unlock()
	i, ok := s.r.idx[id]
	if !ok {
		return false
	}
	it := s.r.items[i].Clone()
	if it.Kind == model.KindTool && it.Tool == nil {
		it.Tool = &model.Tool{Status: "running"}
	}
	if (it.Kind == model.KindApproval || it.Kind == model.KindQuestion) && it.Ask == nil {
		it.Ask = &model.Ask{}
	}
	fn(it)
	s.r.put(it)
	return true
}

func (s *sink) Append(id, field, text string) {
	if text == "" || !s.lock() {
		return
	}
	defer s.r.mu.Unlock()
	i, ok := s.r.idx[id]
	if !ok {
		return
	}
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
		s.r.m.setAgentCaps(sess.Agent, c)
	}
}

func (s *sink) Exited(err error) {
	r := s.r
	r.mu.Lock()
	if r.gen != s.gen {
		r.mu.Unlock()
		return
	}
	r.drv = nil
	r.gen++
	duringStart := r.startGen == s.gen
	closing := r.closing
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
	sess, _ := r.m.st.Session(r.id)
	wasBusy := sess.Status == model.StatusRunning || sess.Status == model.StatusWaiting
	if err != nil && !closing && !duringStart {
		msg := err.Error()
		if !wasBusy && strings.HasPrefix(msg, "agent process exited: signal") {
			msg = ""
		}
		if msg != "" {
			r.put(&model.Item{ID: model.NewID(), Kind: model.KindError, Done: true, Text: msg})
		}
	}
	r.mu.Unlock()
	if duringStart {
		return
	}
	if err != nil && !closing && wasBusy {
		r.m.setStatus(r.id, model.StatusError)
	} else {
		r.m.setStatus(r.id, model.StatusStopped)
	}
}
