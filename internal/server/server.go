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
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/tattsun/collie/internal/agent"
	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/session"
	"github.com/tattsun/collie/internal/term"
)

const Version = "0.1.0"

type Options struct {
	Token  string
	NoAuth bool
	Web    fs.FS
}

type Server struct {
	m     *session.Manager
	terms *term.Manager
	opt   Options
	mux   *http.ServeMux
	gz    sync.Map // static path -> []byte (gzip)

	agentsAvail map[string]bool
	build       string
}

func New(m *session.Manager, terms *term.Manager, opt Options) *Server {
	s := &Server{m: m, terms: terms, opt: opt, mux: http.NewServeMux(), agentsAvail: map[string]bool{}}
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
	terms.OnChange = func() { m.Hub().Broadcast(map[string]any{"t": "terms", "terms": terms.List()}) }
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	// Agent output is rendered as markdown; even if something slipped past
	// the renderer's escaping, no injected script may run.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
		"img-src 'self' data: blob: https:; connect-src 'self' ws: wss:; font-src 'self' data:; object-src 'none'; "+
		"base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/login" && !s.authed(r) {
		httpError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	mx := s.mux
	mx.HandleFunc("POST /api/login", s.login)
	mx.HandleFunc("POST /api/logout", s.logout)
	mx.HandleFunc("GET /api/state", s.state)
	mx.HandleFunc("GET /api/ws", s.ws)

	mx.HandleFunc("POST /api/projects", s.createProject)
	mx.HandleFunc("DELETE /api/projects/{id}", s.deleteProject)
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

	mx.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { httpError(w, 404, "not found") })
	mx.HandleFunc("/", s.static)
}

// ---- auth ----

const cookieName = "collie_auth"

func (s *Server) authed(r *http.Request) bool {
	if s.opt.NoAuth {
		return true
	}
	// `tailscale serve` proxies from loopback and adds identity headers for
	// tailnet users (never for Funnel traffic).
	if r.Header.Get("Tailscale-User-Login") != "" && isLoopback(r.RemoteAddr) {
		return true
	}
	if c, err := r.Cookie(cookieName); err == nil && s.tokenOK(c.Value) {
		return true
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") && s.tokenOK(strings.TrimPrefix(h, "Bearer ")) {
		return true
	}
	return false
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

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if !s.opt.NoAuth && !s.tokenOK(strings.TrimSpace(req.Token)) {
		time.Sleep(500 * time.Millisecond)
		httpError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: strings.TrimSpace(req.Token), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 400 * 24 * 3600,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, map[string]any{"ok": true})
}

// ---- state & websocket ----

func (s *Server) stateData() map[string]any {
	cfg := s.m.Config()
	return map[string]any{
		"projects":  s.m.Projects(),
		"sessions":  s.m.Sessions(),
		"agentCaps": s.m.AgentCaps(),
		"modes":     agent.Modes,
		"agents":    s.agentsAvail,
		"root":      cfg.ProjectRoot,
		"terms":     s.terms.List(),
		"version":   Version,
		"build":     s.build,
	}
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.stateData()) }

func (s *Server) acceptWS(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	patterns := []string{r.Host, "*.ts.net", "localhost:*", "127.0.0.1:*"}
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		patterns = append(patterns, h)
	}
	return websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:  patterns,
		CompressionMode: websocket.CompressionContextTakeover,
	})
}

func (s *Server) ws(w http.ResponseWriter, r *http.Request) {
	c, err := s.acceptWS(w, r)
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	hub := s.m.Hub()
	client := session.NewClient(func() {
		cancel()
		_ = c.Close(websocket.StatusPolicyViolation, "client too slow")
	})
	hub.Add(client)
	defer hub.Remove(client)

	init := s.stateData()
	init["t"] = "init"
	b, _ := json.Marshal(init)
	client.Enqueue(b)

	go func() {
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
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
				b, _ := json.Marshal(map[string]any{"t": "suberr", "sid": msg.SID, "error": err.Error()})
				client.Enqueue(b)
			}
		case "unsub":
			s.m.Unsubscribe(client, msg.SID)
		case "seen":
			s.m.Seen(msg.SID)
		}
	}
}

