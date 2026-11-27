package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/tattsun/collie/internal/agent"
	"github.com/tattsun/collie/internal/session"
	"github.com/tattsun/collie/internal/term"
)

func agentModes() any { return agent.Modes }

func (s *Server) apiRoutes() {
	mx := s.mux
	mx.HandleFunc("POST /api/projects", s.createProject)
	mx.HandleFunc("DELETE /api/projects/{id}", s.deleteProject)
	mx.HandleFunc("PATCH /api/projects/{id}", s.patchProject)
	mx.HandleFunc("POST /api/projects/{id}/stop", s.stopProject)
	mx.HandleFunc("GET /api/projects/{id}/container", s.projectContainer)
	mx.HandleFunc("GET /api/projects/{id}/env", s.getProjectEnv)
	mx.HandleFunc("PUT /api/projects/{id}/env", s.putProjectEnv)
	mx.HandleFunc("GET /api/projects/{id}/changes", s.projectChanges)
	mx.HandleFunc("GET /api/dirs", s.dirs)

	mx.HandleFunc("POST /api/sessions", s.createSession)
	mx.HandleFunc("PATCH /api/sessions/{id}", s.patchSession)
	mx.HandleFunc("DELETE /api/sessions/{id}", s.deleteSession)
	mx.HandleFunc("POST /api/sessions/{id}/messages", s.sendMessage)
	mx.HandleFunc("POST /api/sessions/{id}/interrupt", s.interrupt)
	mx.HandleFunc("POST /api/sessions/{id}/stop", s.stop)
	mx.HandleFunc("POST /api/sessions/{id}/asks/{ask}", s.reply)
	mx.HandleFunc("GET /api/sessions/{id}/items", s.items)
	mx.HandleFunc("GET /api/sessions/{id}/changes", s.sessionChanges)

	mx.HandleFunc("POST /api/uploads", s.upload)
	mx.HandleFunc("GET /api/uploads/{name}", s.getUpload)

	mx.HandleFunc("GET /api/terms", s.listTerms)
	mx.HandleFunc("POST /api/terms", s.createTerm)
	mx.HandleFunc("DELETE /api/terms/{id}", s.deleteTerm)
	mx.HandleFunc("GET /api/terms/{id}/ws", s.termWS)
}

func ok(w http.ResponseWriter) { writeJSON(w, map[string]any{"ok": true}) }

// ---- projects ----

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var req session.CreateProjectReq
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	// Registering a server folder or running agents on the host reaches
	// beyond any container.
	if (req.Kind == "existing" || req.Runtime == "host") && actor(r).Admin && !s.verified(w, r) {
		return
	}
	p, err := s.m.CreateProject(actor(r), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, p)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	if err := s.m.DeleteProject(actor(r), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) patchProject(w http.ResponseWriter, r *http.Request) {
	var req session.PatchProjectReq
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if req.Runtime != nil && *req.Runtime == "host" && actor(r).Admin && !s.verified(w, r) {
		return
	}
	p, err := s.m.PatchProject(actor(r), r.PathValue("id"), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, p)
}

// stopProject stops all agents and shells of a project; with kill, its
// containers are killed immediately (emergency stop).
func (s *Server) stopProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kill bool `json:"kill"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	id := r.PathValue("id")
	if err := s.m.StopProject(actor(r), id, req.Kill); err != nil {
		fail(w, err)
		return
	}
	for _, t := range s.terms.List() {
		if t.Project == id {
			if tt, ok := s.terms.Get(t.ID); ok {
				tt.Close()
			}
		}
	}
	ok(w)
}

// projectContainer reports the project's containers (all members') usage.
func (s *Server) projectContainer(w http.ResponseWriter, r *http.Request) {
	a := actor(r)
	p, found := s.m.Store().Project(r.PathValue("id"))
	if !found || !s.m.ProjectVisible(a.ID, p) {
		fail(w, session.ErrNotFound)
		return
	}
	sb := s.m.Sandbox()
	if sb == nil {
		writeJSON(w, []any{})
		return
	}
	list := sb.Inspect(p.ID, "")
	res := []map[string]any{}
	for _, c := range list {
		u, _ := s.users.User(c.UserID)
		b, _ := json.Marshal(c)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		m["userName"] = u.DisplayName
		m["mine"] = c.UserID == a.ID
		res = append(res, m)
	}
	writeJSON(w, res)
}

func (s *Server) getProjectEnv(w http.ResponseWriter, r *http.Request) {
	env, err := s.m.ProjectEnv(actor(r), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"env": env})
}

func (s *Server) putProjectEnv(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Env string `json:"env"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if err := s.m.SetProjectEnv(actor(r), r.PathValue("id"), req.Env); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) projectChanges(w http.ResponseWriter, r *http.Request) {
	ch, err := s.m.ProjectChanges(actor(r), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, ch)
}

