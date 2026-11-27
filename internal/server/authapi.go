package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tattsun/collie/internal/auth"
	"github.com/tattsun/collie/internal/sandbox"
	"github.com/tattsun/collie/internal/session"
	"github.com/tattsun/collie/internal/update"
)

func (s *Server) authRoutes() {
	mx := s.mux
	// sign-in (public)
	mx.HandleFunc("GET /api/auth/status", s.authStatus)
	mx.HandleFunc("POST /api/auth/login/begin", s.loginBegin)
	mx.HandleFunc("POST /api/auth/login/finish", s.loginFinish)
	mx.HandleFunc("GET /api/auth/invite", s.inviteInfo)
	mx.HandleFunc("POST /api/auth/invite/begin", s.inviteBegin)
	mx.HandleFunc("POST /api/auth/invite/finish", s.inviteFinish)
	mx.HandleFunc("POST /api/auth/logout", s.logout)
	mx.HandleFunc("POST /api/login", s.legacyLogin)

	// the signed-in user
	mx.HandleFunc("GET /api/me", s.me)
	mx.HandleFunc("PATCH /api/me", s.patchMe)
	mx.HandleFunc("POST /api/me/passkeys/begin", s.addPasskeyBegin)
	mx.HandleFunc("POST /api/me/passkeys/finish", s.addPasskeyFinish)
	mx.HandleFunc("PATCH /api/me/passkeys/{id}", s.renamePasskey)
	mx.HandleFunc("DELETE /api/me/passkeys/{id}", s.deletePasskey)
	mx.HandleFunc("POST /api/me/link", s.myDeviceLink)
	mx.HandleFunc("POST /api/me/signout-others", s.signoutOthers)
	mx.HandleFunc("GET /api/me/env", s.getMyEnv)
	mx.HandleFunc("PUT /api/me/env", s.putMyEnv)

	// profiles: "global" (admins) or "me"
	mx.HandleFunc("GET /api/profile/{scope}/files", s.profileFiles)
	mx.HandleFunc("GET /api/profile/{scope}/file", s.profileRead)
	mx.HandleFunc("PUT /api/profile/{scope}/file", s.profileWrite)
	mx.HandleFunc("DELETE /api/profile/{scope}/file", s.profileDelete)

	// groups
	mx.HandleFunc("POST /api/groups", s.createGroup)
	mx.HandleFunc("POST /api/groups/{id}/members", s.setMember)
	mx.HandleFunc("DELETE /api/groups/{id}/members/{uid}", s.removeMember)
	mx.HandleFunc("DELETE /api/groups/{id}", s.deleteGroup)

	// administration
	mx.HandleFunc("GET /api/admin/overview", s.adminOverview)
	mx.HandleFunc("POST /api/admin/invites", s.createInvite)
	mx.HandleFunc("DELETE /api/admin/invites/{hash}", s.revokeInvite)
	mx.HandleFunc("PATCH /api/admin/users/{id}", s.patchUser)
	mx.HandleFunc("DELETE /api/admin/users/{id}", s.deleteUser)
	mx.HandleFunc("POST /api/admin/users/{id}/link", s.userDeviceLink)
	mx.HandleFunc("POST /api/admin/containers/kill", s.killContainer)
	mx.HandleFunc("GET /api/shared/env", s.getSharedEnv)
	mx.HandleFunc("PUT /api/shared/env", s.putSharedEnv)
	mx.HandleFunc("POST /api/sandbox/build", s.buildImage)
	mx.HandleFunc("POST /api/admin/update/check", s.updateCheck)
	mx.HandleFunc("POST /api/admin/update/apply", s.updateApply)
	mx.HandleFunc("POST /api/admin/update/cancel", s.updateCancel)
	mx.HandleFunc("PATCH /api/admin/update/settings", s.updateSettings)
}

// ---- sign-in ----

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	rp, rpErr := s.relyingParty(r)
	st := map[string]any{
		"passkeys": s.users.PasskeysExist(),
		"noAuth":   s.opt.NoAuth,
		"rpId":     rp.ID,
	}
	if rpErr != nil {
		st["rpError"] = rpErr.Error()
	}
	if u := current(r); u.ID != "" {
		legacy, _ := r.Context().Value(legacyKey).(bool)
		st["user"] = userInfo{ID: u.ID, Name: u.Name, DisplayName: u.DisplayName, Admin: u.Admin}
		st["legacy"] = legacy
	}
	writeJSON(w, st)
}

func (s *Server) loginBegin(w http.ResponseWriter, r *http.Request) {
	rp, err := s.relyingParty(r)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	opts, id, err := s.pk.BeginLogin(rp)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"id": id, "options": opts})
}

