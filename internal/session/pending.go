package session

import (
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tattsun/collie/internal/agent"
	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
)

// Changes that need a new container (a project's limits, its image, an
// update of the image, containers inside, a Home's project list) reach a
// user's running container for the project as soon as nothing is at work
// in it: no agent starting, working, waiting for an answer or with
// background tasks, and no terminal open. Collie then stops it, along with
// agent processes that are only idle (they resume on the next message),
// and the next agent or terminal gets a container that matches. Until
// then the container is pending (model.Project.Pending), which the
// project's settings and its sessions show, and anyone in the project can
// restart it at once (RestartContainers), stopping what runs in it.
//
// A change of what the containers can reach (the network setting) and a
// move between a container and the server can't wait: they restart the
// project's containers right away.

// Why Collie stopped an agent with its container (see interruptedText).
const (
	whyApply   = "apply"   // restarted to apply new settings or a new image
	whyNetwork = "network" // restarted for another network setting
	whyRuntime = "runtime" // the project moved between a container and the server
	whyStop    = "stop"    // someone stopped the project (Stop everything)
	whyKill    = "kill"    // someone killed the project's containers (Kill now)
	whyAdmin   = "admin"   // an admin killed the container (Admin → Containers)
	whyIdle    = "idle"    // nothing had used the container for a while
	whyAccess  = "access"  // its owner can no longer use the project
	whyRestart = "restart" // Collie itself restarted
)

// interruptedText is what a session at work is told when Collie stopped
// its agent (the web app translates these).
func interruptedText(why string) string {
	switch why {
	case whyApply:
		return "Stopped by a restart of the project's container (reason: applying changed settings or a new image). Send a message to resume."
	case whyNetwork:
		return "Stopped by a restart of the project's container (reason: a change of its network setting). Send a message to resume."
	case whyRuntime:
		return "Stopped because the project moved between a container and the server. Send a message to resume there."
	case whyStop:
		return "Stopped when the project's containers were stopped (Stop everything). Send a message to resume."
	case whyKill:
		return "Stopped when the project's containers were killed (Kill now). Send a message to resume."
	case whyAdmin:
		return "Stopped when an admin killed the project's container. Send a message to resume."
	case whyIdle:
		return "Stopped when the project's container was stopped for being idle. Send a message to resume."
	case whyAccess:
		return "Stopped because access to the project changed."
	case whyRestart:
		return "Stopped because Collie restarted (for an update, say). Send a message to resume."
	}
	return "Stopped by a restart of the project's container. Send a message to resume."
}

// atWork reports whether anything of the user's in the project is at work
// in their container: an agent starting, working, waiting for an answer
// or with background tasks, an open terminal, or the container started
// only just now (an agent or a terminal may be on its way in).
func (m *Manager) atWork(projectID, uid string) bool {
	if m.TermsInUse != nil && m.TermsInUse(projectID, uid) {
		return true
	}
	m.ctrMu.Lock()
	recent := time.Since(m.ctrEnsured[projectID+"/"+uid]) < 30*time.Second
	m.ctrMu.Unlock()
	if recent {
		return true
	}
	for _, s := range m.st.Sessions() {
		if s.ProjectID != projectID || s.Owner != uid {
			continue
		}
		switch s.Status {
		case model.StatusStarting, model.StatusRunning, model.StatusWaiting:
			return true
		}
		if len(s.Tasks) > 0 {
			return true
		}
	}
	return false
}

// applyPending looks at the user's running container for the project (see
// sandbox.Apply): when it still runs with an older configuration and
// nothing is at work in it, its idle agents and the container stop.
func (m *Manager) applyPending(p model.Project, uid string) {
	if m.sb == nil || !m.sb.Available() || !p.Isolated() {
		return
	}
	sp, err := m.containerSpec(p, uid)
	if err != nil {
		return
	}
	switch m.sb.Apply(sp, func() bool {
		if m.atWork(p.ID, uid) {
			return false
		}
		m.stopAgents(p.ID, uid, "") // only idle ones: nothing to tell
		return true
	}) {
	case sandbox.Pending:
		m.setPending(p.ID, uid, true)
	case sandbox.Applied:
		log.Printf("project %s: stopped %s's container to apply its new settings", p.ID, uid)
		m.forgetContainer(p.ID, uid)
	case sandbox.Current:
		m.setPending(p.ID, uid, false)
	}
}

