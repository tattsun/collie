// Package session owns the live sessions: it starts/resumes agent drivers,
// keeps timelines, and fans events out to connected clients.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tattsun/collie/internal/agent"
	"github.com/tattsun/collie/internal/gitutil"
	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/store"
)

var errSessionGone = errors.New("session not found")

type Config struct {
	ClaudeBin   string
	CodexBin    string
	ProjectRoot string
	IdleTimeout time.Duration
}

type Manager struct {
	cfg Config
	st  *store.Store
	hub *Hub

	mu   sync.Mutex
	rts  map[string]*Runtime
	caps map[string]model.Caps
	bmu  sync.Mutex // serializes session update+broadcast so clients never see stale order
}

func NewManager(cfg Config, st *store.Store, hub *Hub) *Manager {
	m := &Manager{cfg: cfg, st: st, hub: hub, rts: map[string]*Runtime{}, caps: map[string]model.Caps{}}
	// Nothing is running after a restart.
	for _, s := range st.Sessions() {
		if s.Status != model.StatusStopped && s.Status != model.StatusError {
			st.UpdateSession(s.ID, func(x *model.Session) { x.Status = model.StatusStopped })
		}
	}
	for _, p := range st.Projects() {
		git := gitutil.IsRepo(p.Path)
		st.UpdateProject(p.ID, func(x *model.Project) { x.Git = git })
	}
	m.loadCaps()
	go m.reaper()
	go m.probeCaps()
	return m
}

func (m *Manager) capsPath() string { return filepath.Join(m.st.Dir(), "caps.json") }

func (m *Manager) loadCaps() {
	b, err := os.ReadFile(m.capsPath())
	if err == nil {
		_ = json.Unmarshal(b, &m.caps)
	}
	if m.caps == nil {
		m.caps = map[string]model.Caps{}
	}
}

func (m *Manager) saveCaps() {
	m.mu.Lock()
	b, err := json.Marshal(m.caps)
	m.mu.Unlock()
	if err == nil {
		_ = os.WriteFile(m.capsPath(), b, 0o600)
	}
}

// probeCaps fills in the Codex model list on first run so the new-session
// form can offer models before any session has started. (Claude's list
// arrives with the first Claude session.)
func (m *Manager) probeCaps() {
	m.mu.Lock()
	_, have := m.caps["codex"]
	m.mu.Unlock()
	if have {
		return
	}
	if _, err := exec.LookPath(m.cfg.CodexBin); err != nil {
		return
	}
	home, _ := os.UserHomeDir()
	caps, err := agent.ProbeCodex(m.cfg.CodexBin, home)
	if err != nil {
		log.Printf("codex model probe: %v", err)
		return
	}
	if len(caps.Models) > 0 {
		m.setAgentCaps("codex", caps)
	}
}

func (m *Manager) Hub() *Hub                 { return m.hub }
func (m *Manager) Store() *store.Store       { return m.st }
func (m *Manager) Config() Config            { return m.cfg }
func (m *Manager) Projects() []model.Project { return m.st.Projects() }

func (m *Manager) Sessions() []model.Session {
	all := m.st.Sessions()
	return all
}

func (m *Manager) AgentCaps() map[string]model.Caps {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]model.Caps, len(m.caps))
	for k, v := range m.caps {
		out[k] = v
	}
	return out
}

func (m *Manager) setAgentCaps(agentName string, c model.Caps) {
	m.mu.Lock()
	cur := m.caps[agentName]
	if c.Commands != nil {
		cur.Commands = c.Commands
	}
	if c.Models != nil {
		cur.Models = c.Models
	}
	m.caps[agentName] = cur
	m.mu.Unlock()
	m.saveCaps()
	m.hub.Broadcast(map[string]any{"t": "agentcaps", "agent": agentName, "caps": cur})
}

