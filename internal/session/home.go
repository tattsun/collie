package session

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tattsun/collie/internal/agent"
	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
)

// Home: one per user, outside any project. The user talks with the Home
// agent about anything, and it coordinates their project agents through
// Collie's tools (an MCP server), which Collie serves on a per-user Unix
// socket that only that user's Home sees. The Home agent reads the user's
// projects (mounted read-only) but changes them only through the sessions
// it starts or instructs, whose approvals still go to the user.

var (
	errHomeUnavailable = errors.New("Home needs podman on the server (it runs in a container)")
	errHomeFixed       = errors.New("Home cannot be renamed, moved or deleted")
)

// homeReadTools never change anything, so agents may call them unasked.
var homeReadTools = []string{"list_projects", "list_sessions", "read_session", "list_schedules"}

func (m *Manager) homeOf(uid string) (model.Project, bool) {
	for _, p := range m.st.Projects() {
		if p.Home && p.OwnerUser == uid {
			return p, true
		}
	}
	return model.Project{}, false
}

func (m *Manager) homeWorkspace(uid string) string {
	return filepath.Join(m.st.Dir(), "users", uid, "home")
}

// EnsureHome returns the user's Home, creating it on first use, and starts
// its tools socket.
func (m *Manager) EnsureHome(a Actor) (model.Project, error) {
	p, err := m.ensureHome(a)
	if err != nil {
		return p, err
	}
	if _, err := m.homeSocket(a.ID); err != nil {
		log.Printf("home: tools socket for %s: %v", a.ID, err)
	}
	return p, nil
}

func (m *Manager) ensureHome(a Actor) (model.Project, error) {
	m.homeMu.Lock()
	defer m.homeMu.Unlock()
	if p, ok := m.homeOf(a.ID); ok {
		return p, nil
	}
	runtime := "container"
	if m.sb == nil || !m.sb.Available() {
		if !a.Admin {
			return model.Project{}, errHomeUnavailable
		}
		runtime = "host"
	}
	dir := m.homeWorkspace(a.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return model.Project{}, err
	}
	p := model.Project{ID: model.NewID(), Name: "Home", Path: dir, CreatedAt: model.NowMs(),
		OwnerUser: a.ID, CreatedBy: a.ID, Runtime: runtime, Home: true}
	m.st.PutProject(p)
	m.pushProject(p)
	return p, nil
}

// homeMounts lists the project folders a user's Home sees (read-only).
// Folders holding Collie's data (or inside it) are left out: they would
// expose other users' secrets.
func (m *Manager) homeMounts(uid string) []string {
	data := m.st.Dir()
	seen := map[string]bool{}
	var out []string
	for _, p := range m.VisibleProjects(uid) {
		if p.Home || seen[p.Path] || within(data, p.Path) || within(p.Path, data) {
			continue
		}
		if fi, err := os.Stat(p.Path); err != nil || !fi.IsDir() {
			continue
		}
		seen[p.Path] = true
		out = append(out, p.Path)
	}
	sort.Strings(out)
	return out
}

// homeSpec adds what a Home container needs: the projects, and the tools
// socket with the binary that bridges to it.
func (m *Manager) homeSpec(sp *sandbox.Spec, uid string) error {
	sp.ReadOnly = m.homeMounts(uid)
	return m.toolsSpec(sp, toolScope{uid: uid})
}

// homeSocket starts the user's Home tools socket.
func (m *Manager) homeSocket(uid string) (string, error) { return m.toolSocket(toolScope{uid: uid}) }

// collieClaudeArgs adds Collie's tools to Claude Code's arguments (next to
// any MCP config already given) and lets it use the allowed ones unasked.
func collieClaudeArgs(base []string, cmd string, args []string, allowed []string) []string {
	cfg := string(mustJSON(map[string]any{"mcpServers": map[string]any{
		"collie": map[string]any{"command": cmd, "args": args},
	}}))
	out := slices.Clone(base)
	if i := slices.Index(out, "--mcp-config"); i >= 0 && i+1 < len(out) {
		out = slices.Insert(out, i+2, cfg)
	} else {
		out = append(out, "--mcp-config", cfg)
	}
	if len(allowed) > 0 {
		out = append(out, "--allowedTools")
		for _, t := range allowed {
			out = append(out, "mcp__collie__"+t)
		}
	}
	return out
}