func shortAgent(r *http.Request) string {
	ua := r.UserAgent()
	switch {
	case strings.Contains(ua, "iPhone"):
		return "iPhone"
	case strings.Contains(ua, "iPad"):
		return "iPad"
	case strings.Contains(ua, "Android"):
		return "Android"
	case strings.Contains(ua, "Macintosh"):
		return "Mac"
	case strings.Contains(ua, "Windows"):
		return "Windows"
	case strings.Contains(ua, "Linux"):
		return "Linux"
	}
	return "Browser"
}

func (s *Server) loginFinish(w http.ResponseWriter, r *http.Request) {
	u, err := s.pk.FinishLogin(r.URL.Query().Get("c"), r)
	if err != nil {
		time.Sleep(300 * time.Millisecond)
		httpError(w, 401, err.Error())
		return
	}
	secret, err := s.users.NewSession(u.ID, shortAgent(r))
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	s.setSessionCookie(w, r, secret)
	writeJSON(w, userInfo{ID: u.ID, Name: u.Name, DisplayName: u.DisplayName, Admin: u.Admin})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if _, h, ok := s.users.SessionUser(c.Value); ok {
			s.users.DeleteSession(h)
			s.m.Hub().KillSession("", h)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.SetCookie(w, &http.Cookie{Name: legacyCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	ok(w)
}

// legacyLogin accepts the access token only until passkeys are set up.
func (s *Server) legacyLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if s.users.PasskeysExist() {
		httpError(w, http.StatusForbidden, "token sign-in is disabled: sign in with a passkey")
		return
	}
	if !s.opt.NoAuth && !s.tokenOK(strings.TrimSpace(req.Token)) {
		time.Sleep(500 * time.Millisecond)
		httpError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: legacyCookie, Value: strings.TrimSpace(req.Token), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	ok(w)
}

// ---- invitations & device links ----

func (s *Server) inviteURL(r *http.Request, secret string) string {
	origin := ""
	if rp, err := s.relyingParty(r); err == nil {
		origin = rp.Origin
	}
	return origin + "/?invite=" + secret
}

func (s *Server) inviteInfo(w http.ResponseWriter, r *http.Request) {
	inv, found := s.users.Invite(r.URL.Query().Get("code"))
	if !found {
		httpError(w, 404, "this link is invalid, expired or was already used")
		return
	}
	res := map[string]any{"kind": "new", "admin": inv.Admin, "expiresAt": inv.ExpiresAt}
	if inv.UserID != "" {
		u, _ := s.users.User(inv.UserID)
		res["kind"] = "device"
		res["user"] = userInfo{ID: u.ID, Name: u.Name, DisplayName: u.DisplayName}
	}
	writeJSON(w, res)
}

func (s *Server) inviteBegin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code        string `json:"code"`
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
		Passkey     string `json:"passkey"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	inv, found := s.users.Invite(req.Code)
	if !found {
		httpError(w, 404, "this link is invalid, expired or was already used")
		return
	}
	rp, err := s.relyingParty(r)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	c := auth.Ceremony{Invite: req.Code, Name: req.Passkey}
	if inv.UserID != "" {
		u, ok := s.users.User(inv.UserID)
		if !ok {
			httpError(w, 404, "user not found")
			return
		}
		c.User = u
	} else {
		name := strings.ToLower(strings.TrimSpace(req.Name))
		if !auth.ValidName(name) {
			httpError(w, 400, "user name: 1-32 of a-z 0-9 - _")
			return
		}
		if _, taken := s.users.UserByName(name); taken {
			httpError(w, 400, "that user name is taken")
			return
		}
		display := strings.TrimSpace(req.DisplayName)
		if display == "" {
			display = name
		}
		c.User = auth.User{Handle: auth.NewHandle(), Name: name, DisplayName: display, Admin: inv.Admin}
		c.NewUser = true
	}
	opts, id, err := s.pk.BeginRegistration(rp, c)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"id": id, "options": opts})
}

func (s *Server) inviteFinish(w http.ResponseWriter, r *http.Request) {
	c, cred, err := s.pk.FinishRegistration(r.URL.Query().Get("c"), r)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if c.Invite == "" {
		httpError(w, 400, "not an invitation")
		return
	}
	inv, err := s.users.ConsumeInvite(c.Invite)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	uid := inv.UserID
	if c.NewUser {
		u, err := s.users.CreateUserWithHandle(c.User.Name, c.User.DisplayName, inv.Admin, c.User.Handle)
		if err != nil {
			httpError(w, 400, err.Error())
			return
		}
		uid = u.ID
		if sb := s.m.Sandbox(); sb != nil {
			_ = sb.SeedPersonal(u.ID, u.Name, u.DisplayName)
		}
	}
	if err := s.users.AddPasskey(uid, *cred, c.Name); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	_ = s.users.SetRP(c.RP.ID, c.RP.Origin)
	secret, err := s.users.NewSession(uid, shortAgent(r))
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	s.setSessionCookie(w, r, secret)
	u, _ := s.users.User(uid)
	writeJSON(w, userInfo{ID: u.ID, Name: u.Name, DisplayName: u.DisplayName, Admin: u.Admin})
}

// ---- the signed-in user ----

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	sessHash, _ := r.Context().Value(sessKey).(string)
	type pk struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		CreatedAt int64  `json:"createdAt"`
		LastUsed  int64  `json:"lastUsed,omitempty"`
	}
	pks := []pk{}
	for _, p := range u.Passkeys {
		pks = append(pks, pk{p.ID, p.Name, p.CreatedAt, p.LastUsed})
	}
	sessions := []map[string]any{}
	for _, x := range s.users.Sessions(u.ID) {
		sessions = append(sessions, map[string]any{"current": x.Hash == sessHash, "createdAt": x.CreatedAt, "lastSeen": x.LastSeen, "agent": x.Agent})
	}
	codex := false
	if sb := s.m.Sandbox(); sb != nil {
		if fi, err := os.Stat(filepath.Join(sb.CodexDir(u.ID), "auth.json")); err == nil && fi.Size() > 2 {
			codex = true
		}
	}
	env, _ := s.m.UserEnv(u.ID)
	writeJSON(w, map[string]any{
		"user":     userInfo{ID: u.ID, Name: u.Name, DisplayName: u.DisplayName, Admin: u.Admin},
		"passkeys": pks, "sessions": sessions,
		"codexLoggedIn": codex,
		"claudeToken":   strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=") && !strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=\n"),
	})
}

func (s *Server) patchMe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DisplayName *string `json:"displayName"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if _, err := s.users.UpdateUser(current(r).ID, req.DisplayName, nil, nil); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

// addPasskeyBegin adds a passkey to the signed-in user. During bootstrap
// (signed in with the legacy token) this registers the first passkey.
func (s *Server) addPasskeyBegin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	_ = decode(r, &req)
	rp, err := s.relyingParty(r)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	opts, id, err := s.pk.BeginRegistration(rp, auth.Ceremony{User: current(r), Name: req.Name})
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"id": id, "options": opts})
}

