// Package session owns the live sessions: it starts/resumes agent drivers,
// keeps timelines, and fans events out to connected clients.
package session

import (
	"cmp"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tattsun/collie/internal/agent"
	"github.com/tattsun/collie/internal/gitutil"
	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
	"github.com/tattsun/collie/internal/store"
)

var errSessionGone = errors.New("session not found")

type Config struct {
	ClaudeBin   string
	CodexBin    string
	ProjectRoot string
	IdleTimeout time.Duration
	// HostOwner is the user who owns the server user's own agent state (the
	// first admin); legacy data is assigned to them.
	HostOwner string
}

type Manager struct {
	cfg Config
	st  *store.Store
	hub *Hub
	sb  *sandbox.Manager
	acc Access
	// hostOwner owns the server user's own agent state (the first admin);
	// only their isolated homes inherit it.
	hostOwner string

	// TermsInUse reports whether a user has terminals open in a project
	// (set by the server, which owns terminals).
	TermsInUse func(projectID, userID string) bool
	// CloseTerms closes a user's terminals in a project (set by the server).
	CloseTerms func(projectID, userID string)
	// UserActive reports whether a user may still use Collie (set by the
	// server); schedules of those who may not are skipped.
	UserActive func(userID string) bool
	ctrMu      sync.Mutex
	ctrActive  map[string]time.Time // "project/user" containers started by this process
	ctrEnsured map[string]time.Time // ...and when an agent or terminal last started one
	// Sessions Collie's last restart cut off, to be told (ReportRestart).
	restartMu sync.Mutex
	restarted []string
	// shutdown: Collie is stopping, so agents that end leave their
	// sessions as they were (see interruptedByRestart).
	shutdown atomic.Bool

	// Home: one per user (see home.go). toolLn are the agents' tools
	// sockets (see media.go), by toolScope.key.
	homeMu sync.Mutex
	toolLn map[string]net.Listener
	// Notifications to Home agents about the sessions they watch
	// (home_follow.go), by Home session.
	noteMu sync.Mutex
	notes  map[string]*noteBox
	held   map[string]*heldTurn // turn ends held back, by watched session

	mu  sync.Mutex
	rts map[string]*Runtime
	bmu sync.Mutex // serializes session update+broadcast so clients never see stale order

	// What agents offer (models, commands) by user, then agent, and what
	// Collie's own probe found (see caps.go).
	cmu    sync.Mutex
	caps   map[string]map[string]model.Caps
	probed map[string]model.Caps

	media mediaGate // files show_media copies at once (see media.go)

	// Plan usage by user, then agent (see usage.go).
	umu   sync.Mutex
	usage map[string]map[string]model.Usage

	// Schedules (see schedules.go): schedMu serializes starting runs and
	// changing schedules; clock stands in for time.Now in tests.
	schedMu sync.Mutex
	clock   func() time.Time

	// gitHelper, if set, stands in for git in isolated projects' containers
	// (tests).
	gitHelper func(p model.Project, uid string) gitutil.Ops

	// Notify, if set, hears when a session starts waiting for an approval
	// or answers ("approval" / "question"), finishes its turn ("done") or
	// fails ("error"), to tell its owner on devices where Collie is closed
	// (set by the server, which sends Web Push).
	Notify func(s model.Session, event string)
}

