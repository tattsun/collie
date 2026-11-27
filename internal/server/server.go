// Package server exposes the HTTP API, the WebSocket event stream and the
// embedded web UI.
package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/tattsun/collie/internal/auth"
	"github.com/tattsun/collie/internal/session"
	"github.com/tattsun/collie/internal/term"
)

const Version = "0.2.0"

type Options struct {
	// Token is the legacy access token. It (and the Tailscale identity
	// header) only work until the first passkey is registered.
	Token  string
	NoAuth bool
	Web    fs.FS
	// ProxyUID is the uid of the local reverse proxy whose identity headers
	// are trusted (tailscaled runs as root).
	ProxyUID int
	// PublicURL pins the origin passkeys are bound to (default: the host the
	// first passkey was registered on).
	PublicURL string
	// Owner is the first admin: the identity of legacy / --no-auth requests.
	Owner string
}

type Server struct {
	m     *session.Manager
	terms *term.Manager
	users *auth.Store
	pk    *auth.Passkeys
	opt   Options
	mux   *http.ServeMux
	gz    sync.Map // static path -> []byte (gzip)

	agentsAvail map[string]bool
	build       string
	logins      claudeLogins
}

func New(m *session.Manager, terms *term.Manager, users *auth.Store, opt Options) *Server {
	s := &Server{m: m, terms: terms, users: users, pk: auth.NewPasskeys(users), opt: opt,
		mux: http.NewServeMux(), agentsAvail: map[string]bool{}}
	cfg := m.Config()
	for name, bin := range map[string]string{"claude": cfg.ClaudeBin, "codex": cfg.CodexBin} {
		_, err := exec.LookPath(bin)
		s.agentsAvail[name] = err == nil
	}
	if opt.Web != nil {
		if b, err := fs.ReadFile(opt.Web, "index.html"); err == nil {
			sum := sha256.Sum256(b)
			s.build = hex.EncodeToString(sum[:6])
		}
	}
	terms.OnChange = s.pushTerms
	m.TermsInUse = terms.InUse
	if sb := m.Sandbox(); sb != nil {
		sb.OnChange = func() {
			m.Hub().Each(func(c *session.Client) {
				u, _ := users.User(c.UserID)
				c.Enqueue(mustJSON(map[string]any{"t": "sandbox", "sandbox": s.sandboxStatus(u.Admin)}))
			})
		}
	}
	// Users or groups changed: everyone's view may have changed.
	users.OnChange = func() { m.Revalidate(s.stateForID) }
	s.routes()
	go func() {
		for range time.Tick(5 * time.Minute) {
			users.Flush()
		}
	}()
	return s
}

func (s *Server) Handler() http.Handler { return s }

// ---- identity ----

type ctxKey int

const (
	userKey ctxKey = iota
	sessKey
	legacyKey
)

const (
	sessionCookie = "collie_sess"
	legacyCookie  = "collie_auth"
)

// identify resolves the signed-in user. Until the first passkey exists the
// legacy token / Tailscale identity sign the owner in (bootstrap); after
// that only passkey sessions count.
func (s *Server) identify(r *http.Request) (u auth.User, sessHash string, legacy bool, ok bool) {
	if s.opt.NoAuth {
		u, ok = s.users.User(s.opt.Owner)
		return u, "", true, ok
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if u, h, ok := s.users.SessionUser(c.Value); ok {
			return u, h, false, true
		}
	}
	if s.users.PasskeysExist() {
		return auth.User{}, "", false, false
	}
	legacyOK := false
	if r.Header.Get("Tailscale-User-Login") != "" && isLoopback(r.RemoteAddr) && s.fromTrustedProxy(r) {
		legacyOK = true
	} else if c, err := r.Cookie(legacyCookie); err == nil && s.tokenOK(c.Value) {
		legacyOK = true
	} else if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") && s.tokenOK(strings.TrimPrefix(h, "Bearer ")) {
		legacyOK = true
	}
	if !legacyOK {
		return auth.User{}, "", false, false
	}
	u, ok = s.users.User(s.opt.Owner)
	return u, "", true, ok
}

func current(r *http.Request) auth.User {
	u, _ := r.Context().Value(userKey).(auth.User)
	return u
}

