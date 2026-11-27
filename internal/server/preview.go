package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tattsun/collie/internal/preview"
)

// Previews: the session's owner opens the servers their agent started
// (package preview). Collie's origin lists the ports and hands out links;
// the pages themselves are served on the preview origin (PreviewHandler),
// which never sees Collie's sign-in.

func (s *Server) previewRoutes() {
	s.mux.HandleFunc("GET /api/sessions/{id}/ports", s.sessionPorts)
	s.mux.HandleFunc("POST /api/sessions/{id}/preview", s.openPreview)
}

// PreviewHandler serves the preview origin (on its own listener, or for
// its own host name).
func (s *Server) PreviewHandler() http.Handler { return s.pv }

func (s *Server) previewEnabled() bool {
	return s.opt.PreviewURL != "off" && (s.opt.PreviewURL != "" || s.opt.PreviewPort != 0)
}

func (s *Server) newPreview() *preview.Proxy {
	// Host-run sessions' servers: Collie's own user's, never Collie itself.
	s.hostFilter = preview.Filter{UID: os.Getuid(), SkipOwn: true, SkipPorts: s.opt.OwnPorts}
	return &preview.Proxy{Resolve: s.resolvePreview, Origin: s.previewOrigin, Back: func(*http.Request) string {
		return strings.TrimSuffix(s.opt.PublicURL, "/")
	}}
}

// browserHost is the host name a browser reached Collie by.
func (s *Server) browserHost(r *http.Request) string {
	if u, err := url.Parse(s.opt.PublicURL); err == nil && u.Host != "" {
		return u.Hostname()
	}
	h := r.Host
	if isLoopback(r.RemoteAddr) && r.Header.Get("X-Forwarded-Host") != "" {
		h = r.Header.Get("X-Forwarded-Host")
	}
	if hn, _, err := net.SplitHostPort(h); err == nil {
		return hn
	}
	return strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
}

// browserScheme is the scheme a browser reached Collie (or the preview
// origin) by.
func (s *Server) browserScheme(r *http.Request) string {
	if isHTTPS(r) || (isLoopback(r.RemoteAddr) && strings.HasSuffix(s.browserHost(r), ".ts.net")) {
		return "https"
	}
	return "http"
}

// previewBase is the preview origin for a browser on Collie's origin, or
// "" when previews aren't set up (or can't be guessed: behind a reverse
// proxy, it takes --preview-url).
func (s *Server) previewBase(r *http.Request) string {
	if !s.previewEnabled() {
		return ""
	}
	v := s.opt.PreviewURL
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

// previewOrigin is the preview origin as the browser sees it, for a
// request to it.
func (s *Server) previewOrigin(r *http.Request) string {
	if v := s.opt.PreviewURL; strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		if u, err := url.Parse(v); err == nil {
			return u.Scheme + "://" + u.Host
		}
	}
	h := r.Host
	if isLoopback(r.RemoteAddr) && r.Header.Get("X-Forwarded-Host") != "" {
		h = r.Header.Get("X-Forwarded-Host")
	}
	scheme := "http"
	if isHTTPS(r) || strings.HasPrefix(s.opt.PublicURL, "https://") ||
		(isLoopback(r.RemoteAddr) && strings.Contains(h, ".ts.net")) {
		scheme = "https"
	}
	return scheme + "://" + h
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
	var t preview.Target
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
	out := map[string]any{"enabled": s.previewBase(r) != "", "running": true, "ports": []preview.Port{}}
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
// the preview origin: good once, for a minute, for this user, sign-in,
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
	if _, err := s.previewTarget(a.ID, sid); err != nil {
		fail(w, err)
		return
	}
	base := s.previewBase(r)
	if base == "" {
		httpError(w, http.StatusConflict, "previews are not set up on this server")
		return
	}
	sessHash, _ := r.Context().Value(sessKey).(string)
	legacy, _ := r.Context().Value(legacyKey).(bool)
	tok := s.pv.Mint(preview.Grant{UserID: a.ID, Login: sessHash, Legacy: legacy, SessionID: sid, Port: req.Port})
	if tok == "" {
		httpError(w, http.StatusTooManyRequests, "too many preview links at once; try again in a minute")
		return
	}
	writeJSON(w, map[string]any{"url": base + preview.OpenPath + "?t=" + url.QueryEscape(tok)})
}