func (s *Server) addPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	c, cred, err := s.pk.FinishRegistration(r.URL.Query().Get("c"), r)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	u := current(r)
	if c.User.ID != u.ID || c.Invite != "" {
		httpError(w, 400, "mismatched request")
		return
	}
	if err := s.users.AddPasskey(u.ID, *cred, c.Name); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	_ = s.users.SetRP(c.RP.ID, c.RP.Origin)
	// The legacy sign-in stops working now: move to a passkey session.
	if legacy, _ := r.Context().Value(legacyKey).(bool); legacy {
		if secret, err := s.users.NewSession(u.ID, shortAgent(r)); err == nil {
			s.setSessionCookie(w, r, secret)
		}
	}
	ok(w)
}

func (s *Server) renamePasskey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if err := s.users.RenamePasskey(current(r).ID, r.PathValue("id"), req.Name); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) deletePasskey(w http.ResponseWriter, r *http.Request) {
	if err := s.users.RemovePasskey(current(r).ID, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

// myDeviceLink makes a one-time link to add a passkey from another device.
func (s *Server) myDeviceLink(w http.ResponseWriter, r *http.Request) {
	secret, err := s.users.CreateInvite(auth.Invite{UserID: current(r).ID, CreatedBy: current(r).ID, Note: "device"}, auth.LinkTTL)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"url": s.inviteURL(r, secret), "expiresIn": int(auth.LinkTTL.Seconds())})
}

func (s *Server) signoutOthers(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	h, _ := r.Context().Value(sessKey).(string)
	s.users.DeleteUserSessions(u.ID, h)
	s.m.Hub().Each(func(c *session.Client) {
		if c.UserID == u.ID && c.SessionHash != h {
			c.Kill()
		}
	})
	ok(w)
}

func (s *Server) getMyEnv(w http.ResponseWriter, r *http.Request) {
	env, err := s.m.UserEnv(current(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"env": env})
}