func NewManager(cfg Config, st *store.Store, hub *Hub, sb *sandbox.Manager) *Manager {
	m := &Manager{cfg: cfg, st: st, hub: hub, sb: sb, rts: map[string]*Runtime{},
		caps: map[string]map[string]model.Caps{}, probed: map[string]model.Caps{},
		usage:     map[string]map[string]model.Usage{},
		ctrActive: map[string]time.Time{}, ctrEnsured: map[string]time.Time{}, hostOwner: cfg.HostOwner, toolLn: map[string]net.Listener{}}
	if sb != nil {
		sb.SetNetRules(st.Project)
	}
	if cfg.HostOwner != "" {
		m.AdoptOrphans(cfg.HostOwner)
	}
	// Nothing is running after a restart: sessions that were at work say
	// why they stopped.
	m.restarted = m.interruptedByRestart()
	m.settleAll()
	for _, p := range st.Projects() {
		git := isRepo(p)
		st.UpdateProject(p.ID, func(x *model.Project) {
			x.Git = git
			x.Pending = nil // its containers are stopped (stopStaleContainers)
			// Its git stopped with Collie (or, in a container, may still
			// be running): it is to be cloned again.
			if x.Status == model.ProjectCloning {
				x.Status, x.Error = model.ProjectCloneFailed, cloneInterrupted
			}
		})
	}
	m.loadCaps()
	m.claimExisting()
	go m.reaper()
	go m.probeCaps()
	if m.sb != nil {
		m.sb.HoldStarts()
	}
	go func() {
		m.stopStaleContainers()
		// After that, so that it does not stop the containers of runs that
		// make up for missed times.
		m.runSchedules()
	}()
	return m
}

func (m *Manager) Sandbox() *sandbox.Manager { return m.sb }

// stopStaleContainers stops project containers left running by a previous
// collie process: nothing in them is attached any more, and the next start
// picks up configuration changes.
//
// Containers start only after this (HoldStarts): the migrations move files
// they mount (an agent resuming before its Codex thread moved would start a
// new one), and only the containers listed before them are stopped.
func (m *Manager) stopStaleContainers() {
	if m.sb == nil {
		return
	}
	defer m.sb.ReleaseStarts()
	if !m.sb.Available() {
		return
	}
	var stale []sandbox.ContainerInfo
	for _, c := range m.sb.All() {
		if c.Running {
			stale = append(stale, c)
		}
	}
	m.sb.MigrateLayout(m.hostOwner)
	m.sb.MigrateCodex(m.st.Projects())
	for _, c := range stale {
		_ = m.sb.Stop(c.ProjectID, c.UserID, false)
	}
}

// ensureContainer makes sure the user's container for the project (and
// the image) is up. The container is only re-created for configuration
// changes when nothing in it is in use.
// Notices for a session whose agent waits for its container.
const (
	noticeBuildingImage = "Building the agent container image (first time only — this takes a few minutes)…"
	noticeSlowStart     = "Starting the container. The first start after an image update can take a few minutes…"
)

// ensureContainer starts the user's container for the project; notify, if
// set, is told when that takes long (an image build, a slow first start).
func (m *Manager) ensureContainer(p model.Project, uid string, notify func(string)) error {
	if m.sb == nil || !m.sb.Available() {
		return errors.New("this project runs isolated, but podman is not installed on the server")
	}
	if err := folderThere(p); err != nil {
		return err
	}
	var onBuild func()
	if notify != nil {
		onBuild = func() { notify(noticeBuildingImage) }
	}
	image, err := m.sb.WaitImage(p.Image, 45*time.Minute, onBuild)
	if err != nil {
		return err
	}
	sp, err := m.specFor(p, uid, image)
	if err != nil {
		return err
	}
	if notify != nil {
		sp.OnSlowStart = func() { notify(noticeSlowStart) }
	}
	if err := m.sb.Ensure(sp, !m.projectBusy(p.ID, uid)); err != nil {
		return fmt.Errorf("start project container: %w", err)
	}
	now := time.Now()
	m.ctrMu.Lock()
	m.ctrActive[p.ID+"/"+uid] = now
	if m.ctrEnsured == nil {
		m.ctrEnsured = map[string]time.Time{}
	}
	m.ctrEnsured[p.ID+"/"+uid] = now
	m.ctrMu.Unlock()
	m.setPending(p.ID, uid, m.sb.Stale(p.ID, uid))
	return nil
}

// containerSpec is the container the user gets for the project now, for
// an image that is ready (no build is waited for).
func (m *Manager) containerSpec(p model.Project, uid string) (sandbox.Spec, error) {
	image, err := m.sb.WaitImage(p.Image, 0, nil)
	if err != nil {
		return sandbox.Spec{}, err
	}
	return m.specFor(p, uid, image)
}