// collieCodexArgs adds Collie's tools to Codex's configuration for this
// process only (CODEX_HOME is shared by all of the user's containers).
func collieCodexArgs(base []string, cmd string, args []string) []string {
	return append(slices.Clone(base),
		"-c", "mcp_servers.collie.command="+strconv.Quote(cmd),
		"-c", "mcp_servers.collie.args="+string(mustJSON(args)))
}

const homeInstructions = `<!-- Managed by Collie: rewritten whenever a Home session starts. Keep your own notes in another file (e.g. NOTES.md). -->
# Collie Home

You are the user's **Home agent** in Collie, the web app where they run coding agents (Claude Code and Codex) in their projects, often from a phone. The user comes here to talk about anything — questions, ideas, planning, advice, everyday consultations — and to have you coordinate work across their projects.

## Your tools (the ` + "`collie`" + ` MCP server)
- ` + "`list_projects`, `list_sessions`, `read_session`" + `: the user's projects, what each session is doing, and what was said in it.
- ` + "`start_session`" + `: start an agent in a project with your instructions. It runs with the user's default model and reasoning effort for that agent unless you pass ` + "`model` / `effort`" + `; the effort cannot be changed once it runs.
- ` + "`send_message`" + `: give an existing session more instructions.
- ` + "`create_schedule`, `list_schedules`, `delete_schedule`" + `: start a session in a project (or here in Home) at set times, with the same instructions every time.
- ` + "`show_media`" + `: show the user images, videos or an HTML page right in this chat.

## How to work
- You are the coordinator. Changes to a project are made by a session in that project: start one (or instruct an existing one) with clear, self-contained instructions — that agent does not see this conversation.
- The user's projects are mounted read-only at the paths ` + "`list_projects`" + ` shows. Read code there to answer questions or to write better instructions. You cannot edit them; this folder is your own writable workspace.
- Sessions' permission prompts and questions go to the user (Collie shows them under "Needs you"); you cannot answer them. A message sent to a session that is waiting for an approval rejects the pending request with your text, so leave approvals to the user unless they ask otherwise.
- Leave ` + "`model` and `effort`" + ` out unless the user asks for them or the task clearly calls for it (a higher effort for hard design or debugging work, a lower one for a quick lookup). ` + "`list_sessions` and `read_session`" + ` show what each session runs with.
- The user's instructions for all sessions (set in Collie) reach every session they start, including yours and the ones you start, so don't repeat them in prompts.
- Confirm with the user before starting work that is large, costly or hard to undo. Clearly requested, simple tasks need no confirmation.
- After starting or instructing a session, tell the user its title and project so they can follow it in Collie. When asked how it is going, check with ` + "`read_session`" + `.
- For recurring work ("every morning at 9…", "every Friday at 18:00…"), make a schedule with ` + "`create_schedule`" + `. Its runs start while the user may be away: write instructions that stand on their own, and use the user's time zone (ask if you cannot tell). Tell them when it first runs; they can pause, change or run it in Collie under Schedules.
- Answer in the user's language. Keep replies short and conversational; they are often read on a phone.
`

// writeHomeInstructions (re)writes the Home agent's instructions into its
// workspace, where Claude Code (CLAUDE.md) and Codex (AGENTS.md) read them.
func writeHomeInstructions(dir string) {
	for _, n := range []string{"CLAUDE.md", "AGENTS.md"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(homeInstructions), 0o600); err != nil {
			log.Printf("home: write %s: %v", n, err)
		}
	}
}

