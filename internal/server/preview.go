package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/preview"
)

// Previews: the session's owner opens the servers their agent started
// (package preview). Collie's origin lists the ports and hands out links;
// the pages themselves are served on preview origins (PreviewHandler),
// where Collie's sign-in never opens anything.
//
// Preview origins are either a few ports of Collie's host name (slots,
// PreviewURL "", ":" or a URL), or, with a wildcard URL
// (https://*.preview.example.com), a host name of its own for every
// container: <label>.preview.example.com.

func (s *Server) previewRoutes() {
	s.mux.HandleFunc("GET /api/sessions/{id}/ports", s.sessionPorts)
	s.mux.HandleFunc("POST /api/sessions/{id}/preview", s.openPreview)
}

// PreviewHandler serves one slot's preview port (the first for names of
// their own).
func (s *Server) PreviewHandler(slot int) http.Handler { return s.pv.Slot(slot) }

func (s *Server) previewEnabled() bool {
	return s.opt.PreviewURL != "off" && (s.opt.PreviewURL != "" || len(s.opt.PreviewPorts) > 0)
}

// previewSlots is how many previews a browser can have open at once on
// Collie's host name (with names of their own: one per container).
func (s *Server) previewSlots() int {
	if _, ok := parseWildcard(s.opt.PreviewURL); ok {
		return 1
	}
	return max(1, len(s.publicPorts()))
}

// publicPorts are the preview ports as browsers use them.
func (s *Server) publicPorts() []int {
	if s.opt.PreviewPublic != nil {
		return s.opt.PreviewPublic
	}
	return s.opt.PreviewPorts
}

func (s *Server) publicPort(slot int) (string, bool) {
	ports := s.publicPorts()
	if slot < 0 || slot >= len(ports) {
		return "", false
	}
	return strconv.Itoa(ports[slot]), true
}

func (s *Server) newPreview() *preview.Proxy {
	// Host-run sessions' servers: Collie's own user's, never Collie itself.
	s.hostFilter = preview.Filter{UID: os.Getuid(), SkipOwn: true, SkipPorts: s.opt.OwnPorts}
	return &preview.Proxy{Resolve: s.resolvePreview, Origin: s.previewOrigin, Bind: s.bindPreview,
		Back: func(*http.Request) string { return strings.TrimSuffix(s.opt.PublicURL, "/") }}
}

// wildcard is a preview URL with a host name per container.
type wildcard struct {
	scheme, suffix, port string // suffix: ".preview.example.com"
}

func parseWildcard(v string) (wildcard, bool) {
	u, err := url.Parse(v)
	if err != nil || !strings.HasPrefix(u.Host, "*.") {
		return wildcard{}, false
	}
	return wildcard{scheme: u.Scheme, suffix: strings.ToLower(strings.TrimPrefix(u.Hostname(), "*")), port: u.Port()}, true
}

func (w wildcard) url(label string) string {
	host := label + w.suffix
	if w.port != "" {
		host = net.JoinHostPort(host, w.port)
	}
	return w.scheme + "://" + host
}

// label is the host label of a host name, if it is one of the wildcard's.
func (w wildcard) label(hostname string) (string, bool) {
	l, ok := strings.CutSuffix(strings.ToLower(hostname), w.suffix)
	if !ok || !labelOK(l) {
		return "", false
	}
	return l, true
}