func actor(r *http.Request) session.Actor {
	u := current(r)
	return session.Actor{ID: u.ID, Name: u.Name, Admin: u.Admin}
}

// public endpoints (no signed-in user required)
var publicAPI = map[string]bool{
	"/api/auth/status":        true,
	"/api/auth/login/begin":   true,
	"/api/auth/login/finish":  true,
	"/api/auth/invite":        true,
	"/api/auth/invite/begin":  true,
	"/api/auth/invite/finish": true,
	"/api/auth/logout":        true,
	"/api/login":              true,
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	// Agent output is rendered as markdown; even if something slipped past
	// the renderer's escaping, no injected script may run.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
		"img-src 'self' data: blob: https:; connect-src 'self' ws: wss:; font-src 'self' data:; object-src 'none'; "+
		"base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		// Every API call (WebSockets too) must be same-origin, whatever its
		// method: cookies and the Tailscale identity header would otherwise be
		// usable by any site the user visits.
		if !sameOrigin(r) {
			httpError(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		u, h, legacy, ok := s.identify(r)
		if !ok && !publicAPI[r.URL.Path] {
			httpError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if ok {
			ctx := context.WithValue(r.Context(), userKey, u)
			ctx = context.WithValue(ctx, sessKey, h)
			ctx = context.WithValue(ctx, legacyKey, legacy)
			r = r.WithContext(ctx)
		}
	}
	s.mux.ServeHTTP(w, r)
}

func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !current(r).Admin {
		httpError(w, http.StatusForbidden, "admins only")
		return false
	}
	return true
}

// allowedHosts lists the host[:port] values a same-origin browser request
// can carry in its Origin header.
func allowedHosts(r *http.Request) []string {
	hosts := []string{r.Host}
	// Behind a local reverse proxy (tailscale serve) the public name may only
	// arrive in X-Forwarded-Host. Browsers cannot set this header themselves,
	// and it is only trusted from loopback.
	if isLoopback(r.RemoteAddr) {
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

func normHost(h string) string {
	h = strings.ToLower(h)
	return strings.TrimSuffix(strings.TrimSuffix(h, ":443"), ":80")
}

// sameOrigin rejects cross-site browser requests (CSRF and cross-site
// WebSocket hijacking). Browsers always send Origin on WebSocket handshakes
// and cross-origin requests; non-browser clients (curl) send neither header.
func sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host == "" {
			return false // includes the opaque "null" origin
		}
		for _, h := range allowedHosts(r) {
			if normHost(u.Host) == normHost(h) {
				return true
			}
		}
		return false
	}
	return r.Header.Get("Sec-Fetch-Site") != "cross-site"
}

func (s *Server) fromTrustedProxy(r *http.Request) bool {
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	uid, err := loopbackPeerUID(r.RemoteAddr, local.String())
	return err == nil && uid == s.opt.ProxyUID
}

func (s *Server) tokenOK(t string) bool {
	return s.opt.Token != "" && subtle.ConstantTimeCompare([]byte(t), []byte(s.opt.Token)) == 1
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// relyingParty works out the origin passkeys are bound to for a request.
func (s *Server) relyingParty(r *http.Request) (auth.RP, error) {
	if s.opt.PublicURL != "" {
		u, err := url.Parse(s.opt.PublicURL)
		if err != nil || u.Host == "" {
			return auth.RP{}, errors.New("invalid --public-url")
		}
		return auth.RP{ID: u.Hostname(), Origin: u.Scheme + "://" + u.Host}, nil
	}
	host := r.Host
	if isLoopback(r.RemoteAddr) {
		if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
			host = fh
		}
	}
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	https := r.TLS != nil || (isLoopback(r.RemoteAddr) && (r.Header.Get("X-Forwarded-Proto") == "https" || strings.HasSuffix(hostname, ".ts.net")))
	var rp auth.RP
	switch {
	case hostname == "localhost":
		scheme := "http"
		if https {
			scheme = "https"
		}
		rp = auth.RP{ID: "localhost", Origin: scheme + "://" + host}
	case https:
		rp = auth.RP{ID: hostname, Origin: "https://" + normHost(host)}
	default:
		return auth.RP{}, errors.New("passkeys need HTTPS: open Collie through `tailscale serve` (https://<machine>.<tailnet>.ts.net)")
	}
	if id, origin := s.users.RP(); id != "" && (id != rp.ID || origin != rp.Origin) {
		return auth.RP{}, errors.New("passkeys are registered for " + origin + " — open Collie there")
	}
	return rp, nil
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, secret string) {
	rp, _ := s.relyingParty(r)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: secret, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(auth.SessionTTL.Seconds()),
		Secure: strings.HasPrefix(rp.Origin, "https://"),
	})
	// The legacy token cookie is no longer needed.
	http.SetCookie(w, &http.Cookie{Name: legacyCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
}

// ---- state & websocket ----

type userInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Admin       bool   `json:"admin,omitempty"`
}

