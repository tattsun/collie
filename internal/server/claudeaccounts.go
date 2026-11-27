package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/tattsun/collie/internal/sandbox"
	"github.com/tattsun/collie/internal/term"
)

func (s *Server) claudeRoutes(mx *http.ServeMux) {
	mx.HandleFunc("GET /api/me/claude/accounts", s.listClaudeAccounts)
	mx.HandleFunc("POST /api/me/claude/accounts", s.saveClaudeAccount)
	mx.HandleFunc("PUT /api/me/claude/accounts/{id}", s.saveClaudeAccount)
	mx.HandleFunc("DELETE /api/me/claude/accounts/{id}", s.deleteClaudeAccount)
	mx.HandleFunc("PUT /api/me/claude/default", s.setClaudeDefault)
	mx.HandleFunc("PUT /api/me/claude/projects/{pid}", s.setClaudeProject)
	mx.HandleFunc("POST /api/me/claude/token", s.saveClaudeToken)
	mx.HandleFunc("POST /api/me/claude/login", s.startClaudeLogin)
	mx.HandleFunc("GET /api/me/claude/login/{id}", s.getClaudeLogin)
	mx.HandleFunc("POST /api/me/claude/login/{id}/code", s.claudeLoginCode)
	mx.HandleFunc("DELETE /api/me/claude/login/{id}", s.cancelClaudeLogin)
}

func (s *Server) listClaudeAccounts(w http.ResponseWriter, r *http.Request) {
	sb := s.m.Sandbox()
	if sb == nil {
		writeJSON(w, sandbox.AccountsView{Accounts: []sandbox.AccountView{}, Projects: map[string]string{}})
		return
	}
	v, err := sb.Accounts(current(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, v)
}

func (s *Server) saveClaudeAccount(w http.ResponseWriter, r *http.Request) {
	sb := s.m.Sandbox()
	if sb == nil {
		httpError(w, 400, "isolation is not available")
		return
	}
	var in sandbox.AccountInput
	if err := decode(r, &in); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	in.ID = r.PathValue("id")
	id, err := sb.SaveAccount(current(r).ID, in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id})
}

func (s *Server) deleteClaudeAccount(w http.ResponseWriter, r *http.Request) {
	sb := s.m.Sandbox()
	if sb == nil {
		httpError(w, 400, "isolation is not available")
		return
	}
	if err := sb.DeleteAccount(current(r).ID, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) setClaudeDefault(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	sb := s.m.Sandbox()
	if err := decode(r, &req); err != nil || sb == nil {
		httpError(w, 400, "bad request")
		return
	}
	if err := sb.SetDefaultAccount(current(r).ID, req.ID); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) setClaudeProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if err := s.m.SetClaudeProjectAccount(current(r).ID, r.PathValue("pid"), req.ID); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

// saveClaudeToken stores a pasted `claude setup-token` token (into the
// given subscription account, else the first one, else a new one).
func (s *Server) saveClaudeToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
		ID    string `json:"id"`
		Name  string `json:"name"`
	}
	sb := s.m.Sandbox()
	if err := decode(r, &req); err != nil || sb == nil {
		httpError(w, 400, "bad request")
		return
	}
	id, err := sb.SaveSubscriptionToken(current(r).ID, req.ID, req.Name, req.Token)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id})
}

// ---- guided sign-in ----

func (s *Server) startClaudeLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"` // existing subscription account to refresh
		Name string `json:"name"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	a := actor(r)
	sb := s.m.Sandbox()
	if sb == nil {
		httpError(w, 400, "isolation is not available")
		return
	}
	name := strings.TrimSpace(req.Name)
	if req.ID == "" && name == "" {
		name = "Claude subscription"
	}
	sp := term.Spec{Cols: loginCols, Rows: 40, User: a.ID, Title: "Claude sign-in"}
	home, argv, kill, err := s.m.ClaudeLoginShell(a, "login-"+sp.ID(), []string{"claude", "setup-token"})
	if err != nil {
		httpError(w, 409, err.Error())
		return
	}
	sp.Cwd, sp.Owner, sp.Project, sp.Argv, sp.Kill = home.Path, home.ID, home.ID, argv, kill
	t, err := s.terms.Create(sp)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	l := &claudeLogin{id: newLoginID(), uid: a.ID, name: name, t: t, state: "starting"}
	s.logins.put(l)
	go s.watchLogin(l, func(token string) (string, error) {
		return sb.SaveSubscriptionToken(a.ID, req.ID, name, token)
	})
	writeJSON(w, l.snapshot())
}

func (s *Server) getClaudeLogin(w http.ResponseWriter, r *http.Request) {
	l, found := s.logins.get(r.PathValue("id"), current(r).ID)
	if !found {
		httpError(w, 404, "not found")
		return
	}
	writeJSON(w, l.snapshot())
}

func (s *Server) claudeLoginCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	l, found := s.logins.get(r.PathValue("id"), current(r).ID)
	if !found {
		httpError(w, 404, "not found")
		return
	}
	code := strings.Join(strings.Fields(req.Code), "")
	if code == "" || strings.ContainsAny(code, "\x1b\x00") {
		httpError(w, 400, "paste the code the browser shows")
		return
	}
	// Typed as a paste followed by Enter; sent apart so the CLI's input
	// does not take the Enter as part of the pasted text.
	if err := l.t.Write([]byte(code)); err != nil {
		httpError(w, 409, "the sign-in already ended")
		return
	}
	time.Sleep(150 * time.Millisecond)
	_ = l.t.Write([]byte("\r"))
	writeJSON(w, l.snapshot())
}

func (s *Server) cancelClaudeLogin(w http.ResponseWriter, r *http.Request) {
	if l, found := s.logins.get(r.PathValue("id"), current(r).ID); found {
		l.t.Close()
		s.logins.drop(l.id)
	}
	ok(w)
}
