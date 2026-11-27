package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
)

// Media: what agents show the user. Images in tool results (screenshots,
// images the agent read) are kept on the tool's timeline entry, and the
// show_media tool puts files the agent chooses — design options, screen
// recordings, HTML reports — on the timeline as a media entry. Either way
// the bytes are copied into the uploads, so the timeline keeps showing what
// the user saw.

// ---- uploads ----

var uploadRe = regexp.MustCompile(`^[0-9a-f]{16}([0-9a-f]{16})?\.(png|jpe?g|gif|webp|svg|mp4|webm|html)$`)

func (m *Manager) UploadPath(id string) (string, bool) {
	if !uploadRe.MatchString(id) {
		return "", false
	}
	return filepath.Join(m.st.UploadsDir(), id), true
}

var uploadTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".svg": "image/svg+xml", ".mp4": "video/mp4", ".webm": "video/webm", ".html": "text/html; charset=utf-8",
}

// UploadType is the content type an upload is served with.
func UploadType(name string) string { return uploadTypes[filepath.Ext(name)] }

// UploadActive reports whether an upload is a document that could run
// code if a browser rendered it as a page (HTML, SVG). Those are only ever
// served into a sandbox (see the server).
func UploadActive(name string) bool {
	switch filepath.Ext(name) {
	case ".html", ".svg":
		return true
	}
	return false
}

const (
	maxImage = 25 << 20  // images and HTML pages
	maxVideo = 100 << 20 // screen recordings
)

// sniffUpload returns the extension a file is kept under, judged by its
// content (and, for SVG and HTML, which have no reliable signature, also
// by its name), or "" when it is not something Collie shows. Without
// media only the image formats users may attach are accepted.
func sniffUpload(name string, data []byte, media bool) string {
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
	case "video/mp4":
		ext = ".mp4"
	case "video/webm":
		ext = ".webm"
	}
	if !media {
		if ext == ".mp4" || ext == ".webm" || len(data) > maxImage {
			return ""
		}
		return ext
	}
	if ext == "" && utf8.Valid(data) {
		head := data[:min(len(data), 4096)]
		switch strings.ToLower(filepath.Ext(name)) {
		case ".svg":
			if bytes.Contains(head, []byte("<svg")) {
				ext = ".svg"
			}
		case ".html", ".htm", ".xhtml":
			ext = ".html"
		}
	}
	limit := maxImage
	if ext == ".mp4" || ext == ".webm" {
		limit = maxVideo
	}
	if len(data) > limit {
		return ""
	}
	return ext
}

var errBadUpload = errors.New("only png, jpeg, gif and webp images are supported")

// SaveUpload stores an image the user attached and returns its name.
func (m *Manager) SaveUpload(data []byte) (string, error) {
	ext := sniffUpload("", data, false)
	if ext == "" {
		return "", errBadUpload
	}
	return m.saveUpload(data, ext)
}

func (m *Manager) saveUpload(data []byte, ext string) (string, error) {
	name := model.NewID() + model.NewID() + ext
	p, _ := m.UploadPath(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return "", err
	}
	return name, nil
}

// ---- tools sockets ----

// Collie's tools for agents (an MCP server) are served on Unix sockets:
// one per user for their Home, and one per user and project, mounted into
// that user's container for the project. The agent's bridge says which
// session it serves; a socket only accepts sessions that run where it is
// mounted, so an agent reaches at most the same user's other sessions in
// the same project.

const toolSockName = "collie.sock"

// The most connections a tools socket serves at once.
const maxToolConns = 64

type toolScope struct {
	uid     string
	project string // "" for the user's Home
}

func (sc toolScope) key() string {
	if sc.project == "" {
		return sc.uid
	}
	return sc.project + "/" + sc.uid
}

func (m *Manager) toolDir(sc toolScope) string {
	if sc.project == "" {
		return filepath.Join(m.st.Dir(), "run", sc.uid)
	}
	return filepath.Join(m.st.Dir(), "toolsock", sc.project, sc.uid)
}

// toolConn is one agent's connection to its tools socket.
type toolConn struct {
	scope   toolScope
	session string // set once the bridge has named a session it may serve
}

// toolSocket starts the scope's tools socket (once per process) and
// returns the directory holding it.
func (m *Manager) toolSocket(sc toolScope) (string, error) {
	m.homeMu.Lock()
	defer m.homeMu.Unlock()
	dir := m.toolDir(sc)
	if m.toolLn[sc.key()] != nil {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sock := filepath.Join(dir, toolSockName)
	_ = os.Remove(sock) // left over from an earlier run
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return "", err
	}
	_ = os.Chmod(sock, 0o600)
	m.toolLn[sc.key()] = ln
	go func() {
		// Each agent process holds one connection; more than this many
		// at once is an agent hoarding them.
		slots := make(chan struct{}, maxToolConns)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
				go func() {
					defer func() { <-slots }()
					m.serveTools(&toolConn{scope: sc}, c)
				}()
			default:
				c.Close()
			}
		}
	}()
	return dir, nil
}