type groupInfo struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Members map[string]string `json:"members"`
	MyRole  string            `json:"myRole,omitempty"`
}

func (s *Server) sandboxStatus(admin bool) any {
	sb := s.m.Sandbox()
	if sb == nil {
		return map[string]any{"available": false}
	}
	st := sb.Status()
	if !admin {
		st.Log, st.Shared = "", ""
	}
	return st
}

func (s *Server) groupsFor(u auth.User) []groupInfo {
	out := []groupInfo{}
	for _, g := range s.users.Groups() {
		role := g.Members[u.ID]
		if role == "" && !u.Admin {
			continue
		}
		out = append(out, groupInfo{ID: g.ID, Name: g.Name, Members: g.Members, MyRole: role})
	}
	return out
}

func (s *Server) directory() []userInfo {
	out := []userInfo{}
	for _, x := range s.users.Users() {
		if !x.Disabled {
			out = append(out, userInfo{ID: x.ID, Name: x.Name, DisplayName: x.DisplayName, Admin: x.Admin})
		}
	}
	return out
}

func (s *Server) stateFor(u auth.User, legacy bool) map[string]any {
	cfg := s.m.Config()
	a := session.Actor{ID: u.ID, Name: u.Name, Admin: u.Admin}
	// Every user has a Home (without podman only admins, as it then runs on the host).
	_, _ = s.m.EnsureHome(a)
	return map[string]any{
		"t":         "state",
		"me":        userInfo{ID: u.ID, Name: u.Name, DisplayName: u.DisplayName, Admin: u.Admin},
		"projects":  s.m.VisibleProjects(u.ID),
		"sessions":  s.m.VisibleSessions(u.ID),
		"groups":    s.groupsFor(u),
		"users":     s.directory(),
		"agentCaps": s.m.AgentCaps(),
		"usage":     s.m.UsageFor(u.ID),
		"modes":     agentModes(),
		"agents":    s.agentsAvail,
		"root":      s.m.OwnerRoot(a, ""),
		"rootBase":  cfg.ProjectRoot,
		"terms":     s.termsFor(u.ID),
		"sandbox":   s.sandboxStatus(u.Admin),
		"version":   Version,
		"build":     s.build,
		"legacy":    legacy,
		"passkeys":  len(u.Passkeys),
	}
}

// stateForID is used when re-sending state after users/groups changed;
// nil means the user is gone (the connection is closed).
func (s *Server) stateForID(uid string) any {
	u, ok := s.users.User(uid)
	if !ok || u.Disabled {
		return nil
	}
	return s.stateFor(u, false)
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	legacy, _ := r.Context().Value(legacyKey).(bool)
	writeJSON(w, s.stateFor(current(r), legacy))
}

func (s *Server) termsFor(uid string) []term.Info {
	out := []term.Info{}
	for _, t := range s.terms.List() {
		if t.User == uid {
			out = append(out, t)
		}
	}
	return out
}

func (s *Server) pushTerms() {
	s.m.Hub().Each(func(c *session.Client) {
		c.Enqueue(mustJSON(map[string]any{"t": "terms", "terms": s.termsFor(c.UserID)}))
	})
}

func (s *Server) acceptWS(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	// Exact hosts only (sameOrigin has already checked this in ServeHTTP;
	// the library check is a second layer). Glob characters are escaped so
	// e.g. IPv6 literals match literally.
	var patterns []string
	for _, h := range allowedHosts(r) {
		patterns = append(patterns, globEscape.Replace(h))
	}
	return websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:  patterns,
		CompressionMode: websocket.CompressionContextTakeover,
	})
}

