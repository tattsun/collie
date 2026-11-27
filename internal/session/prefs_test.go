package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/model"
)

func ptr[T any](v T) *T { return &v }

func TestSessionPrefs(t *testing.T) {
	m, _ := newTestManager(t)
	p, err := m.PatchSessionPrefs("alice", SessionPrefsPatch{
		Agents: map[string]AgentDefaults{
			"claude": {Model: " opus ", Effort: "high"},
			"codex":  {Effort: "xhigh"},
		},
		Instructions: ptr("  Write PR titles in English.\r\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Agents["claude"] != (AgentDefaults{Model: "opus", Effort: "high"}) || p.Instructions != "Write PR titles in English." {
		t.Errorf("saved: %+v", p)
	}
	if got := m.SessionPrefs("alice"); got.Agents["codex"].Effort != "xhigh" || got.Instructions != p.Instructions {
		t.Errorf("read back: %+v", got)
	}
	if got := m.SessionPrefs("bob"); len(got.Agents) != 0 || got.Instructions != "" {
		t.Errorf("bob sees alice's preferences: %+v", got)
	}

	// The defaults fill in only what is not asked for; "default" is the agent's own.
	for _, c := range []struct{ uid, agent, model, effort, wantModel, wantEffort string }{
		{"alice", "claude", "", "", "opus", "high"},
		{"alice", "claude", "sonnet", "", "sonnet", "high"},
		{"alice", "claude", "default", "default", "", ""},
		{"alice", "codex", "", "low", "", "low"},
		{"bob", "claude", "", "", "", ""},
	} {
		if mo, ef := m.ResolveModelEffort(c.uid, c.agent, c.model, c.effort); mo != c.wantModel || ef != c.wantEffort {
			t.Errorf("ResolveModelEffort(%s, %s, %q, %q) = %q, %q; want %q, %q", c.uid, c.agent, c.model, c.effort, mo, ef, c.wantModel, c.wantEffort)
		}
	}

	// Only what the patch names changes; an empty entry clears an agent.
	p, err = m.PatchSessionPrefs("alice", SessionPrefsPatch{Agents: map[string]AgentDefaults{"codex": {Model: "default"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Agents["codex"]; ok || p.Agents["claude"].Model != "opus" || p.Instructions == "" {
		t.Errorf("after clearing codex: %+v", p)
	}

	for _, c := range []struct {
		req  SessionPrefsPatch
		want string
	}{
		{SessionPrefsPatch{Agents: map[string]AgentDefaults{"codex": {Effort: "max"}}}, `unknown effort "max" for codex (one of: low, medium, high, xhigh)`},
		{SessionPrefsPatch{Agents: map[string]AgentDefaults{"gemini": {}}}, `unknown agent "gemini"`},
		{SessionPrefsPatch{Instructions: ptr(strings.Repeat("x", maxInstructions+1))}, "too long"},
	} {
		if _, err := m.PatchSessionPrefs("alice", c.req); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.req, err, c.want)
		}
	}
	if got := m.SessionPrefs("alice"); got.Agents["claude"].Effort != "high" {
		t.Errorf("a refused patch changed something: %+v", got)
	}
	if _, err := m.PatchSessionPrefs("../alice", SessionPrefsPatch{Instructions: ptr("x")}); err == nil {
		t.Error("a user id stepped out of the users folder")
	}
}

func TestSessionsTakeOnlyTheAgentsEfforts(t *testing.T) {
	m, _ := newTestManager(t)
	m.st.PutProject(model.Project{ID: "p1", Name: "work", Path: t.TempDir(), OwnerUser: "root"})
	root := Actor{ID: "root", Admin: true}
	if _, err := m.CreateSession(root, CreateSessionReq{ProjectID: "p1", Agent: "codex", Effort: "max"}); err == nil ||
		!strings.Contains(err.Error(), `unknown effort "max" for codex`) {
		t.Errorf("codex with effort max: %v", err)
	}
	s, err := m.CreateSession(root, CreateSessionReq{ProjectID: "p1", Agent: "claude", Model: "opus", Effort: "max"})
	if err != nil || s.Model != "opus" || s.Effort != "max" {
		t.Errorf("claude with effort max: %+v %v", s, err)
	}
}

func TestHomeChoosesModelAndEffort(t *testing.T) {
	m := newHomeTestManager(t)
	m.SetAccess(allAdmins{})
	m.cfg.ClaudeBin = fakeAgent(t, "claude", filepath.Join(t.TempDir(), "started"))
	m.st.PutProject(model.Project{ID: "pa", Name: "alpha", Path: t.TempDir(), OwnerUser: "alice"})
	m.st.PutSession(model.Session{ID: "a1", ProjectID: "pa", Owner: "alice", Agent: "claude", Title: "fix login",
		Model: "claude-opus-5-5", Effort: "xhigh", Status: model.StatusIdle, UpdatedAt: model.NowMs()})
	m.st.PutSession(model.Session{ID: "a2", ProjectID: "pa", Owner: "alice", Agent: "codex", Title: "docs", Status: model.StatusIdle})

	text, _ := call(t, m, "alice", "list_sessions", nil)
	if !strings.Contains(text, `"model": "claude-opus-5-5"`) || !strings.Contains(text, `"effort": "xhigh"`) || !strings.Contains(text, `"effort": "default"`) {
		t.Errorf("list_sessions does not show models and efforts: %s", text)
	}
	text, _ = call(t, m, "alice", "read_session", map[string]any{"session": "a1"})
	if !strings.Contains(text, "claude (model claude-opus-5-5, effort xhigh)") {
		t.Errorf("read_session: %s", text)
	}

	if _, err := m.PatchSessionPrefs("alice", SessionPrefsPatch{Agents: map[string]AgentDefaults{"claude": {Model: "sonnet", Effort: "high"}}}); err != nil {
		t.Fatal(err)
	}
	start := func(args map[string]any) model.Session {
		t.Helper()
		text, isErr := call(t, m, "alice", "start_session", args)
		if isErr {
			t.Fatalf("start_session %v: %s", args, text)
		}
		var res struct{ ID, Model, Effort string }
		if err := json.Unmarshal([]byte(text), &res); err != nil {
			t.Fatal(err)
		}
		// The fake agent fails at once; wait for that, so nothing writes after the test.
		deadline := time.Now().Add(10 * time.Second)
		for {
			s, _ := m.st.Session(res.ID)
			if s.Status == model.StatusError || time.Now().After(deadline) {
				if orDefault(s.Model) != res.Model || orDefault(s.Effort) != res.Effort {
					t.Errorf("start_session answered %+v for %+v", res, s)
				}
				return s
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if s := start(map[string]any{"project": "alpha", "prompt": "go"}); s.Model != "sonnet" || s.Effort != "high" {
		t.Errorf("without a model or effort: %q %q, want the user's defaults", s.Model, s.Effort)
	}
	if s := start(map[string]any{"project": "alpha", "prompt": "go", "model": "opus", "effort": "max"}); s.Model != "opus" || s.Effort != "max" {
		t.Errorf("with opus and max: %q %q", s.Model, s.Effort)
	}
	if s := start(map[string]any{"project": "alpha", "prompt": "go", "model": "default", "effort": "default"}); s.Model != "" || s.Effort != "" {
		t.Errorf("with the agent's defaults: %q %q", s.Model, s.Effort)
	}
	text, isErr := call(t, m, "alice", "start_session", map[string]any{"project": "alpha", "prompt": "go", "agent": "codex", "effort": "max"})
	if !isErr || !strings.Contains(text, `unknown effort "max" for codex (one of: low, medium, high, xhigh)`) {
		t.Errorf("start_session with an effort codex lacks: %s", text)
	}
	if n := len(m.VisibleSessions("alice")); n != 5 {
		t.Errorf("%d sessions, want 5", n)
	}

	modelHelp := func() string {
		for _, x := range rpc(t, m, "alice", "tools/list", nil)["result"].(map[string]any)["tools"].([]any) {
			if d := x.(map[string]any); d["name"] == "start_session" {
				props := d["inputSchema"].(map[string]any)["properties"].(map[string]any)
				if !strings.Contains(props["effort"].(map[string]any)["description"].(string), "Codex: low, medium, high, xhigh.") {
					t.Errorf("effort description: %v", props["effort"])
				}
				return props["model"].(map[string]any)["description"].(string)
			}
		}
		t.Fatal("no start_session")
		return ""
	}
	before := modelHelp()
	m.caps = map[string]model.Caps{"codex": {Models: []model.ModelOpt{{ID: "gpt-5.5"}, {ID: "gpt-5.5-mini"}}}}
	if got := modelHelp(); !strings.Contains(got, "Offered now: codex: gpt-5.5, gpt-5.5-mini") || strings.Contains(before, "gpt") {
		t.Errorf("model description: %q, before the models were known: %q", got, before)
	}
	m.caps = nil
	if got := modelHelp(); got != before {
		t.Errorf("listing the models changed the shared definitions: %q", got)
	}
	if !strings.Contains(homeInstructions, "`model` / `effort`") {
		t.Error("Home's instructions do not mention model and effort")
	}
}

type allAdmins struct{ fakeAccess }

func (allAdmins) IsAdmin(string) bool { return true }

// fakeAgent writes an agent that records what it was started with to log:
// Claude's command line, or Codex's thread/start request.
func fakeAgent(t *testing.T, kind, log string) string {
	t.Helper()
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + log + "'\nexit 1\n"
	if kind == "codex" {
		script = "#!/bin/sh\nread -r line\nprintf '%s\\n' '{\"id\":1,\"result\":{}}'\nread -r line\nread -r line\nprintf '%s\\n' \"$line\" > '" + log + "'\nexit 1\n"
	}
	p := filepath.Join(t.TempDir(), kind)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// Each user's instructions reach only the agents of the sessions they
// started, even in a group project where others can see and drive them.
func TestInstructionsReachOnlyTheirOwnersAgents(t *testing.T) {
	m := newHomeTestManager(t)
	m.SetAccess(allAdmins{fakeAccess{"team": {"alice": "owner", "bob": "member"}}})
	log := filepath.Join(t.TempDir(), "started")
	m.cfg.ClaudeBin, m.cfg.CodexBin = fakeAgent(t, "claude", log), fakeAgent(t, "codex", log)
	m.st.PutProject(model.Project{ID: "pt", Name: "team", Path: t.TempDir(), OwnerGroup: "team"})
	for uid, text := range map[string]string{"alice": "ALICE: titles in English", "bob": "BOB: answer in Japanese"} {
		if _, err := m.PatchSessionPrefs(uid, SessionPrefsPatch{Instructions: ptr(text)}); err != nil {
			t.Fatal(err)
		}
	}
	started := func(sid string) string {
		t.Helper()
		_ = os.Remove(log)
		r, err := m.runtime(sid)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.startDriver(false); err == nil {
			t.Fatal("the fake agent started")
		}
		b, err := os.ReadFile(log)
		if err != nil {
			t.Fatalf("%s: the agent did not run: %v", sid, err)
		}
		return string(b)
	}
	for _, c := range []struct {
		sid, owner, agent, want, notWant string
	}{
		{"s-alice", "alice", "claude", "--append-system-prompt\nALICE: titles in English\n", "BOB"},
		{"s-bob", "bob", "claude", "--append-system-prompt\nBOB: answer in Japanese\n", "ALICE"},
		{"x-bob", "bob", "codex", `"developerInstructions":"BOB: answer in Japanese"`, "ALICE"},
		{"s-carol", "carol", "claude", "", "--append-system-prompt"},
		{"x-carol", "carol", "codex", `"method":"thread/start"`, "developerInstructions"},
	} {
		m.st.PutSession(model.Session{ID: c.sid, ProjectID: "pt", Owner: c.owner, Agent: c.agent, Shared: true, Cwd: t.TempDir()})
		got := started(c.sid)
		if !strings.Contains(got, c.want) || strings.Contains(got, c.notWant) {
			t.Errorf("%s (%s's %s) started with:\n%s", c.sid, c.owner, c.agent, got)
		}
	}
	if !slices.Contains(strings.Split(started("s-alice"), "\n"), "--session-id") {
		t.Error("the fake claude did not record its command line")
	}
}