func (s *Server) putMyEnv(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Env string `json:"env"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if err := s.m.SetUserEnv(current(r).ID, req.Env); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

// ---- profiles ----

func (s *Server) profileDir(w http.ResponseWriter, r *http.Request) (string, bool) {
	sb := s.m.Sandbox()
	if sb == nil {
		httpError(w, 400, "isolation is not available")
		return "", false
	}
	switch r.PathValue("scope") {
	case "global":
		if !requireAdmin(w, r) {
			return "", false
		}
		_ = sandbox.SeedShared(sb.SharedPath())
		return sb.SharedPath(), true
	case "me":
		u := current(r)
		_ = sb.SeedPersonal(u.ID, u.Name, u.DisplayName)
		return sb.PersonalDir(u.ID), true
	}
	httpError(w, 404, "not found")
	return "", false
}

func (s *Server) profileFiles(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.profileDir(w, r)
	if !ok {
		return
	}
	files, err := sandbox.ListProfile(dir)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, files)
}

func (s *Server) profileRead(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.profileDir(w, r)
	if !ok {
		return
	}
	content, err := sandbox.ReadProfileFile(dir, r.URL.Query().Get("path"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"content": content})
}

func (s *Server) profileWrite(w http.ResponseWriter, r *http.Request) {
	dir, found := s.profileDir(w, r)
	if !found {
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if err := sandbox.WriteProfileFile(dir, r.URL.Query().Get("path"), req.Content); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) profileDelete(w http.ResponseWriter, r *http.Request) {
	dir, found := s.profileDir(w, r)
	if !found {
		return
	}
	if err := sandbox.DeleteProfilePath(dir, r.URL.Query().Get("path")); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

// ---- groups ----

func (s *Server) canManageGroup(r *http.Request, gid string) bool {
	u := current(r)
	return u.Admin || s.users.Role(gid, u.ID) == auth.RoleOwner
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	g, err := s.users.CreateGroup(strings.ToLower(strings.TrimSpace(req.Name)), current(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, groupInfo{ID: g.ID, Name: g.Name, Members: g.Members, MyRole: auth.RoleOwner})
}

func (s *Server) setMember(w http.ResponseWriter, r *http.Request) {
	gid := r.PathValue("id")
	if !s.canManageGroup(r, gid) {
		fail(w, forbiddenOr404(s.users.Role(gid, current(r).ID) != ""))
		return
	}
	var req struct {
		User string `json:"user"` // user name or id
		Role string `json:"role"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	u, found := s.users.UserByName(strings.ToLower(strings.TrimSpace(req.User)))
	if !found {
		if u, found = s.users.User(req.User); !found {
			httpError(w, 404, "no such user")
			return
		}
	}
	if req.Role == "" {
		req.Role = auth.RoleMember
	}
	if err := s.users.SetMember(gid, u.ID, req.Role); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	gid, uid := r.PathValue("id"), r.PathValue("uid")
	me := current(r)
	if !s.canManageGroup(r, gid) && uid != me.ID { // anyone may leave
		fail(w, forbiddenOr404(s.users.Role(gid, me.ID) != ""))
		return
	}
	if err := s.users.SetMember(gid, uid, ""); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request) {
	gid := r.PathValue("id")
	if !s.canManageGroup(r, gid) {
		fail(w, forbiddenOr404(s.users.Role(gid, current(r).ID) != ""))
		return
	}
	if n := len(s.m.GroupProjects(gid)); n > 0 {
		httpError(w, 400, "delete or move the group's projects first")
		return
	}
	if err := s.users.DeleteGroup(gid); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

// ---- administration ----

func (s *Server) adminOverview(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	type adminUser struct {
		userInfo
		Disabled  bool  `json:"disabled,omitempty"`
		Passkeys  int   `json:"passkeys"`
		CreatedAt int64 `json:"createdAt"`
		LastSeen  int64 `json:"lastSeen,omitempty"`
	}
	users := []adminUser{}
	for _, u := range s.users.Users() {
		users = append(users, adminUser{userInfo{u.ID, u.Name, u.DisplayName, u.Admin}, u.Disabled, len(u.Passkeys), u.CreatedAt, u.LastSeen})
	}
	invites := []map[string]any{}
	for _, inv := range s.users.Invites() {
		invites = append(invites, map[string]any{"hash": inv.Hash, "admin": inv.Admin, "userId": inv.UserID, "note": inv.Note, "expiresAt": inv.ExpiresAt})
	}
	// Containers: names and owners only — admins do not see project content.
	projects := map[string]string{}
	for _, p := range s.m.AllProjects() {
		owner := ""
		if p.OwnerGroup != "" {
			if g, ok := s.users.Group(p.OwnerGroup); ok {
				owner = "group " + g.Name
			}
		} else if u, ok := s.users.User(p.OwnerUser); ok {
			owner = u.Name
		}
		projects[p.ID] = p.Name + " (" + owner + ")"
	}
	containers := []map[string]any{}
	if sb := s.m.Sandbox(); sb != nil {
		for _, c := range sb.All() {
			u, _ := s.users.User(c.UserID)
			containers = append(containers, map[string]any{"projectId": c.ProjectID, "userId": c.UserID, "project": projects[c.ProjectID],
				"user": u.Name, "running": c.Running, "mem": c.Mem, "cpu": c.CPU, "pids": c.Pids, "limitsEnforced": c.LimitsEnforced})
		}
	}
	writeJSON(w, map[string]any{"users": users, "invites": invites, "containers": containers, "groups": s.groupsFor(current(r)),
		"sandbox": s.sandboxStatus(true)})
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		Admin bool   `json:"admin"`
		Note  string `json:"note"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	secret, err := s.users.CreateInvite(auth.Invite{Admin: req.Admin, Note: req.Note, CreatedBy: current(r).ID}, auth.InviteTTL)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"url": s.inviteURL(r, secret), "expiresIn": int(auth.InviteTTL.Seconds())})
}

func (s *Server) revokeInvite(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if err := s.users.RevokeInvite(r.PathValue("hash")); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) patchUser(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		Admin       *bool   `json:"admin"`
		Disabled    *bool   `json:"disabled"`
		DisplayName *string `json:"displayName"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	uid := r.PathValue("id")
	if _, err := s.users.UpdateUser(uid, req.DisplayName, req.Admin, req.Disabled); err != nil {
		fail(w, err)
		return
	}
	if req.Disabled != nil && *req.Disabled {
		s.m.Hub().KillSession(uid, "")
	}
	ok(w)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	uid := r.PathValue("id")
	if uid == current(r).ID {
		httpError(w, 400, "you cannot delete yourself")
		return
	}
	if err := s.users.DeleteUser(uid); err != nil {
		fail(w, err)
		return
	}
	s.m.Hub().KillSession(uid, "")
	go s.m.PurgeUser(uid)
	ok(w)
}

// userDeviceLink lets an admin help a user who lost their passkeys.
func (s *Server) userDeviceLink(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	secret, err := s.users.CreateInvite(auth.Invite{UserID: r.PathValue("id"), CreatedBy: current(r).ID, Note: "recovery"}, auth.LinkTTL)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"url": s.inviteURL(r, secret), "expiresIn": int(auth.LinkTTL.Seconds())})
}