var globEscape = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`, `]`, `\]`)

// stillSignedIn re-checks a connection's login (sessions can be revoked and
// the legacy sign-in stops working once passkeys exist).
func (s *Server) stillSignedIn(uid, sessHash string, legacy bool) bool {
	if s.opt.NoAuth {
		return true
	}
	if sessHash != "" {
		return s.users.SessionValid(sessHash)
	}
	if legacy {
		u, ok := s.users.User(uid)
		return ok && !u.Disabled && !s.users.PasskeysExist()
	}
	return false
}

func (s *Server) ws(w http.ResponseWriter, r *http.Request) {
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
	hub := s.m.Hub()
	client := session.NewClient(u.ID, sessHash, func() {
		cancel()
		_ = c.Close(websocket.StatusPolicyViolation, "disconnected")
	})
	hub.Add(client)
	defer hub.Remove(client)

	init := s.stateFor(u, legacy)
	init["t"] = "init"
	client.Init(s.m.VisibleProjects(u.ID), s.m.VisibleSessions(u.ID))
	client.Enqueue(mustJSON(init))

	go func() {
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		checks := 0
		for {
			select {
			case <-ctx.Done():
				return
			case b := <-client.Send:
				wctx, wcancel := context.WithTimeout(ctx, 15*time.Second)
				err := c.Write(wctx, websocket.MessageText, b)
				wcancel()
				if err != nil {
					cancel()
					return
				}
			case <-ping.C:
				if checks++; checks%2 == 0 && !s.stillSignedIn(u.ID, sessHash, legacy) {
					_ = c.Close(4401, "signed out")
					cancel()
					return
				}
				pctx, pcancel := context.WithTimeout(ctx, 15*time.Second)
				err := c.Ping(pctx)
				pcancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	a := session.Actor{ID: u.ID, Name: u.Name, Admin: u.Admin}
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var msg struct {
			T   string `json:"t"`
			SID string `json:"sid"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.T {
		case "sub":
			if err := s.m.Subscribe(client, msg.SID); err != nil {
				client.Enqueue(mustJSON(map[string]any{"t": "suberr", "sid": msg.SID, "error": err.Error()}))
			}
		case "unsub":
			s.m.Unsubscribe(client, msg.SID)
		case "seen":
			s.m.Seen(a, msg.SID)
		}
	}
}

func (s *Server) routes() {
	mx := s.mux
	mx.HandleFunc("GET /api/state", s.state)
	mx.HandleFunc("GET /api/ws", s.ws)
	s.authRoutes()
	s.apiRoutes()
	mx.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { httpError(w, 404, "not found") })
	mx.HandleFunc("/", s.static)
}

// ---- static ----

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	if s.opt.Web == nil {
		http.Error(w, "web UI not built", 500)
		return
	}
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p == "" {
		p = "index.html"
	}
	data, err := fs.ReadFile(s.opt.Web, p)
	if err != nil {
		// SPA fallback
		p = "index.html"
		data, err = fs.ReadFile(s.opt.Web, p)
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
	}
	ctype := mime.TypeByExtension(filepath.Ext(p))
	if ctype == "" {
		ctype = http.DetectContentType(data)
	}
	w.Header().Set("Content-Type", ctype)
	if strings.HasPrefix(p, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	compressible := strings.HasPrefix(ctype, "text/") || strings.Contains(ctype, "javascript") ||
		strings.Contains(ctype, "json") || strings.Contains(ctype, "svg")
	if compressible && len(data) > 1024 && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		gz, ok := s.gz.Load(p)
		if !ok {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			_, _ = zw.Write(data)
			_ = zw.Close()
			gz = buf.Bytes()
			s.gz.Store(p, gz)
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Vary", "Accept-Encoding")
		data = gz.([]byte)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

// ---- helpers ----

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func decode(r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return errors.New("content-type must be application/json")
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	return dec.Decode(v)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg})
}

// fail maps manager errors to HTTP statuses.
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrNotFound), errors.Is(err, auth.ErrNotFound):
		httpError(w, http.StatusNotFound, "not found")
	case errors.Is(err, session.ErrForbidden):
		httpError(w, http.StatusForbidden, err.Error())
	default:
		httpError(w, http.StatusBadRequest, err.Error())
	}
}