// ---- MCP (JSON-RPC 2.0, one message per line) ----

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (m *Manager) serveTools(tc *toolConn, c net.Conn) {
	defer c.Close()
	r := bufio.NewReaderSize(c, 64<<10)
	enc := json.NewEncoder(c)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if resp := m.handleRPC(tc, line); resp != nil {
				if enc.Encode(resp) != nil {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func rpcResult(id json.RawMessage, v any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": v}
}

func rpcError(id json.RawMessage, code int, msg string) map[string]any {
	if id == nil {
		id = json.RawMessage("null")
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}}
}

func toolText(text string, isErr bool) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isErr}
}

const (
	homeServerInstructions  = "Collie: see the user's projects and sessions, start or instruct sessions in their projects, schedule recurring sessions, and show the user files with show_media."
	mediaServerInstructions = "Collie is the app the user follows this session in, often on a phone. show_media puts files in front of them: screenshots, design options, " +
		"screen recordings (e.g. Playwright's recordVideo), HTML reports. Use it when seeing the result helps them review your work."
)

func (m *Manager) handleRPC(tc *toolConn, line []byte) (resp any) {
	var req rpcMsg
	if err := json.Unmarshal(line, &req); err != nil {
		return rpcError(nil, -32700, "parse error")
	}
	if req.Method == "collie/session" {
		// Sent by the bridge first: the session whose agent this is.
		var p struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(req.Params, &p) == nil && m.toolSessionOK(tc.scope, p.ID) {
			tc.session = p.ID
		}
		return nil
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return nil // a notification: nothing to answer
	}
	home := tc.scope.project == ""
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		return rpcResult(req.ID, map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "collie", "version": "1"},
			"instructions":    map[bool]string{true: homeServerInstructions, false: mediaServerInstructions}[home],
		})
	case "ping":
		return rpcResult(req.ID, map[string]any{})
	case "tools/list":
		tools := []map[string]any{showMediaDef}
		if home {
			tools = append(withModels(append(slices.Clone(homeToolDefs), scheduleToolDefs...), m.AgentCaps()), showMediaDef)
		}
		return rpcResult(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return rpcError(req.ID, -32602, "invalid params")
		}
		defer func() {
			if e := recover(); e != nil {
				log.Printf("home tool %s: panic: %v", p.Name, e)
				resp = rpcResult(req.ID, toolText("internal error", true))
			}
		}()
		var text string
		var err error
		switch {
		case p.Name == "show_media":
			text, err = m.toolShowMedia(tc, orEmpty(p.Arguments))
		case home:
			text, err = m.callTool(tc.scope.uid, p.Name, p.Arguments)
		default:
			err = fmt.Errorf("unknown tool %q", p.Name)
		}
		if err != nil {
			return rpcResult(req.ID, toolText(err.Error(), true))
		}
		return rpcResult(req.ID, toolText(text, false))
	}
	return rpcError(req.ID, -32601, "method not found: "+req.Method)
}

func orEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("{}")
	}
	return raw
}