// ---- projects ----

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var req session.CreateProjectReq
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	p, err := s.m.CreateProject(req)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, p)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	if err := s.m.DeleteProject(r.PathValue("id")); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) projectChanges(w http.ResponseWriter, r *http.Request) {
	ch, err := s.m.ProjectChanges(r.PathValue("id"))
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, ch)
}

func (s *Server) dirs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"root": s.m.Config().ProjectRoot, "dirs": s.m.SuggestDirs()})
}

// ---- sessions ----

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var req session.CreateSessionReq
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	sess, err := s.m.CreateSession(req)
	if err != nil && sess.ID == "" {
		httpError(w, 400, err.Error())
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
	sess, err := s.m.Patch(r.PathValue("id"), req)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, sess)
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	rm := r.URL.Query().Get("worktree") != "keep"
	if err := s.m.DeleteSession(r.PathValue("id"), rm); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
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
	if err := s.m.Send(r.PathValue("id"), req.Text, req.Images); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	if err := s.m.Interrupt(r.PathValue("id")); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	if err := s.m.Stop(r.PathValue("id")); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) reply(w http.ResponseWriter, r *http.Request) {
	var req agent.Reply
	if err := decode(r, &req); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if err := s.m.Reply(r.PathValue("id"), r.PathValue("ask"), req); err != nil {
		httpError(w, 409, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) items(w http.ResponseWriter, r *http.Request) {
	before, _ := strconv.Atoi(r.URL.Query().Get("before"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 300
	}
	items, start, err := s.m.Items(r.PathValue("id"), before, limit)
	if err != nil {
		httpError(w, 404, err.Error())
		return
	}
	writeJSON(w, map[string]any{"items": items, "start": start})
}

func (s *Server) sessionChanges(w http.ResponseWriter, r *http.Request) {
	ch, err := s.m.Changes(r.PathValue("id"))
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, ch)
}

// ---- uploads ----

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
	ext := ""
	switch http.DetectContentType(data) {
	case "image/png":
		ext = ".png"
	case "image/jpeg":
		ext = ".jpg"
	case "image/gif":
		ext = ".gif"
	case "image/webp":
		ext = ".webp"
	default:
		httpError(w, 400, "only png, jpeg, gif and webp images are supported")
		return
	}
	name := model.NewID() + ext
	p, _ := s.m.UploadPath(name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"id": name, "url": "/api/uploads/" + name})
}

func (s *Server) getUpload(w http.ResponseWriter, r *http.Request) {
	p, ok := s.m.UploadPath(r.PathValue("name"))
	if !ok {
		httpError(w, 404, "not found")
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeFile(w, r, p)
}

// ---- terminals ----

func (s *Server) listTerms(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.terms.List())
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
	var cwd, title, owner string
	if req.SessionID != "" {
		sess, ok := s.m.Store().Session(req.SessionID)
		if !ok {
			httpError(w, 404, "session not found")
			return
		}
		cwd, title, owner = sess.Cwd, sess.Title, sess.ID
	} else {
		p, ok := s.m.Store().Project(req.ProjectID)
		if !ok {
			httpError(w, 404, "project not found")
			return
		}
		cwd, title, owner = p.Path, p.Name, p.ID
	}
	t, err := s.terms.Create(cwd, title, owner, req.Cols, req.Rows)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, t.Info)
}

func (s *Server) deleteTerm(w http.ResponseWriter, r *http.Request) {
	if t, ok := s.terms.Get(r.PathValue("id")); ok {
		t.Close()
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) termWS(w http.ResponseWriter, r *http.Request) {
	t, ok := s.terms.Get(r.PathValue("id"))
	if !ok {
		httpError(w, 404, "terminal not found")
		return
	}
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
		for {
			select {
			case <-ctx.Done():
				return
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