// applyPendingIn runs applyPending for the running containers of one
// project ("" = all) and those already pending.
func (m *Manager) applyPendingIn(projectID string) {
	if m.sb == nil || !m.sb.Available() {
		return
	}
	keys := map[string]bool{}
	m.ctrMu.Lock()
	for k := range m.ctrActive {
		keys[k] = true
	}
	m.ctrMu.Unlock()
	for _, p := range m.st.Projects() {
		for _, uid := range p.Pending {
			keys[p.ID+"/"+uid] = true
		}
	}
	for k := range keys {
		pid, uid, _ := strings.Cut(k, "/")
		if projectID != "" && pid != projectID {
			continue
		}
		if p, ok := m.st.Project(pid); ok {
			m.applyPending(p, uid)
		} else {
			m.forgetContainer(pid, uid)
		}
	}
}

// ApplyPending looks again at the containers waiting to apply new settings
// (after a terminal closed, say).
func (m *Manager) ApplyPending() {
	for _, p := range m.st.Projects() {
		for _, uid := range p.Pending {
			m.applyPending(p, uid)
		}
	}
}

// pendingFreed is called on every session update: a pending container may
// have nothing at work in it any more.
func (m *Manager) pendingFreed(before, s model.Session) {
	if before.Status == s.Status && len(before.Tasks) == len(s.Tasks) {
		return
	}
	switch s.Status {
	case model.StatusIdle, model.StatusStopped, model.StatusError:
	default:
		return
	}
	if len(s.Tasks) > 0 {
		return
	}
	if p, ok := m.st.Project(s.ProjectID); ok && slices.Contains(p.Pending, s.Owner) {
		go m.applyPending(p, s.Owner)
	}
}

// setPending records whether the user's container for the project is
// pending, and tells those who see the project.
func (m *Manager) setPending(projectID, uid string, on bool) {
	p, ok := m.st.Project(projectID)
	if !ok || slices.Contains(p.Pending, uid) == on {
		return
	}
	np, ok := m.st.UpdateProject(projectID, func(x *model.Project) {
		x.Pending = slices.DeleteFunc(slices.Clone(x.Pending), func(u string) bool { return u == uid })
		if on {
			x.Pending = append(x.Pending, uid)
			slices.Sort(x.Pending)
		}
		if len(x.Pending) == 0 {
			x.Pending = nil
		}
	})
	if ok {
		m.pushProject(np)
	}
}

// forgetContainer: the user's container for the project is stopped (or
// gone), so nothing about it is pending.
func (m *Manager) forgetContainer(projectID, uid string) {
	m.ctrMu.Lock()
	if uid == "" {
		for k := range m.ctrActive {
			if strings.HasPrefix(k, projectID+"/") {
				delete(m.ctrActive, k)
			}
		}
	} else {
		delete(m.ctrActive, projectID+"/"+uid)
	}
	m.ctrMu.Unlock()
	p, ok := m.st.Project(projectID)
	if !ok || len(p.Pending) == 0 {
		return
	}
	if uid == "" {
		if np, ok := m.st.UpdateProject(projectID, func(x *model.Project) { x.Pending = nil }); ok {
			m.pushProject(np)
		}
		return
	}
	m.setPending(projectID, uid, false)
}

// RestartContainers restarts the project's pending containers (only the
// user's with uid) to apply what they wait for, stopping the agents,
// terminals and anything else running in them. The agents' sessions say
// so; they resume on their next message, in a new container. It returns
// whose containers it restarted. Anyone in the project may do this, as
// they may stop it.
func (m *Manager) RestartContainers(a Actor, projectID, uid string) ([]string, error) {
	p, ok := m.st.Project(projectID)
	if !ok || (!m.ProjectVisible(a.ID, p) && !a.Admin) {
		return nil, ErrNotFound
	}
	var users []string
	for _, u := range p.Pending {
		if uid == "" || u == uid {
			users = append(users, u)
		}
	}
	for _, u := range users {
		if m.CloseTerms != nil {
			m.CloseTerms(p.ID, u)
		}
		m.stopAgents(p.ID, u, whyApply)
		if m.sb != nil {
			if err := m.sb.Stop(p.ID, u, false); err != nil {
				return users, fmt.Errorf("stop the container: %w", err)
			}
		}
		m.forgetContainer(p.ID, u)
	}
	return users, nil
}

// stopAgents stops the agents of the user's sessions in the project (all
// users' with uid ""), and waits for them. With why, sessions at work get
// a record of it (see interruptedText); without, only idle ones stop.
func (m *Manager) stopAgents(projectID, uid, why string) {
	var wg sync.WaitGroup
	for _, r := range m.projectRuntimes(projectID) {
		s, ok := m.st.Session(r.id)
		if !ok || (uid != "" && s.Owner != uid) || r.driver() == nil {
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); r.interrupt(why) }()
	}
	wg.Wait()
}

// ---- why an agent ended ----

