package session

import (
	"sync"

	"github.com/tattsun/collie/internal/model"
)

// MayUse reports whether uid may still have agents, terminals and a
// container in p: they are active, they can see the project, and for a
// project that runs on the server itself, they are an admin. (Home runs
// on the server only where there is no podman, for everyone alike.)
func (m *Manager) MayUse(uid string, p model.Project) bool {
	if m.UserActive != nil && !m.UserActive(uid) {
		return false
	}
	if !m.ProjectVisible(uid, p) {
		return false
	}
	return p.Isolated() || p.Home || m.isAdmin(uid)
}

// enforcing keeps EnforceAccess runs (one per change of users or groups)
// from overlapping.
var enforcing sync.Mutex

// EnforceAccess takes away what users lost when users or groups changed
// (a removal from a group, a demotion, a disabled account), or while
// Collie was not running: agents of sessions whose owner may no longer use
// the project stop, and so do the owner's containers there. A Home
// container that can still read a project its user lost is stopped too,
// so that its next start mounts only what they may see. Without this, a
// running agent, a background job or an open shell would keep the files.
func (m *Manager) EnforceAccess() {
	enforcing.Lock()
	defer enforcing.Unlock()
	m.mu.Lock()
	rts := make(map[string]*Runtime, len(m.rts))
	for sid, r := range m.rts {
		rts[sid] = r
	}
	m.mu.Unlock()
	for sid, r := range rts {
		s, ok := m.st.Session(sid)
		if !ok {
			continue
		}
		if p, ok := m.st.Project(s.ProjectID); ok && !m.MayUse(s.Owner, p) {
			r.stop()
		}
	}
	if m.sb == nil || !m.sb.Available() {
		return
	}
	for _, c := range m.sb.List() {
		if !c.Running || c.UserID == "" {
			continue
		}
		p, ok := m.st.Project(c.ProjectID)
		if !ok {
			continue
		}
		if !m.MayUse(c.UserID, p) || (p.Home && m.homeReadsLost(c.UserID, p)) {
			m.dropContainer(p, c.UserID)
		}
	}
}

// homeReadsLost reports whether the user's Home container still mounts a
// project the user may no longer see.
func (m *Manager) homeReadsLost(uid string, home model.Project) bool {
	have, err := m.sb.ReadOnlyMounts(home.ID, uid)
	if err != nil {
		return false
	}
	want := map[string]bool{}
	for _, d := range m.homeMounts(uid) {
		want[d] = true
	}
	projects := map[string]bool{}
	for _, p := range m.st.Projects() {
		projects[p.Path] = true
	}
	for _, d := range have {
		if projects[d] && !want[d] {
			return true
		}
	}
	return false
}

// dropContainer stops the user's agents in the project and kills their
// container (with every shell and background job in it).
func (m *Manager) dropContainer(p model.Project, uid string) {
	for _, r := range m.projectRuntimes(p.ID) {
		if s, ok := m.st.Session(r.id); ok && s.Owner == uid {
			r.stop()
		}
	}
	_ = m.sb.Stop(p.ID, uid, true)
	m.ctrMu.Lock()
	delete(m.ctrActive, p.ID+"/"+uid)
	m.ctrMu.Unlock()
}
