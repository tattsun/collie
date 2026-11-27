package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tattsun/collie/internal/model"
)

// The Home agent's schedule tools: the user asks Home for recurring work
// ("every Friday at 18:00, sum up the week's merges") and it sets it up.

var scheduleToolDefs = []map[string]any{
	{
		"name": "create_schedule",
		"description": "Schedule an agent session that starts at set times with the same instructions, e.g. every weekday at 9:00 or every Friday at 18:00. " +
			"Each run is a new session; its agent does not see this conversation, so make the prompt self-contained. " +
			"Runs never overlap: a time is skipped while the previous run is still going. Returns the schedule and its next run times.",
		"inputSchema": obj(map[string]any{
			"project": prop("string", `Project id or name, or "home" to run it here in Home (e.g. a summary across projects)`),
			"prompt":  prop("string", "Instructions for each run's agent"),
			"cron": prop("string", `When, as a cron expression "minute hour day-of-month month day-of-week": "0 9 * * *" every day at 9:00, `+
				`"0 9 * * 1-5" weekdays at 9:00, "0 18 * * 5" Fridays at 18:00, "30 8 1 * *" the 1st of every month at 8:30. At most every 5 minutes`),
			"timezone": prop("string", "IANA time zone of that time, e.g. Asia/Tokyo or America/New_York (default: that of the user's other schedules, else the server's)"),
			"agent":    map[string]any{"type": "string", "enum": []string{"claude", "codex"}, "description": "Which agent (default claude)"},
			"mode": prop("string", "Permission mode (default auto). Claude: auto, default (ask first), acceptEdits, plan. Codex: auto, untrusted, read-only. "+
				"Runs start while the user may be away; a mode that asks first leaves the run waiting for them"),
			"model":    prop("string", `Model (default: the user's default for that agent at each run). An alias or model id the agent accepts; "default" is the agent's own default`),
			"effort":   prop("string", effortHelp),
			"worktree": prop("boolean", "Each run works on its own git branch and checkout"),
			"title":    prop("string", "Short name for the schedule and its sessions (default: from the prompt)"),
		}, "project", "prompt", "cron"),
	},
	{
		"name":        "list_schedules",
		"description": "List the user's schedules: id, project, when (cron and time zone), next run, whether paused, and the latest runs with their status and session id (read one with read_session).",
		"inputSchema": obj(map[string]any{}),
	},
	{
		"name":        "delete_schedule",
		"description": "Delete one of the user's schedules. The sessions it already started are kept.",
		"inputSchema": obj(map[string]any{"schedule": prop("string", "Schedule id (see list_schedules)")}, "schedule"),
	},
}

// callScheduleTool runs a schedule tool; handled is false for other tools.
func (m *Manager) callScheduleTool(a Actor, name string, raw json.RawMessage) (text string, handled bool, err error) {
	switch name {
	case "create_schedule":
		var p struct {
			Project  string `json:"project"`
			Prompt   string `json:"prompt"`
			Cron     string `json:"cron"`
			TZ       string `json:"timezone"`
			Agent    string `json:"agent"`
			Mode     string `json:"mode"`
			Model    string `json:"model"`
			Effort   string `json:"effort"`
			Worktree bool   `json:"worktree"`
			Title    string `json:"title"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return "", true, err
		}
		text, err = m.toolCreateSchedule(a, p.Project, p.Prompt, p.Cron, p.TZ, p.Agent, p.Mode, p.Model, p.Effort, p.Title, p.Worktree)
		return text, true, err
	case "list_schedules":
		text, err = m.toolListSchedules(a)
		return text, true, err
	case "delete_schedule":
		var p struct {
			Schedule string `json:"schedule"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return "", true, err
		}
		sc, err := m.ownSchedule(a.ID, p.Schedule)
		if err == nil {
			err = m.DeleteSchedule(a, sc.ID)
		}
		if errors.Is(err, ErrNotFound) {
			return "", true, fmt.Errorf("no schedule %q (see list_schedules)", p.Schedule)
		}
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("Deleted the schedule %q; the sessions it started are kept.", scheduleName(sc)), true, nil
	}
	return "", false, nil
}