func (m *Manager) specFor(p model.Project, uid, image string) (sandbox.Spec, error) {
	var dirs []string
	for _, s := range m.st.Sessions() {
		if s.ProjectID == p.ID {
			dirs = append(dirs, s.Cwd)
		}
	}
	sp := sandbox.Spec{Project: p, UserID: uid, SessionDirs: dirs, MigrateHostState: uid == m.hostOwner && m.hostOwner != "", Image: image}
	if p.Home {
		if err := m.homeSpec(&sp, uid); err != nil {
			return sp, fmt.Errorf("prepare Home: %w", err)
		}
	} else if err := m.toolsSpec(&sp, toolScope{uid: uid, project: p.ID}); err != nil {
		// The agents just go without Collie's tools.
		log.Printf("project %s: tools socket for %s: %v", p.ID, uid, err)
	}
	return sp, nil
}

// EnsureContainer is ensureContainer for terminals; it does not wait for
// an image build.
func (m *Manager) EnsureContainer(p model.Project, uid string) error {
	if m.sb == nil || !m.sb.Available() {
		return errors.New("podman is not installed on the server")
	}
	if _, err := m.sb.WaitImage(p.Image, 0, nil); err != nil {
		return err
	}
	return m.ensureContainer(p, uid, nil)
}

// projectBusy reports whether any of the user's agent processes or
// terminals in the project is running.
func (m *Manager) projectBusy(projectID, uid string) bool {
	if m.TermsInUse != nil && m.TermsInUse(projectID, uid) {
		return true
	}
	m.mu.Lock()
	rts := make([]*Runtime, 0, len(m.rts))
	for _, r := range m.rts {
		rts = append(rts, r)
	}
	m.mu.Unlock()
	for _, r := range rts {
		s, ok := m.st.Session(r.id)
		if !ok || s.ProjectID != projectID || s.Owner != uid {
			continue
		}
		if r.driver() != nil {
			return true
		}
	}
	return false
}

func (m *Manager) projectRuntimes(projectID string) []*Runtime {
	var out []*Runtime
	for _, s := range m.st.Sessions() {
		if s.ProjectID != projectID {
			continue
		}
		m.mu.Lock()
		r := m.rts[s.ID]
		m.mu.Unlock()
		if r != nil {
			out = append(out, r)
		}
	}
	return out
}

// StopProject stops every agent of the project (all users). With kill, an
// isolated project's containers are killed outright (the emergency stop).
// Any member of the project — or an admin — may do this.
func (m *Manager) StopProject(a Actor, projectID string, kill bool) error {
	p, ok := m.st.Project(projectID)
	if !ok || (!m.ProjectVisible(a.ID, p) && !a.Admin) {
		return ErrNotFound
	}
	why := whyStop
	if kill {
		why = whyKill
	}
	return m.stopProject(projectID, kill, why)
}

// stopProject stops the project's agents and containers; with why, the
// sessions at work get a record of it (see interruptedText).
func (m *Manager) stopProject(projectID string, kill bool, why string) error {
	p, ok := m.st.Project(projectID)
	if !ok {
		return ErrNotFound
	}
	rts := m.projectRuntimes(projectID)
	for _, r := range rts {
		r.markStopping(why)
	}
	var err error
	if p.Isolated() && m.sb != nil && kill {
		err = m.sb.Stop(projectID, "", true)
	}
	var wg sync.WaitGroup
	for _, r := range rts {
		wg.Add(1)
		go func() { defer wg.Done(); r.stop() }()
	}
	wg.Wait()
	if p.Isolated() && m.sb != nil && !kill {
		err = m.sb.Stop(projectID, "", false)
	}
	m.forgetContainer(projectID, "")
	return err
}