func labelOK(l string) bool {
	if l == "" || len(l) > 63 {
		return false
	}
	for _, r := range l {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// previewLabel is the host label of a target's own origin.
func previewLabel(target string) string {
	h := sha256.Sum256([]byte(target))
	return "c" + hex.EncodeToString(h[:])[:20]
}

// targetKey names where a session's servers run: the user's container for
// the project, or (host-run) the host, as that user sees it.
func targetKey(p model.Project, owner string) string {
	if !p.Isolated() {
		return "host:" + owner
	}
	return "c:" + p.ID + "/" + owner
}

// PreviewHost reports whether Collie serves previews for this host name on
// its own HTTPS listener (--domain): a wildcard name of one of the
// containers that sessions run in, so certificates are only ever asked
// for names in use.
func (s *Server) PreviewHost(host string) bool {
	w, ok := parseWildcard(s.opt.PreviewURL)
	if !ok {
		return false
	}
	l, ok := w.label(host)
	if !ok {
		return false
	}
	for _, sess := range s.m.Store().Sessions() {
		if p, ok := s.m.Store().Project(sess.ProjectID); ok && previewLabel(targetKey(p, sess.Owner)) == l {
			return true
		}
	}
	return false
}

// browserHost is the host name a browser reached Collie by.
func (s *Server) browserHost(r *http.Request) string {
	if u, err := url.Parse(s.opt.PublicURL); err == nil && u.Host != "" {
		return u.Hostname()
	}
	return hostname(requestHost(r))
}

// requestHost is the host[:port] a request was made to (behind a local
// proxy, as it passes it on).
func requestHost(r *http.Request) string {
	if isLoopback(r.RemoteAddr) && r.Header.Get("X-Forwarded-Host") != "" {
		return r.Header.Get("X-Forwarded-Host")
	}
	return r.Host
}

func hostname(h string) string {
	if hn, _, err := net.SplitHostPort(h); err == nil {
		return hn
	}
	return strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
}

// browserScheme is the scheme a browser reached Collie by.
func (s *Server) browserScheme(r *http.Request) string {
	if isHTTPS(r) || (isLoopback(r.RemoteAddr) && strings.HasSuffix(s.browserHost(r), ".ts.net")) {
		return "https"
	}
	return "http"
}

// previewBase is the preview origin of a slot for a target, for a browser
// on Collie's origin, or "" when previews aren't set up (or can't be
// guessed: behind a reverse proxy, it takes --preview-url).
func (s *Server) previewBase(r *http.Request, target string, slot int) string {
	if !s.previewEnabled() {
		return ""
	}
	v := s.opt.PreviewURL
	if w, ok := parseWildcard(v); ok {
		return w.url(previewLabel(target))
	}
	port, ok := s.publicPort(slot)
	if !ok {
		return ""
	}
	switch {
	case strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://"):
		u, err := url.Parse(v)
		if err != nil {
			return ""
		}
		return u.Scheme + "://" + net.JoinHostPort(u.Hostname(), port)
	case strings.HasPrefix(v, ":"):
		return s.browserScheme(r) + "://" + net.JoinHostPort(s.browserHost(r), port)
	}
	// Only for a browser that reached Collie directly: from this machine
	// by a local name, or from elsewhere. Through a local proxy (loopback,
	// any other name) the port it serves previews on isn't known.
	host := s.browserHost(r)
	if isLoopback(r.RemoteAddr) && !localName(host) {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

func localName(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// previewOrigin is the preview origin a request to a slot was made to, as
// the browser sees it, and with a wildcard, the label in its host name.
func (s *Server) previewOrigin(r *http.Request, slot int) (origin, label string, ok bool) {
	v := s.opt.PreviewURL
	if w, ok := parseWildcard(v); ok {
		l, ok := w.label(hostname(requestHost(r)))
		if !ok {
			return "", "", false
		}
		return w.url(l), l, true
	}
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		u, err := url.Parse(v)
		port, ok := s.publicPort(slot)
		if err != nil || !ok {
			return "", "", false
		}
		return u.Scheme + "://" + net.JoinHostPort(u.Hostname(), port), "", true
	}
	h := requestHost(r)
	scheme := "http"
	if isHTTPS(r) || strings.HasPrefix(s.opt.PublicURL, "https://") ||
		(isLoopback(r.RemoteAddr) && strings.HasSuffix(hostname(h), ".ts.net")) {
		scheme = "https"
	}
	return scheme + "://" + h, "", true
}

// bindPreview checks, on the shared preview origin, that a link is opened
// in the browser it was made for. That origin is another port of Collie's
// host name, so the browser sends Collie's cookies along; the one it is
// signed in with must be the one the link was made from. Collie's cookie
// is only compared here: it never opens anything by itself.
func (s *Server) bindPreview(r *http.Request, g preview.Grant) bool {
	switch {
	case s.opt.NoAuth:
		return true
	case g.Login != "":
		for _, c := range r.CookiesNamed(sessionCookie) {
			if _, sess, ok := s.users.SessionUser(c.Value, ""); ok && sess.Hash == g.Login {
				return true
			}
		}
	case g.Legacy:
		for _, c := range r.CookiesNamed(legacyCookie) {
			if s.tokenOK(c.Value) {
				return true
			}
		}
	}
	return false
}

// resolvePreview checks that a preview grant still holds: the sign-in it
// was made from is still valid and the user still owns the session.
func (s *Server) resolvePreview(g preview.Grant) (preview.Target, error) {
	if !s.opt.NoAuth {
		switch {
		case g.Login != "":
			if !s.users.SessionValid(g.Login) {
				return preview.Target{}, preview.ErrSignedOut
			}
		case g.Legacy:
			if s.users.PasskeysExist() {
				return preview.Target{}, preview.ErrSignedOut
			}
		default:
			return preview.Target{}, preview.ErrSignedOut
		}
	}
	if u, ok := s.users.User(g.UserID); !ok || u.Disabled {
		return preview.Target{}, preview.ErrSignedOut
	}
	t, err := s.previewTarget(g.UserID, g.SessionID)
	if err != nil {
		return preview.Target{}, errors.Join(preview.ErrGone, err)
	}
	return t, nil
}

// previewTarget is where a user's session's servers are reached, if the
// user may open them.
func (s *Server) previewTarget(uid, sid string) (preview.Target, error) {
	sess, p, err := s.m.PreviewSession(uid, sid)
	if err != nil {
		return preview.Target{}, err
	}
	t := preview.Target{Key: targetKey(p, sess.Owner)}
	if p.Isolated() {
		argv := s.m.Sandbox().BridgeArgv(p, sess.Owner)
		t.Upstream = s.upstreams.Get(strings.Join(argv, "\x00"), func() *preview.Upstream {
			return preview.BridgeUpstream(preview.NewBridge(argv))
		})
		pid, owner := p.ID, sess.Owner
		t.Touch = func() { s.m.TouchContainer(pid, owner) }
	} else {
		// On the host every user's agents of every host-run project run
		// as the same user: a session reaches only the servers working in
		// its own folder, checked again for every connection.
		f := s.hostFilter
		if f.Allow == nil { // (tests may say otherwise)
			f.Allow = func(dirs []string) bool {
				fs, _ := s.hostFolders()
				fo, ok := preview.Where(dirs, fs)
				return ok && slices.Contains(fo.Sessions, sid)
			}
		}
		t.Upstream = s.upstreams.Get("host|"+sid, func() *preview.Upstream { return preview.HostUpstream(f) })
	}
	if p.Allowlisted() {
		t.Allow = append([]string{}, p.NetAllow...)
	}
	return t, nil
}

// folders are where sessions work, for telling which session a server
// belongs to: their own worktrees, and the project's folder, which the
// sessions without a worktree share (and which may have none). Each is
// there as stored and, if that goes through a symlink, as the real path
// that processes on the host report (containers see the stored paths).
func folders(projects []model.Project, sessions []model.Session) (out []preview.Folder, project map[string]bool) {
	project = map[string]bool{}
	at := map[string]int{}
	add1 := func(path, sid string) {
		i, ok := at[path]
		if !ok {
			i = len(out)
			at[path] = i
			out = append(out, preview.Folder{Path: path})
		}
		if sid != "" {
			out[i].Sessions = append(out[i].Sessions, sid)
		}
	}
	add := func(path, sid string) {
		if path == "" {
			return
		}
		path = filepath.Clean(path)
		add1(path, sid)
		if real, err := filepath.EvalSymlinks(path); err == nil && real != path {
			add1(real, sid)
		}
	}
	for _, p := range projects {
		project[filepath.Clean(p.Path)] = true
		if real, err := filepath.EvalSymlinks(p.Path); err == nil {
			project[real] = true
		}
		add(p.Path, "")
	}
	for _, x := range sessions {
		add(x.Cwd, x.ID)
	}
	return out, project
}

// containerFolders are the folders of the sessions in a user's container
// for a project (all theirs).
func (s *Server) containerFolders(p model.Project, owner string) ([]preview.Folder, map[string]bool) {
	var sessions []model.Session
	for _, x := range s.m.Store().Sessions() {
		if x.ProjectID == p.ID && x.Owner == owner {
			sessions = append(sessions, x)
		}
	}
	return folders([]model.Project{p}, sessions)
}

// hostFolders are the folders of every host-run session, of every user:
// a server in another one's folder is not this session's.
func (s *Server) hostFolders() ([]preview.Folder, map[string]bool) {
	st := s.m.Store()
	var projects []model.Project
	host := map[string]bool{}
	for _, p := range st.Projects() {
		if !p.Isolated() {
			projects = append(projects, p)
			host[p.ID] = true
		}
	}
	var sessions []model.Session
	for _, x := range st.Sessions() {
		if host[x.ProjectID] {
			sessions = append(sessions, x)
		}
	}
	return folders(projects, sessions)
}

// portInfo is a listening port as the Ports tab shows it.
type portInfo struct {
	Port    int    `json:"port"`
	Addr    string `json:"addr"`
	Process string `json:"process,omitempty"`
	// Sessions are those working in the folder the server runs in (several
	// for the project's folder); none when that is not known.
	Sessions []string `json:"sessions"`
	// Folder is "worktree" or "project" (the project's folder), "" when
	// not known.
	Folder string `json:"folder,omitempty"`
}

// sessionPorts lists what listens in the session's container (or, for a
// host-run session, in its folder on the server), each with the sessions
// whose folder it runs in.
func (s *Server) sessionPorts(w http.ResponseWriter, r *http.Request) {
	a := actor(r)
	sid := r.PathValue("id")
	t, err := s.previewTarget(a.ID, sid)
	if err != nil {
		fail(w, err)
		return
	}
	_, own := parseWildcard(s.opt.PreviewURL)
	out := map[string]any{"enabled": s.previewBase(r, t.Key, 0) != "", "shared": !own, "slots": s.previewSlots(),
		"running": true, "ports": []portInfo{}}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	ports, err := t.Upstream.Ports(ctx)
	sess, _ := s.m.Store().Session(sid)
	p, _ := s.m.Store().Project(sess.ProjectID)
	if err != nil {
		if p.Isolated() && !s.m.Sandbox().Running(p.ID, sess.Owner) {
			out["running"] = false
		} else {
			out["error"] = err.Error()
		}
		writeJSON(w, out)
		return
	}
	var fs []preview.Folder
	var project map[string]bool
	if p.Isolated() {
		fs, project = s.containerFolders(p, sess.Owner)
	} else {
		fs, project = s.hostFolders()
	}
	out["ports"] = describePorts(ports, fs, project, func(id string) bool {
		x, ok := s.m.Store().Session(id)
		return ok && x.Owner == a.ID
	})
	writeJSON(w, out)
}

// describePorts says for each port which of the user's sessions (mine)
// work in the folder its server runs in.
func describePorts(ports []preview.Port, fs []preview.Folder, project map[string]bool, mine func(string) bool) []portInfo {
	list := []portInfo{}
	for _, x := range ports {
		pi := portInfo{Port: x.Port, Addr: x.Addr, Process: x.Cmd, Sessions: []string{}}
		if fo, ok := preview.Where(x.Dirs, fs); ok {
			pi.Folder = "worktree"
			if project[fo.Path] {
				pi.Folder = "project"
			}
			for _, id := range fo.Sessions {
				if mine(id) {
					pi.Sessions = append(pi.Sessions, id)
				}
			}
		}
		list = append(list, pi)
	}
	return list
}

// openPreview hands out a link that opens one of the session's ports on
// a preview origin: good once, for a minute, for this user, sign-in,
// session and port only.
func (s *Server) openPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port int `json:"port"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if !preview.ValidPort(req.Port) {
		httpError(w, 400, "choose a port between 1 and 65535")
		return
	}
	a := actor(r)
	sid := r.PathValue("id")
	t, err := s.previewTarget(a.ID, sid)
	if err != nil {
		fail(w, err)
		return
	}
	if err := t.Upstream.Check(req.Port); err != nil {
		httpError(w, http.StatusForbidden, "no server of this session is listening on that port")
		return
	}
	g := preview.Grant{UserID: a.ID, SessionID: sid, Port: req.Port, Target: t.Key}
	g.Login, _ = r.Context().Value(sessKey).(string)
	g.Legacy, _ = r.Context().Value(legacyKey).(bool)
	if _, ok := parseWildcard(s.opt.PreviewURL); ok {
		g.Label = previewLabel(t.Key)
	}
	if s.previewBase(r, t.Key, 0) == "" {
		httpError(w, http.StatusConflict, "previews are not set up on this server")
		return
	}
	tok, slot := s.pv.MintSlot(r, g, s.previewSlots())
	base := s.previewBase(r, t.Key, slot)
	if tok == "" || base == "" {
		httpError(w, http.StatusTooManyRequests, "too many preview links at once; try again in a minute")
		return
	}
	writeJSON(w, map[string]any{"url": base + preview.OpenPath + "?t=" + url.QueryEscape(tok)})
}