func (s *Server) dirs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"root": s.m.Config().ProjectRoot, "dirs": s.m.SuggestDirs(actor(r))})
}

// ---- sessions ----

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var req session.CreateSessionReq
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	sess, err := s.m.CreateSession(actor(r), req)
	if err != nil && sess.ID == "" {
		fail(w, err)
		return
	}
	writeJSON(w, sess)
}

func (s *Server) patchSession(w http.ResponseWriter, r *http.Request) {
	var req session.PatchSessionReq
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	sess, err := s.m.Patch(actor(r), r.PathValue("id"), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, sess)
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	rm := r.URL.Query().Get("worktree") != "keep"
	if err := s.m.DeleteSession(actor(r), r.PathValue("id"), rm); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text   string   `json:"text"`
		Images []string `json:"images"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if err := s.m.Send(actor(r), r.PathValue("id"), req.Text, req.Images); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	if err := s.m.Interrupt(actor(r), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	if err := s.m.Stop(actor(r), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) reply(w http.ResponseWriter, r *http.Request) {
	var req agent.Reply
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if err := s.m.Reply(actor(r), r.PathValue("id"), r.PathValue("ask"), req); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) items(w http.ResponseWriter, r *http.Request) {
	before, _ := strconv.Atoi(r.URL.Query().Get("before"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 300
	}
	items, start, err := s.m.Items(actor(r), r.PathValue("id"), before, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"items": items, "start": start})
}

func (s *Server) sessionChanges(w http.ResponseWriter, r *http.Request) {
	ch, err := s.m.Changes(actor(r), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, ch)
}

// ---- uploads ----

// Upload names carry 128 random bits; any signed-in user may fetch one by
// name (that is how images in shared sessions reach the other members).
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 25<<20)
	f, _, err := r.FormFile("file")
	if err != nil {
		httpError(w, 400, "missing file")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	name, err := s.m.SaveUpload(data)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"id": name, "url": "/api/uploads/" + name})
}

// Agents' HTML pages and SVG images are never served as documents of
// Collie's origin, where a script in them could act as the user. The CSP
// sandbox gives them an opaque origin even when opened directly (no
// cookies, storage or same-origin requests; the API refuses null and
// cross-site origins), and forbids forms, popups, dialogs and navigating
// the app. Nothing is loaded from the network or from other URLs, so
// nothing leaks out either. Scripts stay off unless the viewer asks for
// them (?js=1, HTML only); they then run inline-only, still sandboxed and
// offline.
const (
	sandboxCSP = "sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src data: blob:; media-src data: blob:; font-src data:; " +
		"base-uri 'none'; form-action 'none'; frame-ancestors 'self'"
	sandboxScriptCSP = "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline' 'unsafe-eval'; style-src 'unsafe-inline'; " +
		"img-src data: blob:; media-src data: blob:; font-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"
)

func (s *Server) getUpload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, ok := s.m.UploadPath(name)
	if !ok {
		httpError(w, 404, "not found")
		return
	}
	h := w.Header()
	h.Set("Content-Type", session.UploadType(name))
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	if session.UploadActive(name) {
		csp := sandboxCSP
		if r.URL.Query().Get("js") == "1" && strings.HasSuffix(name, ".html") {
			csp = sandboxScriptCSP
		}
		h.Set("Content-Security-Policy", csp)
		h.Set("Referrer-Policy", "no-referrer")
	}
	http.ServeFile(w, r, p)
}

// ---- terminals (each visible only to the user who opened it) ----

func (s *Server) listTerms(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.termsFor(current(r).ID))
}

func (s *Server) createTerm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID string `json:"projectId"`
		SessionID string `json:"sessionId"`
		Cols      int    `json:"cols"`
		Rows      int    `json:"rows"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	a := actor(r)
	sp := term.Spec{Cols: req.Cols, Rows: req.Rows, User: a.ID}
	projectID := req.ProjectID
	if req.SessionID != "" {
		sess, found := s.m.Store().Session(req.SessionID)
		if !found || !s.m.SessionVisible(a.ID, sess) {
			fail(w, session.ErrNotFound)
			return
		}
		sp.Cwd, sp.Title, sp.Owner = sess.Cwd, sess.Title, sess.ID
		projectID = sess.ProjectID
	}
	p, found := s.m.Store().Project(projectID)
	if !found || !s.m.ProjectVisible(a.ID, p) {
		fail(w, session.ErrNotFound)
		return
	}
	if req.SessionID == "" {
		sp.Cwd, sp.Title, sp.Owner = p.Path, p.Name, p.ID
	}
	sp.Project = p.ID
	if p.Isolated() {
		// The shell runs inside the user's container for the project.
		if err := s.m.EnsureContainer(p, a.ID); err != nil {
			httpError(w, 409, err.Error())
			return
		}
		sp.Argv, sp.Kill = s.m.Sandbox().Exec(p.ID, a.ID, sp.Cwd, "term-"+sp.ID(), true, nil, []string{"bash", "-l"})
	} else if !a.Admin {
		httpError(w, 403, "only admins can open shells on the server itself")
		return
	}
	t, err := s.terms.Create(sp)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, t.Info)
}

