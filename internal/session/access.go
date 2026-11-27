package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
)

// Who can see and do what:
//
//   - A personal project (OwnerUser) is visible only to its owner.
//   - A group project (OwnerGroup) is visible to the group's members; group
//     owners manage it (rename, secrets, delete).
//   - A session is visible to the user who started it and — if it belongs
//     to a group project and is Shared — to every member of the group, who
//     can also drive it (send, approve, interrupt). Its agent always runs
//     with the starter's agent accounts.
//   - Admins manage users, groups and the server (containers, global
//     profile) but do not see other people's projects or conversations.
//
// Invisible things are reported as "not found", not "forbidden", so their
// existence does not leak.

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("you are not allowed to do that")
)

// Actor is the signed-in user performing an operation.
type Actor struct {
	ID    string
	Name  string // login name (also their project folder)
	Admin bool
}

// Access answers questions about users and groups (implemented by the
// auth store).
type Access interface {
	Role(groupID, userID string) string // "owner", "member" or ""
	GroupName(groupID string) string
	IsAdmin(userID string) bool
}

func (m *Manager) isAdmin(uid string) bool { return m.acc != nil && m.acc.IsAdmin(uid) }

// errHostOnly: agents of non-isolated projects run directly on the server
// as Collie's user, which only admins may command.
var errHostOnly = errors.New("this project runs directly on the server; only admins can run or instruct its agents")

// toolActor is who the agents' tools socket acts for. Home's agent runs in
// a container and reads what other agents wrote, which could steer it, so
// it never gets an admin's say over host-run projects: their agents run on
// the server itself. (A host-run Home, on a server without Podman, runs
// there anyway.)
func (m *Manager) toolActor(uid string) Actor {
	a := Actor{ID: uid, Admin: m.isAdmin(uid)}
	if h, ok := m.homeOf(uid); !ok || h.Isolated() {
		a.Admin = false
	}
	return a
}

var errHomeHostRun = errors.New("this project runs directly on the server and Home runs in a container, so Home can't start or instruct its agents; the user can, in Collie")

// unattendedMode reports whether a permission mode lets an agent act
// without asking, which Home may not pick.
func unattendedMode(mode string) bool {
	switch strings.TrimSpace(mode) {
	case "bypassPermissions", "dontAsk", "full":
		return true
	}
	return false
}

func (m *Manager) SetAccess(a Access) { m.acc = a }

func (m *Manager) role(groupID, userID string) string {
	if m.acc == nil || groupID == "" || userID == "" {
		return ""
	}
	return m.acc.Role(groupID, userID)
}

func (m *Manager) ProjectVisible(uid string, p model.Project) bool {
	if p.OwnerUser != "" {
		return p.OwnerUser == uid
	}
	return m.role(p.OwnerGroup, uid) != ""
}

// ProjectManageable: rename, secrets, delete.
func (m *Manager) ProjectManageable(uid string, p model.Project) bool {
	if p.OwnerUser != "" {
		return p.OwnerUser == uid
	}
	return m.role(p.OwnerGroup, uid) == "owner"
}

func (m *Manager) SessionVisible(uid string, s model.Session) bool {
	p, ok := m.st.Project(s.ProjectID)
	if !ok || !m.ProjectVisible(uid, p) {
		return false
	}
	return s.Owner == uid || (p.OwnerGroup != "" && s.Shared)
}

// sessionAdmin: settings, stop, delete — the starter, or a group owner.
func (m *Manager) sessionAdmin(uid string, s model.Session) bool {
	if s.Owner == uid {
		return true
	}
	p, ok := m.st.Project(s.ProjectID)
	return ok && p.OwnerGroup != "" && m.role(p.OwnerGroup, uid) == "owner" && m.SessionVisible(uid, s)
}

// sessionDrivable: the session's agent identity is still valid — its
// starter still has access to the project, and their account is not
// disabled (the agent runs with their accounts and tokens).
func (m *Manager) sessionDrivable(s model.Session) bool {
	p, ok := m.st.Project(s.ProjectID)
	return ok && m.ProjectVisible(s.Owner, p) && m.userActive(s.Owner)
}

func (m *Manager) userActive(uid string) bool { return m.UserActive == nil || m.UserActive(uid) }

func (m *Manager) visibleSession(uid, sid string) (model.Session, error) {
	s, ok := m.st.Session(sid)
	if !ok || !m.SessionVisible(uid, s) {
		return model.Session{}, ErrNotFound
	}
	return s, nil
}

func (m *Manager) visibleProject(uid, pid string) (model.Project, error) {
	p, ok := m.st.Project(pid)
	if !ok || !m.ProjectVisible(uid, p) {
		return model.Project{}, ErrNotFound
	}
	return p, nil
}