// scheduleProject: like workProject, and "home" is the user's Home.
func (m *Manager) scheduleProject(uid, ref string) (model.Project, error) {
	p, err := m.workProject(uid, ref)
	if err != nil && strings.EqualFold(strings.TrimSpace(ref), "home") {
		if h, ok := m.homeOf(uid); ok {
			return h, nil
		}
	}
	return p, err
}

func (m *Manager) toolCreateSchedule(a Actor, project, prompt, cronExpr, tz, agentName, mode, mdl, effort, title string, worktree bool) (string, error) {
	p, err := m.scheduleProject(a.ID, project)
	if err != nil {
		return "", err
	}
	switch mode {
	case "bypassPermissions", "dontAsk", "full":
		return "", errors.New("the Home agent cannot schedule sessions without permission checks; the user can make such a schedule in Collie (Schedules)")
	}
	given := func(v string) *string { // omitted: the default
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return &v
	}
	req := ScheduleReq{ProjectID: &p.ID, Prompt: &prompt, Cron: &cronExpr, Worktree: &worktree,
		TZ: given(tz), Agent: given(agentName), Mode: given(mode), Model: given(mdl), Effort: given(effort), Title: given(title)}
	sc, err := m.CreateSchedule(a, req)
	if err != nil {
		return "", err
	}
	out := m.scheduleSummary(sc, 3)
	out["note"] = "Tell the user when it first runs. They can pause, change or run it now in Collie (Schedules)."
	return pretty(out), nil
}

func (m *Manager) toolListSchedules(a Actor) (string, error) {
	list := []map[string]any{}
	for _, sc := range m.Schedules(a.ID) {
		list = append(list, m.scheduleSummary(sc, 1))
	}
	if len(list) == 0 {
		return "The user has no schedules.", nil
	}
	return pretty(list), nil
}

func scheduleName(sc model.Schedule) string {
	if sc.Title != "" {
		return sc.Title
	}
	return deriveTitle(sc.Prompt)
}

// scheduleSummary describes a schedule for the Home agent, with times in
// the schedule's time zone.
func (m *Manager) scheduleSummary(sc model.Schedule, next int) map[string]any {
	loc, err := time.LoadLocation(sc.TZ)
	if err != nil {
		loc = time.UTC
	}
	when := func(ms int64) string { return time.UnixMilli(ms).In(loc).Format("Mon 2006-01-02 15:04 MST") }
	project := sc.ProjectID
	if p, ok := m.st.Project(sc.ProjectID); ok {
		project = p.Name
	}
	out := map[string]any{
		"id": sc.ID, "title": scheduleName(sc), "project": project, "agent": sc.Agent, "mode": sc.Mode,
		"cron": sc.Cron, "timezone": sc.TZ,
	}
	// What its runs start with now (an empty setting follows the user's defaults).
	mdl, effort := m.ResolveModelEffort(sc.Owner, sc.Agent, sc.Model, sc.Effort)
	out["model"], out["effort"] = orDefault(mdl), orDefault(effort)
	if sc.Model == "" && sc.Effort == "" {
		out["modelAndEffort"] = "the user's defaults"
	}
	if sc.Worktree {
		out["worktree"] = true
	}
	if sc.Paused {
		out["paused"] = true
	} else if times, err := ScheduleTimes(sc.Cron, sc.TZ, m.now(), next); err == nil {
		var s []string
		for _, t := range times {
			s = append(s, when(t.UnixMilli()))
		}
		out["nextRuns"] = s
	}
	var runs []map[string]any
	for _, r := range sc.Runs[:min(len(sc.Runs), 3)] {
		x := map[string]any{"at": when(r.At), "status": r.Status, "trigger": r.Trigger}
		if r.SessionID != "" {
			x["session"] = r.SessionID
		}
		if r.Error != "" {
			x["note"] = r.Error
		}
		if r.Count > 1 {
			x["times"] = r.Count
		}
		runs = append(runs, x)
	}
	if len(runs) > 0 {
		out["latestRuns"] = runs
	}
	return out
}
