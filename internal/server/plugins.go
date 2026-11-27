package server

import (
	"net/http"

	"github.com/tattsun/collie/internal/sandbox"
	"github.com/tattsun/collie/internal/session"
)

// Claude Code plugins: one definition per level (global, group, personal,
// project; see sandbox/plugins.go), each read as {plugins, canEdit} and
// replaced with {plugins}.

func (s *Server) pluginRoutes() {
	mx := s.mux
	mx.HandleFunc("GET /api/plugins/catalog", s.pluginCatalog)
	mx.HandleFunc("GET /api/shared/plugins", s.getSharedPlugins)
	mx.HandleFunc("PUT /api/shared/plugins", s.putSharedPlugins)
	mx.HandleFunc("GET /api/me/plugins", s.getMyPlugins)
	mx.HandleFunc("PUT /api/me/plugins", s.putMyPlugins)
	mx.HandleFunc("GET /api/groups/{id}/plugins", s.getGroupPlugins)
	mx.HandleFunc("PUT /api/groups/{id}/plugins", s.putGroupPlugins)
	mx.HandleFunc("GET /api/projects/{id}/plugins", s.getProjectPlugins)
	mx.HandleFunc("PUT /api/projects/{id}/plugins", s.putProjectPlugins)
	mx.HandleFunc("GET /api/projects/{id}/plugins/effective", s.effectivePlugins)
}

type pluginsReply struct {
	Plugins sandbox.PluginSet `json:"plugins"`
	CanEdit bool              `json:"canEdit"`
}

func (s *Server) sandboxOr400(w http.ResponseWriter) *sandbox.Manager {
	sb := s.m.Sandbox()
	if sb == nil {
		httpError(w, 400, "isolation is not available")
	}
	return sb
}

// readPlugins / writePlugins: a level kept in a file (access already checked).
func readPlugins(w http.ResponseWriter, path string, canEdit bool) {
	set, err := sandbox.ReadPluginSet(path)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, pluginsReply{Plugins: set, CanEdit: canEdit})
}

func decodePlugins(w http.ResponseWriter, r *http.Request) (sandbox.PluginSet, bool) {
	var req struct {
		Plugins sandbox.PluginSet `json:"plugins"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return req.Plugins, false
	}
	return req.Plugins, true
}

func writePlugins(w http.ResponseWriter, r *http.Request, path string) {
	set, found := decodePlugins(w, r)
	if !found {
		return
	}
	if err := sandbox.WritePluginSet(path, set); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) pluginCatalog(w http.ResponseWriter, r *http.Request) {
	sb := s.sandboxOr400(w)
	if sb == nil {
		return
	}
	list, err := sb.PluginCatalog()
	if err != nil {
		httpError(w, 503, err.Error())
		return
	}
	writeJSON(w, sandbox.SearchCatalog(list, r.URL.Query().Get("q"), 30))
}

func (s *Server) getSharedPlugins(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if sb := s.sandboxOr400(w); sb != nil {
		readPlugins(w, sb.GlobalPluginsPath(), true)
	}
}

func (s *Server) putSharedPlugins(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if sb := s.sandboxOr400(w); sb != nil {
		writePlugins(w, r, sb.GlobalPluginsPath())
	}
}

func (s *Server) getMyPlugins(w http.ResponseWriter, r *http.Request) {
	if sb := s.sandboxOr400(w); sb != nil {
		readPlugins(w, sb.PersonalPluginsPath(current(r).ID), true)
	}
}

func (s *Server) putMyPlugins(w http.ResponseWriter, r *http.Request) {
	if sb := s.sandboxOr400(w); sb != nil {
		writePlugins(w, r, sb.PersonalPluginsPath(current(r).ID))
	}
}

// Group plugins apply to the group's projects: members read them, owners
// (and admins) change them.
func (s *Server) groupPluginsAccess(w http.ResponseWriter, r *http.Request) (string, bool, bool) {
	gid := r.PathValue("id")
	u := current(r)
	member := s.users.Role(gid, u.ID) != ""
	if _, found := s.users.Group(gid); !found || (!member && !u.Admin) {
		fail(w, session.ErrNotFound)
		return "", false, false
	}
	return gid, s.canManageGroup(r, gid), true
}

func (s *Server) getGroupPlugins(w http.ResponseWriter, r *http.Request) {
	gid, canEdit, found := s.groupPluginsAccess(w, r)
	if !found {
		return
	}
	if sb := s.sandboxOr400(w); sb != nil {
		readPlugins(w, sb.GroupPluginsPath(gid), canEdit)
	}
}

func (s *Server) putGroupPlugins(w http.ResponseWriter, r *http.Request) {
	gid, canEdit, found := s.groupPluginsAccess(w, r)
	if !found {
		return
	}
	if !canEdit {
		fail(w, session.ErrForbidden)
		return
	}
	if sb := s.sandboxOr400(w); sb != nil {
		writePlugins(w, r, sb.GroupPluginsPath(gid))
	}
}

func (s *Server) getProjectPlugins(w http.ResponseWriter, r *http.Request) {
	set, canEdit, err := s.m.ProjectPlugins(actor(r), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, pluginsReply{Plugins: set, CanEdit: canEdit})
}

func (s *Server) putProjectPlugins(w http.ResponseWriter, r *http.Request) {
	set, found := decodePlugins(w, r)
	if !found {
		return
	}
	if err := s.m.SetProjectPlugins(actor(r), r.PathValue("id"), set); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) effectivePlugins(w http.ResponseWriter, r *http.Request) {
	eff, err := s.m.ProjectEffectivePlugins(actor(r), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, eff)
}