func (m *Manager) runtime(sid string) (*Runtime, error) {
	if _, ok := m.st.Session(sid); !ok {
		return nil, errSessionGone
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rts[sid]
	if !ok {
		r = newRuntime(m, sid)
		m.rts[sid] = r
	}
	return r, nil
}

func (m *Manager) updateSession(sid string, fn func(*model.Session)) (model.Session, bool) {
	m.bmu.Lock()
	defer m.bmu.Unlock()
	before, _ := m.st.Session(sid)
	s, ok := m.st.UpdateSession(sid, fn)
	if ok && s != before {
		m.hub.Broadcast(map[string]any{"t": "session", "session": s})
	}
	return s, ok
}

func (m *Manager) setStatus(sid, status string) {
	m.updateSession(sid, func(s *model.Session) {
		prev := s.Status
		s.Status = status
		if status == model.StatusIdle && (prev == model.StatusRunning || prev == model.StatusWaiting) {
			s.Unread = true
			s.UpdatedAt = model.NowMs()
		}
		if status == model.StatusWaiting {
			s.Unread = true
			s.UpdatedAt = model.NowMs()
		}
	})
}

// ---- sessions ----

type CreateSessionReq struct {
	ProjectID string   `json:"projectId"`
	Agent     string   `json:"agent"`
	Mode      string   `json:"mode"`
	Model     string   `json:"model"`
	Effort    string   `json:"effort"`
	Title     string   `json:"title"`
	Prompt    string   `json:"prompt"`
	Images    []string `json:"images"`
	Worktree  bool     `json:"worktree"`
}

func (m *Manager) CreateSession(req CreateSessionReq) (model.Session, error) {
	p, ok := m.st.Project(req.ProjectID)
	if !ok {
		return model.Session{}, errors.New("project not found")
	}
	if p.Status == "cloning" {
		return model.Session{}, errors.New("project is still being cloned")
	}
	if req.Agent != "claude" && req.Agent != "codex" {
		return model.Session{}, errors.New("agent must be claude or codex")
	}
	if req.Mode == "" {
		req.Mode = agent.DefaultMode(req.Agent)
	}
	if !validMode(req.Agent, req.Mode) {
		return model.Session{}, fmt.Errorf("unknown mode %q", req.Mode)
	}
	sid := model.NewID()
	now := model.NowMs()
	s := model.Session{
		ID: sid, ProjectID: p.ID, Agent: req.Agent, Mode: req.Mode,
		Model: strings.TrimSpace(req.Model), Effort: strings.TrimSpace(req.Effort),
		Cwd: p.Path, Status: model.StatusStopped, CreatedAt: now, UpdatedAt: now,
		Title: strings.TrimSpace(req.Title),
	}
	if s.Title == "" {
		s.Title = deriveTitle(req.Prompt)
	}
	if req.Worktree {
		if !gitutil.IsRepo(p.Path) || !gitutil.HasCommits(p.Path) {
			return model.Session{}, errors.New("an isolated worktree needs a git repository with at least one commit")
		}
		wt := filepath.Join(m.st.WorktreesDir(), sid)
		branch := "collie/" + sid[:8]
		base := gitutil.HeadCommit(p.Path)
		if err := gitutil.AddWorktree(p.Path, wt, branch); err != nil {
			return model.Session{}, err
		}
		s.Cwd, s.Worktree, s.Branch, s.Base = wt, true, branch, base
	} else {
		s.Branch = gitutil.CurrentBranch(p.Path)
	}
	if s.Agent == "claude" {
		s.AgentRef = model.NewUUID()
	}
	m.st.PutSession(s)
	m.hub.Broadcast(map[string]any{"t": "session", "session": s})
	if strings.TrimSpace(req.Prompt) != "" || len(req.Images) > 0 {
		if err := m.Send(sid, req.Prompt, req.Images); err != nil {
			return s, err
		}
	}
	s, _ = m.st.Session(sid)
	return s, nil
}

func deriveTitle(prompt string) string {
	t := strings.TrimSpace(prompt)
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[:i]
	}
	r := []rune(t)
	if len(r) > 60 {
		t = string(r[:59]) + "…"
	}
	if t == "" {
		t = "New session"
	}
	return t
}

func validMode(agentName, mode string) bool {
	for _, x := range agent.Modes[agentName] {
		if x.ID == mode {
			return true
		}
	}
	return agentName == "claude" && mode == "dontAsk"
}

var uploadRe = regexp.MustCompile(`^[0-9a-f]{16}\.(png|jpe?g|gif|webp)$`)

func (m *Manager) UploadPath(id string) (string, bool) {
	if !uploadRe.MatchString(id) {
		return "", false
	}
	return filepath.Join(m.st.UploadsDir(), id), true
}

func (m *Manager) Send(sid, text string, imageIDs []string) error {
	r, err := m.runtime(sid)
	if err != nil {
		return err
	}
	var imgs []agent.Image
	for _, id := range imageIDs {
		p, ok := m.UploadPath(id)
		if !ok {
			return fmt.Errorf("bad image id %q", id)
		}
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("image %q not found", id)
		}
		imgs = append(imgs, agent.Image{Path: p, Mime: mime.TypeByExtension(filepath.Ext(p))})
	}
	text = strings.TrimRight(text, " \t\r\n")
	if text == "" && len(imgs) == 0 {
		return errors.New("empty message")
	}
	it := &model.Item{ID: "u-" + model.NewID(), Kind: model.KindUser, Text: text, Images: imageIDs, Done: true}
	r.mu.Lock()
	r.ensureLoaded()
	r.put(it)
	r.mu.Unlock()
	m.updateSession(sid, func(s *model.Session) {
		s.UpdatedAt = model.NowMs()
		if s.Archived {
			s.Archived = false
		}
	})
	r.enqueue(agent.Input{ItemID: it.ID, Text: text, Images: imgs})
	return nil
}

func (m *Manager) Interrupt(sid string) error {
	r, err := m.runtime(sid)
	if err != nil {
		return err
	}
	d := r.driver()
	if d == nil {
		return nil
	}
	return d.Interrupt()
}

func (m *Manager) Reply(sid, askID string, rep agent.Reply) error {
	r, err := m.runtime(sid)
	if err != nil {
		return err
	}
	d := r.driver()
	if d == nil {
		return errors.New("the agent is not running anymore; send a message to resume")
	}
	return d.Reply(askID, rep)
}