// oomBase is the OOM kill count of an agent's container when the agent
// started (for one run of it, gen).
type oomBase struct {
	gen int
	n   int64
}

// noteOOMBase reads the OOM kill count of the container a run of the
// session's agent started in.
func (r *Runtime) noteOOMBase(gen int, projectID, uid string) {
	n, err := r.m.sb.OOMKills(projectID, uid)
	if err != nil {
		return
	}
	r.mu.Lock()
	if r.gen == gen {
		r.oom = oomBase{gen: gen, n: n}
	}
	r.mu.Unlock()
}

// explainExit says why an agent ended unexpectedly, when its container
// tells more than its exit status: the kernel killed it for going over the
// container's memory limit (its OOM kill count went up while the agent
// ran, and SIGKILL ended it), or the container itself stopped. It returns
// the message and its model.Item code ("" for the exit status itself).
func (m *Manager) explainExit(sid string, err error, base oomBase, gen int) (string, string) {
	var xe *agent.ExitError
	s, ok := m.st.Session(sid)
	if !ok || !errors.As(err, &xe) || m.sb == nil {
		return err.Error(), ""
	}
	p, ok := m.st.Project(s.ProjectID)
	if !ok || !p.Isolated() {
		return err.Error(), ""
	}
	if xe.Signal == syscall.SIGKILL {
		before := int64(0)
		if base.gen == gen {
			before = base.n
		}
		if n, err := m.sb.OOMKills(p.ID, s.Owner); err == nil && n > before {
			limit, _ := m.sb.MemoryLimit(p.ID, s.Owner)
			return oomText(limit), "oom"
		}
	}
	if !m.sb.Running(p.ID, s.Owner) {
		return "The agent stopped because the project's container stopped. Send a message to resume.", ""
	}
	return err.Error(), ""
}

func oomText(limit int64) string {
	if limit <= 0 {
		return "The agent was killed because the server ran out of memory (the project's container has no memory limit of its own). Send a message to resume. To avoid it, run fewer memory-hungry jobs at once (builds, tests, browsers)."
	}
	return fmt.Sprintf("The agent was killed because the project's container went over its memory limit (%s). Send a message to resume. To avoid it, run fewer memory-hungry jobs at once (builds, tests, browsers), or have an admin raise the project's memory limit.", formatBytes(limit))
}

// formatBytes shows a memory limit the way limits are set (4g is 4 GB).
func formatBytes(n int64) string {
	const gb, mb = 1 << 30, 1 << 20
	switch {
	case n >= gb && n%gb == 0:
		return fmt.Sprintf("%d GB", n/gb)
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/gb)
	case n >= mb:
		return fmt.Sprintf("%d MB", n/mb)
	}
	return fmt.Sprintf("%d bytes", n)
}

// ---- Collie restarting ----

// interruptedByRestart records, when Collie starts, that the sessions
// whose agents were at work when it stopped (or crashed) were cut off,
// and returns them; see ReportRestart.
func (m *Manager) interruptedByRestart() []string {
	var ids []string
	now := model.NowMs()
	for _, s := range m.st.Sessions() {
		atWork := s.Status == model.StatusStarting || s.Status == model.StatusRunning || s.Status == model.StatusWaiting || len(s.Tasks) > 0
		switch {
		case atWork:
			it := &model.Item{ID: model.NewID(), Kind: model.KindError, TS: now, Done: true, Text: interruptedText(whyRestart), Code: "interrupted"}
			if err := m.st.AppendItem(s.ID, it); err != nil {
				log.Printf("session %s: append: %v", s.ID, err)
			}
			m.st.UpdateSession(s.ID, func(x *model.Session) {
				x.Status, x.Tasks, x.Unread, x.UpdatedAt = model.StatusError, nil, true, now
			})
			ids = append(ids, s.ID)
		case s.Status != model.StatusStopped && s.Status != model.StatusError:
			m.st.UpdateSession(s.ID, func(x *model.Session) { x.Status = model.StatusStopped })
		}
	}
	return ids
}

// ReportRestart tells the owners of the sessions Collie's restart cut off
// (on their devices, see Notify) and the Home conversations watching them,
// as for a session that failed. The server calls it once it is set up.
func (m *Manager) ReportRestart() {
	m.restartMu.Lock()
	ids := m.restarted
	m.restarted = nil
	m.restartMu.Unlock()
	for _, sid := range ids {
		s, ok := m.st.Session(sid)
		if !ok || s.Status != model.StatusError {
			continue // it went on in the meantime
		}
		m.bmu.Lock()
		m.noteChange(model.Session{Status: model.StatusRunning}, s)
		m.bmu.Unlock()
		if m.Notify != nil {
			go m.Notify(s, "error")
		}
	}
}