// VisibleProjects / VisibleSessions build a user's view of the state.
func (m *Manager) VisibleProjects(uid string) []model.Project {
	out := []model.Project{}
	for _, p := range m.st.Projects() {
		if m.ProjectVisible(uid, p) {
			out = append(out, p)
		}
	}
	return out
}

func (m *Manager) VisibleSessions(uid string) []model.Session {
	out := []model.Session{}
	for _, s := range m.st.Sessions() {
		if m.SessionVisible(uid, s) {
			out = append(out, s)
		}
	}
	return out
}

// ---- per-client fan-out ----

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// pushSession sends a session update to the clients who may see it, and a
// removal to clients who could see it before but no longer can.
func (m *Manager) pushSession(s model.Session) {
	upd := mustJSON(map[string]any{"t": "session", "session": s})
	del := mustJSON(map[string]any{"t": "session_del", "id": s.ID})
	m.hub.each(func(c *Client) {
		if m.SessionVisible(c.UserID, s) {
			c.know("s:" + s.ID)
			c.enqueue(upd)
		} else if c.forget("s:" + s.ID) {
			c.unsubscribe(s.ID)
			c.enqueue(del)
		}
	})
}

func (m *Manager) pushSessionDeleted(sid string) {
	del := mustJSON(map[string]any{"t": "session_del", "id": sid})
	m.hub.each(func(c *Client) {
		if c.forget("s:" + sid) {
			c.unsubscribe(sid)
			c.enqueue(del)
		}
	})
}

func (m *Manager) pushProject(p model.Project) {
	upd := mustJSON(map[string]any{"t": "project", "project": p})
	del := mustJSON(map[string]any{"t": "project_del", "id": p.ID})
	m.hub.each(func(c *Client) {
		if m.ProjectVisible(c.UserID, p) {
			c.know("p:" + p.ID)
			c.enqueue(upd)
		} else if c.forget("p:" + p.ID) {
			c.enqueue(del)
		}
	})
}

func (m *Manager) pushProjectDeleted(pid string) {
	del := mustJSON(map[string]any{"t": "project_del", "id": pid})
	m.hub.each(func(c *Client) {
		if c.forget("p:" + pid) {
			c.enqueue(del)
		}
	})
}

// Revalidate re-checks every client's view after users or groups changed:
// it drops subscriptions the user lost access to and sends a fresh state
// built by stateFor.
func (m *Manager) Revalidate(stateFor func(uid string) any) {
	m.hub.each(func(c *Client) {
		c.mu.Lock()
		subs := make([]string, 0, len(c.subs))
		for sid := range c.subs {
			subs = append(subs, sid)
		}
		c.mu.Unlock()
		for _, sid := range subs {
			if s, ok := m.st.Session(sid); !ok || !m.SessionVisible(c.UserID, s) {
				c.unsubscribe(sid)
			}
		}
		st := stateFor(c.UserID)
		if st == nil { // the user is gone
			go c.kill()
			return
		}
		c.resetKnown(m.VisibleProjects(c.UserID), m.VisibleSessions(c.UserID))
		c.enqueue(mustJSON(st))
	})
}

// PreviewSession returns a session whose servers the user may open in
// their browser (package preview): only their own sessions, even in a
// shared group session, and for host-run projects only if they're an admin.
func (m *Manager) PreviewSession(uid, sid string) (model.Session, model.Project, error) {
	s, err := m.visibleSession(uid, sid)
	if err != nil {
		return model.Session{}, model.Project{}, err
	}
	if s.Owner != uid {
		return model.Session{}, model.Project{}, errNotOwner
	}
	p, ok := m.st.Project(s.ProjectID)
	if !ok {
		return model.Session{}, model.Project{}, ErrNotFound
	}
	if !p.Isolated() && !m.isAdmin(uid) {
		return model.Session{}, model.Project{}, fmt.Errorf("%w: %v", ErrForbidden, errHostOnly)
	}
	if p.Isolated() && (m.sb == nil || !m.sb.Available()) {
		return model.Session{}, model.Project{}, sandbox.ErrUnavailable
	}
	return s, p, nil
}

var errNotOwner = fmt.Errorf("%w: only the session's owner can open its servers", ErrForbidden)

// TouchContainer counts a use of the user's container for the project
// (a preview in use), so it isn't stopped as idle.
func (m *Manager) TouchContainer(projectID, uid string) {
	k := projectID + "/" + uid
	m.ctrMu.Lock()
	if _, ok := m.ctrActive[k]; ok {
		m.ctrActive[k] = time.Now()
	}
	m.ctrMu.Unlock()
}