func (s *Server) ownTerm(r *http.Request) (*term.Term, bool) {
	t, ok := s.terms.Get(r.PathValue("id"))
	if !ok || t.User != current(r).ID {
		return nil, false
	}
	return t, true
}

func (s *Server) deleteTerm(w http.ResponseWriter, r *http.Request) {
	if t, ok := s.ownTerm(r); ok {
		t.Close()
	}
	ok(w)
}

func (s *Server) termWS(w http.ResponseWriter, r *http.Request) {
	t, found := s.ownTerm(r)
	if !found {
		httpError(w, 404, "terminal not found")
		return
	}
	u := current(r)
	sessHash, _ := r.Context().Value(sessKey).(string)
	legacy, _ := r.Context().Value(legacyKey).(bool)
	c, err := s.acceptWS(w, r)
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	replay, ch, detach := t.Attach()
	defer detach()
	go func() {
		defer cancel()
		if len(replay) > 0 {
			if err := c.Write(ctx, websocket.MessageBinary, replay); err != nil {
				return
			}
		}
		check := time.NewTicker(time.Minute)
		defer check.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-check.C:
				if !s.stillSignedIn(u.ID, sessHash, legacy) {
					_ = c.Close(4401, "signed out")
					return
				}
			case b, ok := <-ch:
				if !ok {
					select {
					case <-t.Done():
						_ = c.Close(4000, "exited")
					default:
						_ = c.Close(4001, "resync")
					}
					return
				}
				// Coalesce whatever else is already queued.
				var buf bytes.Buffer
				buf.Write(b)
			more:
				for buf.Len() < 256<<10 {
					select {
					case nb, ok := <-ch:
						if !ok {
							break more
						}
						buf.Write(nb)
					default:
						break more
					}
				}
				if err := c.Write(ctx, websocket.MessageBinary, buf.Bytes()); err != nil {
					return
				}
			}
		}
	}()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			_ = t.Write(data)
			continue
		}
		var msg struct {
			T string `json:"t"`
			D string `json:"d"`
			C int    `json:"c"`
			R int    `json:"r"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.T {
		case "in":
			_ = t.Write([]byte(msg.D))
		case "resize":
			t.Resize(msg.C, msg.R)
		}
	}
}