// KillContainer is the admin's emergency stop for one container.
func (m *Manager) KillContainer(projectID, uid string) error {
	for _, r := range m.projectRuntimes(projectID) {
		if s, ok := m.st.Session(r.id); ok && s.Owner == uid {
			r.markStopping(whyAdmin)
		}
	}
	if m.sb == nil {
		return nil
	}
	err := m.sb.Stop(projectID, uid, true)
	m.forgetContainer(projectID, uid)
	return err
}

func (m *Manager) Hub() *Hub           { return m.hub }
func (m *Manager) Store() *store.Store { return m.st }
func (m *Manager) Config() Config      { return m.cfg }

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
	if ok && !reflect.DeepEqual(s, before) {
		m.pushSession(s)
		m.trackRun(before, s)
		m.noteChange(before, s)
		m.pendingFreed(before, s)
	}
	return s, ok
}

func (m *Manager) setStatus(sid, status string) {
	var prev string
	s, ok := m.updateSession(sid, func(s *model.Session) {
		prev = s.Status
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
	if ev := statusEvent(prev, status); ok && ev != "" && m.Notify != nil {
		go m.notifyStatus(s, ev)
	}
}

// statusEvent names the status changes a session's owner is told about.
func statusEvent(prev, status string) string {
	switch {
	case prev == status:
		return ""
	case status == model.StatusWaiting:
		return "waiting"
	case status == model.StatusIdle && (prev == model.StatusRunning || prev == model.StatusWaiting):
		return "done"
	case status == model.StatusError:
		return "error"
	}
	return ""
}

func (m *Manager) notifyStatus(s model.Session, ev string) {
	if ev == "waiting" {
		ev = "approval"
		m.mu.Lock()
		r := m.rts[s.ID]
		m.mu.Unlock()
		if r != nil && r.pendingQuestion() {
			ev = "question"
		}
	}
	m.Notify(s, ev)
}

// ---- sessions ----

type CreateSessionReq struct {
	ProjectID string   `json:"projectId"`
	Shared    *bool    `json:"shared"` // group projects: visible to the group (default true)
	Agent     string   `json:"agent"`
	Mode      string   `json:"mode"`
	Model     string   `json:"model"`
	Effort    string   `json:"effort"`
	Title     string   `json:"title"`
	Prompt    string   `json:"prompt"`
	Images    []string `json:"images"`
	Files     []string `json:"files"`
	Worktree  bool     `json:"worktree"`
	// Set by schedules: the new session's id, and the schedule starting it.
	id, schedule string
}

func (m *Manager) CreateSession(a Actor, req CreateSessionReq) (model.Session, error) {
	return m.createSession(a, req, "")
}

// createSession: via marks the first message as sent by the Home agent
// ("home") or a schedule ("schedule").
func (m *Manager) createSession(a Actor, req CreateSessionReq, via string) (model.Session, error) {
	p, err := m.visibleProject(a.ID, req.ProjectID)
	if err != nil {
		return model.Session{}, err
	}
	if !p.Isolated() && !a.Admin {
		return model.Session{}, errHostOnly
	}
	if err := projectReady(p); err != nil {
		return model.Session{}, err
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
	req.Effort = strings.TrimSpace(req.Effort)
	if err := checkEffort(req.Agent, req.Effort); err != nil {
		return model.Session{}, err
	}
	sid := cmp.Or(req.id, model.NewID())
	now := model.NowMs()
	s := model.Session{
		ID: sid, ProjectID: p.ID, Agent: req.Agent, Mode: req.Mode,
		Model: strings.TrimSpace(req.Model), Effort: req.Effort,
		Cwd: p.Path, Status: model.StatusStopped, CreatedAt: now, UpdatedAt: now,
		Title: strings.TrimSpace(req.Title), Owner: a.ID, Schedule: req.schedule,
	}
	if p.OwnerGroup != "" {
		s.Shared = req.Shared == nil || *req.Shared
	}
	if s.Title == "" {
		s.Title = deriveTitle(req.Prompt)
	}
	if req.Worktree {
		// Grouped per project so an isolated project's container can mount
		// all of its worktrees at once.
		wt := filepath.Join(m.st.WorktreesDir(), p.ID, sid)
		if err := os.MkdirAll(filepath.Dir(wt), 0o700); err != nil {
			return model.Session{}, err
		}
		branch := "collie/" + sid[:8]
		base, err := m.git(p, a.ID, true).NewWorktree(p.Path, wt, branch, m.initialAuthor(a, p))
		if err != nil {
			return model.Session{}, err
		}
		s.Cwd, s.Worktree, s.Branch, s.Base = wt, true, branch, base
	} else if p.Isolated() {
		// Read rather than asked of git in the container, which the agent
		// starts in afterwards, in the background.
		s.Branch = gitutil.HeadBranch(p.Path)
	} else {
		s.Branch = gitutil.CurrentBranch(p.Path)
	}
	if s.Agent == "claude" {
		s.AgentRef = model.NewUUID()
	}
	m.st.PutSession(s)
	m.pushSession(s)
	if strings.TrimSpace(req.Prompt) != "" || len(req.Images) > 0 || len(req.Files) > 0 {
		if err := m.send(a, sid, req.Prompt, req.Images, req.Files, via); err != nil {
			return s, err
		}
	}
	s, _ = m.st.Session(sid)
	return s, nil
}

// Why a session can't get a worktree of its own (see gitutil.CheckWorktrees).
var (
	errNoRepo    = gitutil.ErrNoRepo
	errNoCommits = gitutil.ErrNoCommits
)

// initialAuthor authors the empty initial commit a repository without
// commits gets for its first worktree: whoever the actor's agents commit
// as there.
func (m *Manager) initialAuthor(a Actor, p model.Project) gitutil.Author {
	var name, email string
	if p.Isolated() && m.sb != nil {
		name, email = m.sb.GitIdentity(a.ID)
	}
	if name == "" || email == "" {
		name, email = a.Name, sandbox.NoReplyEmail(a.Name)
	}
	// On the host, git's own identity there comes first.
	return gitutil.Author{Name: name, Email: email, PreferGit: !p.Isolated()}
}

// isRepo reports whether the project is a git repository. For an isolated
// one it only looks: git would read the repository's configuration, which
// its agents can write.
func isRepo(p model.Project) bool {
	if p.Isolated() {
		return gitutil.HasGitDir(p.Path)
	}
	return gitutil.IsRepo(p.Path)
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

func (m *Manager) Send(a Actor, sid, text string, imageIDs, fileIDs []string) error {
	return m.send(a, sid, text, imageIDs, fileIDs, "")
}

func (m *Manager) send(a Actor, sid, text string, imageIDs, fileIDs []string, via string) error {
	sess, err := m.visibleSession(a.ID, sid)
	if err != nil {
		return err
	}
	if !m.sessionDrivable(sess) {
		return errOwnerGone
	}
	if err := m.hostCheck(a, sess); err != nil {
		return err
	}
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
	files, err := m.attachedFiles(fileIDs)
	if err != nil {
		return err
	}
	text = strings.TrimRight(text, " \t\r\n")
	if text == "" && len(imgs) == 0 && len(files) == 0 {
		return errors.New("empty message")
	}
	if err := m.claimSent(sid, imageIDs, files); err != nil {
		return err
	}
	it := &model.Item{ID: "u-" + model.NewID(), Kind: model.KindUser, Text: text, Images: imageIDs, Files: files, Done: true, By: a.ID, Via: via}
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
	r.enqueue(agent.Input{ItemID: it.ID, Text: text, Images: imgs}, files)
	return nil
}

// hostCheck refuses non-admins commanding an agent that runs directly on
// the server (that would hand them Collie's own user account).
func (m *Manager) hostCheck(a Actor, s model.Session) error {
	p, ok := m.st.Project(s.ProjectID)
	if ok && !p.Isolated() && !a.Admin {
		return errHostOnly
	}
	return nil
}

var errOwnerGone = errors.New("the person who started this session no longer has access to the project (or their account is disabled), so it is read-only")

func (m *Manager) Interrupt(a Actor, sid string) error {
	if _, err := m.visibleSession(a.ID, sid); err != nil {
		return err
	}
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

// StopTask stops one of the agent's background tasks.
func (m *Manager) StopTask(a Actor, sid, taskID string) error {
	if _, err := m.visibleSession(a.ID, sid); err != nil {
		return err
	}
	r, err := m.runtime(sid)
	if err != nil {
		return err
	}
	d, ok := r.driver().(agent.TaskStopper)
	if !ok {
		return errors.New("the agent is not running anymore")
	}
	return d.StopTask(taskID)
}

func (m *Manager) Reply(a Actor, sid, askID string, rep agent.Reply) error {
	sess, err := m.visibleSession(a.ID, sid)
	if err != nil {
		return err
	}
	if !m.sessionDrivable(sess) {
		return errOwnerGone
	}
	if err := m.hostCheck(a, sess); err != nil {
		return err
	}
	r, err := m.runtime(sid)
	if err != nil {
		return err
	}
	d := r.driver()
	if d == nil {
		return errors.New("the agent is not running anymore; send a message to resume")
	}
	// The request as the agent knows it (see sink).
	id, ok := r.agentAskID(askID)
	if !ok {
		return errors.New("this request is no longer pending")
	}
	return d.Reply(id, rep)
}

type PatchSessionReq struct {
	Shared   *bool   `json:"shared"`
	Title    *string `json:"title"`
	Mode     *string `json:"mode"`
	Model    *string `json:"model"`
	Archived *bool   `json:"archived"`
}

type modelSetter interface{ SetModel(string) error }

func (m *Manager) Patch(a Actor, sid string, req PatchSessionReq) (model.Session, error) {
	s, err := m.visibleSession(a.ID, sid)
	if err != nil {
		return s, err
	}
	if !m.sessionAdmin(a.ID, s) {
		return s, ErrForbidden
	}
	if req.Shared != nil {
		p, _ := m.st.Project(s.ProjectID)
		if p.OwnerGroup == "" {
			return s, errors.New("only sessions in group projects can be shared")
		}
		if s.Owner != a.ID {
			return s, errors.New("only the person who started the session can change who sees it")
		}
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
		if req.Shared != nil {
			x.Shared = *req.Shared
		}
	})
	return s, nil
}

func (m *Manager) Seen(a Actor, sid string) {
	if s, err := m.visibleSession(a.ID, sid); err == nil && s.Unread {
		m.updateSession(sid, func(x *model.Session) { x.Unread = false })
	}
}

func (m *Manager) Stop(a Actor, sid string) error {
	s, err := m.visibleSession(a.ID, sid)
	if err != nil {
		return err
	}
	if !m.sessionAdmin(a.ID, s) {
		return ErrForbidden
	}
	r, err := m.runtime(sid)
	if err != nil {
		return err
	}
	go r.stop()
	return nil
}

func (m *Manager) DeleteSession(a Actor, sid string, removeWorktree bool) error {
	s, err := m.visibleSession(a.ID, sid)
	if err != nil {
		return err
	}
	if !m.sessionAdmin(a.ID, s) {
		return ErrForbidden
	}
	return m.deleteSession(sid, removeWorktree)
}

func (m *Manager) deleteSession(sid string, removeWorktree bool) error {
	s, ok := m.st.Session(sid)
	if !ok {
		return errSessionGone
	}
	var rf refs
	if r, err := m.runtime(sid); err == nil {
		r.stop()
		rf = r.refs()
	}
	if s.Worktree && removeWorktree {
		if p, ok := m.st.Project(s.ProjectID); ok {
			m.removeWorktree(p, s)
		}
	}
	m.mu.Lock()
	delete(m.rts, sid)
	m.mu.Unlock()
	m.deleteAttachments(s, rf)
	m.st.DeleteSession(sid)
	m.pushSessionDeleted(sid)
	return nil
}

func (m *Manager) Items(a Actor, sid string, before, limit int) ([]*model.Item, int, error) {
	if _, err := m.visibleSession(a.ID, sid); err != nil {
		return nil, 0, err
	}
	r, err := m.runtime(sid)
	if err != nil {
		return nil, 0, err
	}
	items, start := r.page(before, limit)
	return items, start, nil
}

func (m *Manager) Changes(a Actor, sid string) (*gitutil.Changes, error) {
	s, err := m.visibleSession(a.ID, sid)
	if err != nil {
		return nil, err
	}
	p, ok := m.st.Project(s.ProjectID)
	if !ok {
		return nil, ErrNotFound
	}
	return m.git(p, a.ID, true).Diff(s.Cwd, s.Base)
}

// Subscribe registers the client for a session's live events and queues a
// snapshot first, atomically, so no event is lost or duplicated.
func (m *Manager) Subscribe(c *Client, sid string) error {
	if _, err := m.visibleSession(c.UserID, sid); err != nil {
		return err
	}
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

// Shutdown stops every agent process. Their sessions keep their status,
// so that the next start can tell those that were at work (see
// interruptedByRestart).
func (m *Manager) Shutdown() {
	m.shutdown.Store(true)
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

// BusySessions counts the sessions a restart would interrupt: agents that
// are starting or working, and those waiting for an answer — unless the
// question has gone unanswered for longer than staleWaiting.
func (m *Manager) BusySessions(staleWaiting time.Duration) int {
	cutoff := model.NowMs() - staleWaiting.Milliseconds()
	n := 0
	for _, s := range m.st.Sessions() {
		switch s.Status {
		case model.StatusStarting, model.StatusRunning:
			n++
		case model.StatusWaiting:
			if s.UpdatedAt >= cutoff {
				n++
			}
		}
	}
	return n
}

// reaper stops idle agent processes and unloads unused timelines.
func (m *Manager) reaper() {
	t := time.NewTicker(time.Minute)
	var swept time.Time
	for range t.C {
		if time.Since(swept) > time.Hour {
			m.sweepAttachments()
			swept = time.Now()
		}
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
			// Background tasks die with the agent process, and it is the one
			// told when they end: keep it while any are running.
			if d != nil && m.cfg.IdleTimeout > 0 && idleFor > m.cfg.IdleTimeout && s.Status == model.StatusIdle && len(s.Tasks) == 0 {
				r.closing = true
				go d.Close()
			}
			if d == nil && r.loaded && idleFor > 20*time.Minute && m.hub.subscribers(r.id) == 0 {
				r.items, r.idx, r.loaded = nil, map[string]int{}, false
			}
			r.mu.Unlock()
		}
		m.reapContainers()
		// E.g. an image that was updated meanwhile.
		m.applyPendingIn("")
	}
}

// reapContainers stops project containers nothing has used for a while.
func (m *Manager) reapContainers() {
	if m.sb == nil || m.cfg.IdleTimeout <= 0 {
		return
	}
	m.ctrMu.Lock()
	ids := make([]string, 0, len(m.ctrActive))
	for id := range m.ctrActive {
		ids = append(ids, id)
	}
	m.ctrMu.Unlock()
	for _, id := range ids {
		if pid, uid, _ := strings.Cut(id, "/"); m.projectBusy(pid, uid) || m.atWork(pid, uid) {
			m.ctrMu.Lock()
			m.ctrActive[id] = time.Now()
			m.ctrMu.Unlock()
			continue
		}
		m.ctrMu.Lock()
		last := m.ctrActive[id]
		m.ctrMu.Unlock()
		if time.Since(last) < m.cfg.IdleTimeout {
			continue
		}
		pid, uid, _ := strings.Cut(id, "/")
		m.stopAgents(pid, uid, whyIdle) // none, unless one got in just now
		if err := m.sb.Stop(pid, uid, false); err != nil {
			log.Printf("stop idle container %s: %v", id, err)
		}
		m.forgetContainer(pid, uid)
	}
}
