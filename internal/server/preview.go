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
// Preview origins are either one shared origin on another port of
// Collie's host name (PreviewURL "", ":PORT" or a URL), or, with a
// wildcard URL (https://*.preview.example.com), a host name of its own for
// every container: <label>.preview.example.com.

func (s *Server) previewRoutes() {
	s.mux.HandleFunc("GET /api/sessions/{id}/ports", s.sessionPorts)
	s.mux.HandleFunc("POST /api/sessions/{id}/preview", s.openPreview)
}

// PreviewHandler serves the preview origins (on their own listener, or
// for their own host names).
func (s *Server) PreviewHandler() http.Handler { return s.pv }

func (s *Server) previewEnabled() bool {
	return s.opt.PreviewURL != "off" && (s.opt.PreviewURL != "" || s.opt.PreviewPort != 0)
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

// previewBase is the preview origin for a target, for a browser on
// Collie's origin, or "" when previews aren't set up (or can't be guessed:
// behind a reverse proxy, it takes --preview-url).
func (s *Server) previewBase(r *http.Request, target string) string {
	if !s.previewEnabled() {
		return ""
	}
	v := s.opt.PreviewURL
	if w, ok := parseWildcard(v); ok {
		return w.url(previewLabel(target))
	}
	switch {
	case strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://"):
		return strings.TrimSuffix(v, "/")
	case strings.HasPrefix(v, ":"):
		return s.browserScheme(r) + "://" + net.JoinHostPort(s.browserHost(r), v[1:])
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
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(s.opt.PreviewPort))
}

func localName(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// previewOrigin is the preview origin a request was made to, as the
// browser sees it, and with a wildcard, the label in its host name.
func (s *Server) previewOrigin(r *http.Request) (origin, label string, ok bool) {
	v := s.opt.PreviewURL
	if w, ok := parseWildcard(v); ok {
		l, ok := w.label(hostname(requestHost(r)))
		if !ok {
			return "", "", false
		}
		return w.url(l), l, true
	}
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		if u, err := url.Parse(v); err == nil {
			return u.Scheme + "://" + u.Host, "", true
		}
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
		t.Upstream = s.upstreams.Get("host", func() *preview.Upstream { return preview.HostUpstream(s.hostFilter) })
	}
	if p.Allowlisted() {
		t.Allow = append([]string{}, p.NetAllow...)
	}
	return t, nil
}

// sessionPorts lists what listens in the session's container (or, for a
// host-run session, on the server as Collie's user).
func (s *Server) sessionPorts(w http.ResponseWriter, r *http.Request) {
	a := actor(r)
	t, err := s.previewTarget(a.ID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	_, own := parseWildcard(s.opt.PreviewURL)
	out := map[string]any{"enabled": s.previewBase(r, t.Key) != "", "shared": !own, "running": true, "ports": []preview.Port{}}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	ports, err := t.Upstream.Ports(ctx)
	if err != nil {
		sess, _ := s.m.Store().Session(r.PathValue("id"))
		if p, ok := s.m.Store().Project(sess.ProjectID); ok && p.Isolated() && !s.m.Sandbox().Running(p.ID, sess.Owner) {
			out["running"] = false
		} else {
			out["error"] = err.Error()
		}
	} else if ports != nil {
		out["ports"] = ports
	}
	writeJSON(w, out)
}

// openPreview hands out a link that opens one of the session's ports on
// its preview origin: good once, for a minute, for this user, sign-in,
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
	base := s.previewBase(r, t.Key)
	if base == "" {
		httpError(w, http.StatusConflict, "previews are not set up on this server")
		return
	}
	g := preview.Grant{UserID: a.ID, SessionID: sid, Port: req.Port, Target: t.Key}
	g.Login, _ = r.Context().Value(sessKey).(string)
	g.Legacy, _ = r.Context().Value(legacyKey).(bool)
	if _, ok := parseWildcard(s.opt.PreviewURL); ok {
		g.Label = previewLabel(t.Key)
	}
	tok := s.pv.Mint(g)
	if tok == "" {
		httpError(w, http.StatusTooManyRequests, "too many preview links at once; try again in a minute")
		return
	}
	writeJSON(w, map[string]any{"url": base + preview.OpenPath + "?t=" + url.QueryEscape(tok)})
}