// closeToolSockets stops the tools sockets of a deleted project.
func (m *Manager) closeToolSockets(projectID string) {
	m.homeMu.Lock()
	for k, ln := range m.toolLn {
		if strings.HasPrefix(k, projectID+"/") {
			_ = ln.Close()
			delete(m.toolLn, k)
		}
	}
	m.homeMu.Unlock()
	_ = os.RemoveAll(m.toolDir(toolScope{project: projectID}))
}

// toolSessionOK reports whether a session's agent runs where the scope's
// socket is reachable.
func (m *Manager) toolSessionOK(sc toolScope, sid string) bool {
	s, ok := m.st.Session(sid)
	if !ok || s.Owner != sc.uid {
		return false
	}
	p, ok := m.st.Project(s.ProjectID)
	if !ok {
		return false
	}
	if sc.project == "" {
		return p.Home && p.OwnerUser == sc.uid
	}
	return p.ID == sc.project && !p.Home
}

// toolsSpec mounts the scope's tools socket and Collie's binary (the MCP
// bridge) into the container.
func (m *Manager) toolsSpec(sp *sandbox.Spec, sc toolScope) error {
	dir, err := m.toolSocket(sc)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	sp.CollieRun = dir
	sp.CollieBin = exe
	return nil
}

// collieTools is how a session's agent starts Collie's MCP server: the
// bridge to its tools socket, told which session it serves.
func (m *Manager) collieTools(p model.Project, s model.Session) (string, []string, error) {
	sc := toolScope{uid: s.Owner}
	if !p.Home {
		sc.project = p.ID
	}
	dir, err := m.toolSocket(sc)
	if err != nil {
		return "", nil, err
	}
	if p.Isolated() {
		return sandbox.CollieBinPath, []string{"mcp-bridge", sandbox.CollieRunDir + "/" + toolSockName, s.ID}, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	return exe, []string{"mcp-bridge", filepath.Join(dir, toolSockName), s.ID}, nil
}

// ---- show_media ----

const maxMediaFiles = 20

// mediaTools are offered to every session's agent and may be used unasked:
// they only show the user files the agent can already read.
var mediaTools = []string{"show_media"}

var showMediaDef = map[string]any{
	"name": "show_media",
	"description": "Show files to the user in their Collie chat: screenshots, design mockups, screen recordings, or an HTML page (a report, an explanation, a mockup). " +
		"They appear inline in the conversation and open full screen. Use it whenever seeing the result helps the user review your work: " +
		"several design options at once (caption each, e.g. \"A: …\", so they can answer with a letter), before/after screenshots, a recording of a feature working. " +
		"Supported: PNG, JPEG, GIF, WebP, SVG, MP4 and WebM (videos up to 100 MB, the rest up to 25 MB), and HTML. " +
		"HTML is previewed in a locked-down sandbox: scripts run only if the user allows them, and nothing is loaded from the network or from other files, " +
		"so make pages self-contained (inline <style>, images as data: URIs). " +
		"Files are copied when shown: after changing one, show it again.",
	"inputSchema": obj(map[string]any{
		"files": map[string]any{
			"type": "array", "minItems": 1, "maxItems": maxMediaFiles,
			"description": "The files, in the order to show them",
			"items": obj(map[string]any{
				"path":    prop("string", "File path: absolute, or relative to the session's working directory"),
				"caption": prop("string", "Short label shown with the file, e.g. \"B: dark sidebar\""),
			}, "path"),
		},
		"title": prop("string", "One line saying what these are (shown above them)"),
	}, "files"),
	"annotations": map[string]any{"readOnlyHint": true, "destructiveHint": false, "openWorldHint": false},
}

type mediaFile struct {
	Path    string `json:"path"`
	Caption string `json:"caption"`
}

// parseShowMedia reads show_media's arguments, also accepting the shapes
// models sometimes send instead (a JSON-encoded list, plain path strings).
func parseShowMedia(raw json.RawMessage) (string, []mediaFile, error) {
	var a struct {
		Title string          `json:"title"`
		Files json.RawMessage `json:"files"`
		Path  string          `json:"path"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", nil, err
	}
	var files []mediaFile
	list := a.Files
	var s string
	if json.Unmarshal(list, &s) == nil {
		if strings.HasPrefix(strings.TrimSpace(s), "[") {
			list = json.RawMessage(s)
		} else if s != "" {
			files = append(files, mediaFile{Path: s})
			list = nil
		}
	}
	if len(list) > 0 && string(list) != "null" {
		var items []json.RawMessage
		if err := json.Unmarshal(list, &items); err != nil {
			return "", nil, errors.New("files must be a list of {path, caption}")
		}
		for _, it := range items {
			var f mediaFile
			if json.Unmarshal(it, &s) == nil {
				f.Path = s
			} else if err := json.Unmarshal(it, &f); err != nil {
				return "", nil, errors.New("files must be a list of {path, caption}")
			}
			files = append(files, f)
		}
	}
	if a.Path != "" {
		files = append(files, mediaFile{Path: a.Path})
	}
	for i := range files {
		files[i].Path = strings.TrimSpace(files[i].Path)
		files[i].Caption = clip(strings.TrimSpace(files[i].Caption), 300)
		if files[i].Path == "" {
			return "", nil, errors.New("a file has no path")
		}
	}
	switch {
	case len(files) == 0:
		return "", nil, errors.New("no files given")
	case len(files) > maxMediaFiles:
		return "", nil, fmt.Errorf("at most %d files at once", maxMediaFiles)
	}
	return clip(strings.TrimSpace(a.Title), 500), files, nil
}

func (m *Manager) toolShowMedia(tc *toolConn, raw json.RawMessage) (string, error) {
	if tc.session == "" {
		return "", errors.New("Collie does not know which session this is; it needs a restart of the session (stop it, then send a message)")
	}
	s, ok := m.st.Session(tc.session)
	if !ok {
		return "", errSessionGone
	}
	p, ok := m.st.Project(s.ProjectID)
	if !ok {
		return "", errSessionGone
	}
	title, files, err := parseShowMedia(raw)
	if err != nil {
		return "", err
	}
	var media []model.Media
	var names []string
	for _, f := range files {
		path := f.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(s.Cwd, path)
		}
		path = filepath.Clean(path)
		name, ext, err := m.copyAgentFile(p, s, path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", f.Path, err)
		}
		media = append(media, model.Media{ID: name, Mime: uploadTypes[ext], Caption: f.Caption, Name: filepath.Base(path)})
		names = append(names, filepath.Base(path))
	}
	r, err := m.runtime(s.ID)
	if err != nil {
		return "", err
	}
	r.sinkPut(&model.Item{ID: "m-" + model.NewID(), Kind: model.KindMedia, Text: title, Media: media, Done: true})
	return fmt.Sprintf("Shown to the user in Collie: %s.", strings.Join(names, ", ")), nil
}

// copyAgentFile copies a file the agent shows into the uploads.
func (m *Manager) copyAgentFile(p model.Project, s model.Session, path string) (string, string, error) {
	release, err := m.media.acquire(s.Owner)
	if err != nil {
		return "", "", err
	}
	defer release()
	data, err := m.readAgentFile(p, s, path, maxVideo)
	if err != nil {
		return "", "", err
	}
	ext := sniffUpload(path, data, true)
	if ext == "" {
		return "", "", errors.New("Collie shows PNG, JPEG, GIF, WebP, SVG, MP4, WebM (100 MB at most) and HTML (25 MB at most) files only")
	}
	name, err := m.saveSessionUpload(s.ID, data, ext)
	return name, ext, err
}

// Each file show_media copies is held in memory whole (up to maxVideo)
// while it is read: only so many at once, and only a few per user, so
// that agents calling it on many connections can't exhaust the server's
// memory.
const (
	mediaReadsPerUser = 2
	mediaReadsAll     = 6
)

var mediaReadWait = 2 * time.Minute // a variable for tests

var errMediaBusy = errors.New("Collie is busy copying other files to show; try again in a moment")

type mediaGate struct {
	mu   sync.Mutex
	all  chan struct{}
	user map[string]chan struct{}
}

// acquire waits for a slot to read a file for the user, and returns what
// frees it.
func (g *mediaGate) acquire(uid string) (func(), error) {
	g.mu.Lock()
	if g.all == nil {
		g.all = make(chan struct{}, mediaReadsAll)
		g.user = map[string]chan struct{}{}
	}
	u := g.user[uid]
	if u == nil {
		u = make(chan struct{}, mediaReadsPerUser)
		g.user[uid] = u
	}
	g.mu.Unlock()
	t := time.NewTimer(mediaReadWait)
	defer t.Stop()
	select {
	case u <- struct{}{}:
	case <-t.C:
		return nil, errMediaBusy
	}
	select {
	case g.all <- struct{}{}:
	case <-t.C:
		<-u
		return nil, errMediaBusy
	}
	return func() { <-g.all; <-u }, nil
}

// readAgentFile reads a file as the session's agent sees it.
func (m *Manager) readAgentFile(p model.Project, s model.Session, path string, limit int64) ([]byte, error) {
	if p.Isolated() {
		if m.sb == nil || !m.sb.Available() {
			return nil, errors.New("podman is not available")
		}
		return m.sb.ReadFile(p.ID, s.Owner, path, limit)
	}
	if !m.isAdmin(s.Owner) {
		return nil, errHostOnly
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("larger than %d MB", limit>>20)
	}
	return data, nil
}

// blob keeps an image a session's agent saw in a tool result.
func (m *Manager) blob(sid string, data []byte) string {
	ext := sniffUpload("", data, false)
	if ext == "" {
		return ""
	}
	name, err := m.saveSessionUpload(sid, data, ext)
	if err != nil {
		log.Printf("keep tool image: %v", err)
		return ""
	}
	return name
}