func obj(props map[string]any, required ...string) map[string]any {
	o := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

func prop(typ, desc string) map[string]any { return map[string]any{"type": typ, "description": desc} }

var effortHelp = `Reasoning effort (default: the user's default for that agent; "default" is the agent's own). Claude: ` +
	strings.Join(agent.Efforts["claude"], ", ") + ". Codex: " + strings.Join(agent.Efforts["codex"], ", ") + ". It cannot be changed once the session runs"

var homeToolDefs = []map[string]any{
	{
		"name":        "list_projects",
		"description": "List the user's projects: id, name, folder (mounted read-only here), session counts and last activity.",
		"inputSchema": obj(map[string]any{}),
	},
	{
		"name":        "list_sessions",
		"description": "List sessions (agent conversations), most recent first, with their model, reasoning effort and status: running, waiting (needs the user), idle, stopped or error. `background` lists work an agent left running after its turn (builds, tests, sub-agents); it reports back when that ends.",
		"inputSchema": obj(map[string]any{
			"project":          prop("string", "Project id or name; omit for all projects"),
			"include_archived": prop("boolean", "Include archived sessions"),
			"limit":            prop("integer", "Maximum number of sessions (default 30)"),
		}),
	},
	{
		"name":        "read_session",
		"description": "Read the latest part of a session's conversation: messages, tool calls, pending approvals and results.",
		"inputSchema": obj(map[string]any{
			"session": prop("string", "Session id"),
			"last":    prop("integer", "How many recent entries to read (default 40, at most 200)"),
		}, "session"),
	},
	{
		"name": "start_session",
		"description": "Start an agent session in a project and give it instructions. The agent does not see this conversation, so make the prompt self-contained. " +
			"Its permission prompts go to the user. Returns the new session's id, title, model and effort.",
		"inputSchema": obj(map[string]any{
			"project":  prop("string", "Project id or name"),
			"prompt":   prop("string", "Instructions for the agent"),
			"agent":    map[string]any{"type": "string", "enum": []string{"claude", "codex"}, "description": "Which agent (default claude)"},
			"mode":     prop("string", "Permission mode (default auto). Claude: auto, default (ask first), acceptEdits, plan. Codex: auto, untrusted, read-only"),
			"model":    prop("string", `Model (default: the user's default for that agent). An alias or model id the agent accepts; "default" is the agent's own default`),
			"effort":   prop("string", effortHelp),
			"worktree": prop("boolean", "Work on a separate git branch and checkout, so it does not collide with other sessions in the project"),
			"title":    prop("string", "Short title (default: from the prompt)"),
		}, "project", "prompt"),
	},
	{
		"name": "send_message",
		"description": "Send more instructions to an existing session; it is queued if the agent is busy. " +
			"If the session is waiting for an approval, the message rejects that request with your text as the reason.",
		"inputSchema": obj(map[string]any{
			"session": prop("string", "Session id"),
			"text":    prop("string", "The message"),
		}, "session", "text"),
	},
}

// withModels returns the tool definitions with the models each agent
// offers (as last reported) added to the model descriptions of
// start_session and create_schedule.
func withModels(defs []map[string]any, caps map[string]model.Caps) []map[string]any {
	var known []string
	for _, name := range []string{"claude", "codex"} {
		var ids []string
		for _, mo := range caps[name].Models {
			ids = append(ids, mo.ID)
		}
		if len(ids) > 0 {
			known = append(known, name+": "+strings.Join(ids, ", "))
		}
	}
	out := slices.Clone(defs)
	for i, d := range out {
		if (d["name"] != "start_session" && d["name"] != "create_schedule") || len(known) == 0 {
			continue
		}
		schema := maps.Clone(d["inputSchema"].(map[string]any))
		props := maps.Clone(schema["properties"].(map[string]any))
		mp := maps.Clone(props["model"].(map[string]any))
		mp["description"] = mp["description"].(string) + ". Offered now: " + strings.Join(known, "; ")
		props["model"], schema["properties"] = mp, props
		def := maps.Clone(d)
		def["inputSchema"] = schema
		out[i] = def
	}
	return out
}

// ---- tools ----

func (m *Manager) callTool(uid, name string, raw json.RawMessage) (string, error) {
	raw = orEmpty(raw)
	a := Actor{ID: uid, Admin: m.isAdmin(uid)}
	switch name {
	case "list_projects":
		return m.toolListProjects(a)
	case "list_sessions":
		var p struct {
			Project  string `json:"project"`
			Archived bool   `json:"include_archived"`
			Limit    int    `json:"limit"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return "", err
		}
		return m.toolListSessions(a, p.Project, p.Archived, p.Limit)
	case "read_session":
		var p struct {
			Session string `json:"session"`
			Last    int    `json:"last"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return "", err
		}
		return m.toolReadSession(a, p.Session, p.Last)
	case "start_session":
		var p struct {
			Project  string `json:"project"`
			Prompt   string `json:"prompt"`
			Agent    string `json:"agent"`
			Mode     string `json:"mode"`
			Model    string `json:"model"`
			Effort   string `json:"effort"`
			Worktree bool   `json:"worktree"`
			Title    string `json:"title"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return "", err
		}
		return m.toolStartSession(a, p.Project, CreateSessionReq{Prompt: p.Prompt, Agent: p.Agent, Mode: p.Mode,
			Model: p.Model, Effort: p.Effort, Title: p.Title, Worktree: p.Worktree})
	case "send_message":
		var p struct {
			Session string `json:"session"`
			Text    string `json:"text"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return "", err
		}
		return m.toolSendMessage(a, p.Session, p.Text)
	}
	if text, ok, err := m.callScheduleTool(a, name, raw); ok {
		return text, err
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

func ago(ms int64) string {
	if ms <= 0 {
		return ""
	}
	d := time.Since(time.UnixMilli(ms))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func pretty(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// workProject resolves a project the Home agent may act on, by id or name.
func (m *Manager) workProject(uid, ref string) (model.Project, error) {
	ref = strings.TrimSpace(ref)
	var named []model.Project
	for _, p := range m.VisibleProjects(uid) {
		if p.Home {
			continue
		}
		if p.ID == ref {
			return p, nil
		}
		if strings.EqualFold(p.Name, ref) {
			named = append(named, p)
		}
	}
	switch len(named) {
	case 1:
		return named[0], nil
	case 0:
		return model.Project{}, fmt.Errorf("no project %q (see list_projects)", ref)
	}
	return model.Project{}, fmt.Errorf("several projects are named %q; use its id", ref)
}

// workSession resolves a session the Home agent may act on.
func (m *Manager) workSession(uid, sid string) (model.Session, model.Project, error) {
	s, err := m.visibleSession(uid, strings.TrimSpace(sid))
	if err != nil {
		return model.Session{}, model.Project{}, fmt.Errorf("no session %q (see list_sessions)", sid)
	}
	p, _ := m.st.Project(s.ProjectID)
	if p.Home {
		return model.Session{}, model.Project{}, errors.New("that is a Home conversation, not a project session")
	}
	return s, p, nil
}

func (m *Manager) toolListProjects(a Actor) (string, error) {
	type counts struct{ total, active, waiting int }
	per := map[string]*counts{}
	last := map[string]int64{}
	for _, s := range m.VisibleSessions(a.ID) {
		if s.Archived {
			continue
		}
		c := per[s.ProjectID]
		if c == nil {
			c = &counts{}
			per[s.ProjectID] = c
		}
		c.total++
		switch s.Status {
		case model.StatusRunning, model.StatusStarting:
			c.active++
		case model.StatusWaiting:
			c.waiting++
		}
		last[s.ProjectID] = max(last[s.ProjectID], s.UpdatedAt)
	}
	type out struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Path         string `json:"path"`
		Group        string `json:"group,omitempty"`
		Runtime      string `json:"runtime"`
		Git          bool   `json:"git"`
		Sessions     int    `json:"sessions"`
		Running      int    `json:"running,omitempty"`
		NeedsUser    int    `json:"needsUser,omitempty"`
		LastActivity string `json:"lastActivity,omitempty"`
	}
	list := []out{}
	for _, p := range m.VisibleProjects(a.ID) {
		if p.Home {
			continue
		}
		o := out{ID: p.ID, Name: p.Name, Path: p.Path, Runtime: p.Runtime, Git: p.Git, LastActivity: ago(last[p.ID])}
		if o.Runtime == "" {
			o.Runtime = "host"
		}
		if p.OwnerGroup != "" && m.acc != nil {
			o.Group = m.acc.GroupName(p.OwnerGroup)
		}
		if c := per[p.ID]; c != nil {
			o.Sessions, o.Running, o.NeedsUser = c.total, c.active, c.waiting
		}
		list = append(list, o)
	}
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name) })
	return pretty(list), nil
}

func (m *Manager) toolListSessions(a Actor, project string, archived bool, limit int) (string, error) {
	var only string
	if strings.TrimSpace(project) != "" {
		p, err := m.workProject(a.ID, project)
		if err != nil {
			return "", err
		}
		only = p.ID
	}
	if limit <= 0 {
		limit = 30
	}
	type out struct {
		ID       string `json:"id"`
		Project  string `json:"project"`
		Title    string `json:"title"`
		Agent    string `json:"agent"`
		Model    string `json:"model"`
		Effort   string `json:"effort"`
		Status   string `json:"status"`
		Mode     string `json:"mode"`
		Updated  string `json:"updated"`
		Preview  string `json:"preview,omitempty"`
		Branch   string `json:"branch,omitempty"`
		Worktree bool   `json:"worktree,omitempty"`
		Archived bool   `json:"archived,omitempty"`
		Others   bool   `json:"startedByAnotherMember,omitempty"`
		// Background work the agent left running; it reports when it ends.
		Background []string `json:"background,omitempty"`
	}
	var ss []model.Session
	for _, s := range m.VisibleSessions(a.ID) {
		p, ok := m.st.Project(s.ProjectID)
		if !ok || p.Home || (only != "" && s.ProjectID != only) || (s.Archived && !archived) {
			continue
		}
		ss = append(ss, s)
	}
	sort.Slice(ss, func(i, j int) bool { return ss[i].UpdatedAt > ss[j].UpdatedAt })
	list := []out{}
	for _, s := range ss {
		if len(list) == limit {
			break
		}
		p, _ := m.st.Project(s.ProjectID)
		o := out{ID: s.ID, Project: p.Name, Title: s.Title, Agent: s.Agent, Model: orDefault(s.Model), Effort: orDefault(s.Effort), Status: s.Status, Mode: s.Mode,
			Updated: ago(s.UpdatedAt), Preview: s.Preview, Branch: s.Branch, Worktree: s.Worktree, Archived: s.Archived,
			Others: s.Owner != a.ID}
		for _, t := range s.Tasks {
			o.Background = append(o.Background, cmp.Or(t.Title, t.Kind)+" (started "+ago(t.Started)+")")
		}
		list = append(list, o)
	}
	return pretty(list), nil
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (m *Manager) toolReadSession(a Actor, sid string, last int) (string, error) {
	s, p, err := m.workSession(a.ID, sid)
	if err != nil {
		return "", err
	}
	if last <= 0 {
		last = 40
	}
	last = min(last, 200)
	items, start, err := m.Items(a, s.ID, 0, last)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Session %q (%s) in project %s — %s (model %s, effort %s), mode %s, status %s, updated %s.\n",
		s.Title, s.ID, p.Name, s.Agent, orDefault(s.Model), orDefault(s.Effort), s.Mode, s.Status, ago(s.UpdatedAt))
	if start > 0 {
		fmt.Fprintf(&b, "Showing the last %d entries; %d earlier ones are omitted.\n", len(items), start)
	}
	var parts []string
	for _, it := range items {
		if t := formatItem(it); t != "" {
			parts = append(parts, t)
		}
	}
	// Keep the newest entries within a reasonable size.
	const budget = 40000
	size := 0
	from := len(parts)
	for from > 0 && size+len(parts[from-1]) < budget {
		from--
		size += len(parts[from])
	}
	if from > 0 {
		fmt.Fprintf(&b, "(%d more entries left out for length)\n", from)
	}
	b.WriteString("\n")
	b.WriteString(strings.Join(parts[from:], "\n"))
	return b.String(), nil
}

func formatItem(it *model.Item) string {
	switch it.Kind {
	case model.KindUser:
		who := "user"
		switch it.Via {
		case "home":
			who = "user (sent by you, the Home agent)"
		case "schedule":
			who = "user (sent by one of their schedules)"
		}
		text := clip(it.Text, 4000)
		if len(it.Images) > 0 {
			text += fmt.Sprintf(" [%d image(s)]", len(it.Images))
		}
		return "### " + who + "\n" + text + "\n"
	case model.KindText:
		if strings.TrimSpace(it.Text) == "" {
			return ""
		}
		return "### assistant\n" + clip(it.Text, 4000) + "\n"
	case model.KindTool:
		if it.Tool == nil {
			return ""
		}
		line := fmt.Sprintf("- tool %s %s", it.Tool.Name, clip(string(it.Tool.Input), 200))
		if it.Tool.Status != "" {
			line += " → " + it.Tool.Status
		}
		if it.Tool.Status == "error" && it.Tool.Output != "" {
			line += ": " + clip(it.Tool.Output, 300)
		}
		if len(it.Images) > 0 {
			line += fmt.Sprintf(" [%d image(s)]", len(it.Images))
		}
		return line
	case model.KindMedia:
		var files []string
		for _, f := range it.Media {
			n := f.Name
			if f.Caption != "" {
				n += " (" + clip(f.Caption, 100) + ")"
			}
			files = append(files, n)
		}
		line := "- showed the user " + strings.Join(files, ", ")
		if it.Text != "" {
			line += ": " + clip(it.Text, 300)
		}
		return line
	case model.KindApproval, model.KindQuestion:
		if it.Ask == nil {
			return ""
		}
		what := it.Ask.Title
		if what == "" {
			what = it.Ask.Tool
		}
		if it.Ask.State == "pending" {
			return "- WAITING FOR THE USER: " + clip(what, 300)
		}
		return fmt.Sprintf("- asked the user: %s (%s %s)", clip(what, 200), it.Ask.State, clip(it.Ask.Answer, 100))
	case model.KindPlan:
		var b strings.Builder
		b.WriteString("- plan:")
		for _, st := range it.Plan {
			fmt.Fprintf(&b, "\n  [%s] %s", st.Status, clip(st.Step, 200))
		}
		return b.String()
	case model.KindNotice:
		return "- notice: " + clip(it.Text, 500)
	case model.KindError:
		return "- error: " + clip(it.Text, 1000)
	case model.KindResult:
		if it.Result == nil {
			return "- turn finished"
		}
		st := it.Result.Status
		if st == "" {
			st = "success"
		}
		if it.Result.IsError && it.Text != "" {
			return "- turn failed: " + clip(it.Text, 500)
		}
		return "- turn finished (" + st + ")"
	}
	return ""
}

func (m *Manager) toolStartSession(a Actor, project string, req CreateSessionReq) (string, error) {
	p, err := m.workProject(a.ID, project)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return "", errors.New("prompt is empty")
	}
	if req.Agent == "" {
		req.Agent = "claude"
	}
	switch req.Mode {
	case "bypassPermissions", "dontAsk", "full":
		return "", errors.New("the Home agent cannot start sessions without permission checks; ask the user to start it from Collie")
	}
	req.ProjectID = p.ID
	req.Model, req.Effort = m.ResolveModelEffort(a.ID, req.Agent, req.Model, req.Effort)
	s, err := m.createSession(a, req, "home")
	if err != nil {
		return "", err
	}
	return pretty(map[string]any{"id": s.ID, "title": s.Title, "project": p.Name, "agent": s.Agent, "model": orDefault(s.Model),
		"effort": orDefault(s.Effort), "mode": s.Mode, "status": s.Status, "branch": s.Branch}), nil
}

// orDefault names an unset model or effort for the Home agent: the agent's
// own default.
func orDefault(v string) string { return cmp.Or(v, "default") }

func (m *Manager) toolSendMessage(a Actor, sid, text string) (string, error) {
	s, p, err := m.workSession(a.ID, sid)
	if err != nil {
		return "", err
	}
	waiting := s.Status == model.StatusWaiting
	if err := m.send(a, s.ID, text, nil, "home"); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("Sent to %q in %s.", s.Title, p.Name)
	if waiting {
		msg += " It was waiting for an approval, which this message rejected."
	}
	return msg, nil
}