type PatchSessionReq struct {
	Title    *string `json:"title"`
	Mode     *string `json:"mode"`
	Model    *string `json:"model"`
	Archived *bool   `json:"archived"`
}

type modelSetter interface{ SetModel(string) error }

func (m *Manager) Patch(sid string, req PatchSessionReq) (model.Session, error) {
	s, ok := m.st.Session(sid)
	if !ok {
		return s, errSessionGone
	}
	r, err := m.runtime(sid)
	if err != nil {
		return s, err
	}
	if req.Mode != nil && *req.Mode != s.Mode {
		if !validMode(s.Agent, *req.Mode) {
			return s, fmt.Errorf("unknown mode %q", *req.Mode)
		}
		if d := r.driver(); d != nil {
			if err := d.SetMode(*req.Mode); err != nil {
				return s, err
			}
		}
	}
	if req.Model != nil && *req.Model != s.Model {
		if d, ok := r.driver().(modelSetter); ok && d != nil {
			if err := d.SetModel(*req.Model); err != nil {
				return s, err
			}
		}
	}
	s, _ = m.updateSession(sid, func(x *model.Session) {
		if req.Title != nil && strings.TrimSpace(*req.Title) != "" {
			x.Title = strings.TrimSpace(*req.Title)
		}
		if req.Mode != nil {
			x.Mode = *req.Mode
		}
		if req.Model != nil {
			x.Model = strings.TrimSpace(*req.Model)
		}
		if req.Archived != nil {
			x.Archived = *req.Archived
		}
	})
	return s, nil
}

func (m *Manager) Seen(sid string) {
	if s, ok := m.st.Session(sid); ok && s.Unread {
		m.updateSession(sid, func(x *model.Session) { x.Unread = false })
	}
}

func (m *Manager) Stop(sid string) error {
	r, err := m.runtime(sid)
	if err != nil {
		return err
	}
	go r.stop()
	return nil
}

func (m *Manager) DeleteSession(sid string, removeWorktree bool) error {
	s, ok := m.st.Session(sid)
	if !ok {
		return errSessionGone
	}
	if r, err := m.runtime(sid); err == nil {
		r.stop()
	}
	if s.Worktree && removeWorktree {
		if p, ok := m.st.Project(s.ProjectID); ok {
			if err := gitutil.RemoveWorktree(p.Path, s.Cwd); err != nil {
				log.Printf("remove worktree %s: %v", s.Cwd, err)
			}
		}
	}
	m.mu.Lock()
	delete(m.rts, sid)
	m.mu.Unlock()
	m.st.DeleteSession(sid)
	m.hub.Broadcast(map[string]any{"t": "session_del", "id": sid})
	return nil
}

func (m *Manager) Items(sid string, before, limit int) ([]*model.Item, int, error) {
	r, err := m.runtime(sid)
	if err != nil {
		return nil, 0, err
	}
	items, start := r.page(before, limit)
	return items, start, nil
}

func (m *Manager) Changes(sid string) (*gitutil.Changes, error) {
	s, ok := m.st.Session(sid)
	if !ok {
		return nil, errSessionGone
	}
	return gitutil.Diff(s.Cwd, s.Base)
}

// Subscribe registers the client for a session's live events and queues a
// snapshot first, atomically, so no event is lost or duplicated.
func (m *Manager) Subscribe(c *Client, sid string) error {
	r, err := m.runtime(sid)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureLoaded()
	snap := r.snapshot(300)
	b, err := jsonMarshal(snap)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.subs[sid] = true
	c.mu.Unlock()
	c.enqueue(b)
	return nil
}

func (m *Manager) Unsubscribe(c *Client, sid string) {
	c.mu.Lock()
	delete(c.subs, sid)
	c.mu.Unlock()
}

// Shutdown stops every agent process.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	rts := make([]*Runtime, 0, len(m.rts))
	for _, r := range m.rts {
		rts = append(rts, r)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, r := range rts {
		wg.Add(1)
		go func() { defer wg.Done(); r.stop() }()
	}
	wg.Wait()
	_ = m.st.Flush()
}

// reaper stops idle agent processes and unloads unused timelines.
func (m *Manager) reaper() {
	t := time.NewTicker(time.Minute)
	for range t.C {
		m.mu.Lock()
		rts := make([]*Runtime, 0, len(m.rts))
		for _, r := range m.rts {
			rts = append(rts, r)
		}
		m.mu.Unlock()
		for _, r := range rts {
			s, ok := m.st.Session(r.id)
			if !ok {
				continue
			}
			r.mu.Lock()
			idleFor := time.Since(r.lastActive)
			d := r.drv
			if d != nil && m.cfg.IdleTimeout > 0 && idleFor > m.cfg.IdleTimeout && s.Status == model.StatusIdle {
				r.closing = true
				go d.Close()
			}
			if d == nil && r.loaded && idleFor > 20*time.Minute && m.hub.subscribers(r.id) == 0 {
				r.items, r.idx, r.loaded = nil, map[string]int{}, false
			}
			r.mu.Unlock()
		}
	}
}