func (s *Server) killContainer(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		ProjectID string `json:"projectId"`
		UserID    string `json:"userId"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if err := s.m.KillContainer(req.ProjectID, req.UserID); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) getSharedEnv(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	env, err := s.m.SharedEnv()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"env": env})
}

func (s *Server) putSharedEnv(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		Env string `json:"env"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if err := s.m.SetSharedEnv(req.Env); err != nil {
		fail(w, err)
		return
	}
	ok(w)
}

func (s *Server) buildImage(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req struct {
		Refresh bool `json:"refresh"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	sb := s.m.Sandbox()
	if sb == nil || !sb.Available() {
		httpError(w, 400, "podman is not installed on the server")
		return
	}
	if err := sb.Build(req.Refresh); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, sb.Status())
}

// ---- updates ----

func (s *Server) updater(w http.ResponseWriter, r *http.Request) *update.Updater {
	if !requireAdmin(w, r) {
		return nil
	}
	if s.opt.Updater == nil {
		httpError(w, 400, "updates are not available")
		return nil
	}
	return s.opt.Updater
}

func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	up := s.updater(w, r)
	if up == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := up.Check(ctx); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, up.Status())
}

func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	up := s.updater(w, r)
	if up == nil {
		return
	}
	if err := up.Apply(false); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, up.Status())
}

func (s *Server) updateCancel(w http.ResponseWriter, r *http.Request) {
	up := s.updater(w, r)
	if up == nil {
		return
	}
	up.Cancel()
	ok(w)
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	up := s.updater(w, r)
	if up == nil {
		return
	}
	var req struct {
		Auto  *bool   `json:"auto"`
		Token *string `json:"token"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if req.Token != nil {
		if err := up.SetToken(*req.Token); err != nil {
			fail(w, err)
			return
		}
	}
	if req.Auto != nil {
		if err := up.SetAuto(*req.Auto); err != nil {
			fail(w, err)
			return
		}
	}
	writeJSON(w, up.Status())
}

// forbiddenOr404 hides groups from non-members.
func forbiddenOr404(member bool) error {
	if member {
		return session.ErrForbidden
	}
	return session.ErrNotFound
}

var _ = errors.New
