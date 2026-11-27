// Package model holds the data types shared between the store, the agent
// drivers, the session runtime and the HTTP layer.
package model

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

type Project struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	CreatedAt int64  `json:"createdAt"`
	// Owner: exactly one of OwnerUser / OwnerGroup is set.
	OwnerUser  string `json:"ownerUser,omitempty"`
	OwnerGroup string `json:"ownerGroup,omitempty"`
	CreatedBy  string `json:"createdBy,omitempty"`
	// Runtime is where agents run: "container" (isolated, one rootless
	// Podman container per project) or "host" ("" = host, for projects
	// created before isolation existed).
	Runtime string `json:"runtime,omitempty"`
	Memory  string `json:"memory,omitempty"` // container limits; "" = server default
	CPUs    string `json:"cpus,omitempty"`
	Pids    int    `json:"pids,omitempty"`
	// Nested lets the project's containers run Podman inside (see
	// sandbox/nested.go); an admin decision, as it widens what agents reach.
	Nested bool `json:"nested,omitempty"`
	// Network is what the project's containers can connect to: "" the
	// internet (as far as the server's firewall allows), NetAllowlist only
	// the hosts in NetAllow and the agents' own services (sandbox/network.go).
	Network  string   `json:"network,omitempty"`
	NetAllow []string `json:"netAllow,omitempty"`
	// Home marks a user's Home: a workspace outside any project where the
	// Home agent chats with the user and coordinates their project agents.
	Home bool `json:"home,omitempty"`
	// Runtime-only fields.
	Git    bool   `json:"git"`
	Status string `json:"status,omitempty"` // "", "cloning", "error"
	Error  string `json:"error,omitempty"`
}

// Isolated reports whether the project's agents run in a container.
func (p *Project) Isolated() bool { return p.Runtime == "container" }

// NetAllowlist is Project.Network for a project whose containers reach only
// the hosts on its list.
const NetAllowlist = "allowlist"

// Allowlisted reports whether the project's containers reach only the
// hosts on its allowlist.
func (p *Project) Allowlisted() bool { return p.Isolated() && p.Network == NetAllowlist }

// Session statuses.
const (
	StatusStarting = "starting"
	StatusRunning  = "running"
	StatusWaiting  = "waiting" // blocked on an approval / question
	StatusIdle     = "idle"    // process alive, turn finished
	StatusStopped  = "stopped" // no process; resumes on next message
	StatusError    = "error"
)

type Session struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Agent     string `json:"agent"` // "claude" | "codex"
	Title     string `json:"title"`
	Model     string `json:"model,omitempty"`
	Effort    string `json:"effort,omitempty"`
	Mode      string `json:"mode"`
	Cwd       string `json:"cwd"`
	Worktree  bool   `json:"worktree,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Base      string `json:"base,omitempty"` // commit the worktree branch was created from
	// AgentRef is the agent-native conversation id (Claude session uuid or
	// Codex thread id) used to resume after the process is restarted.
	AgentRef  string  `json:"agentRef,omitempty"`
	Started   bool    `json:"started,omitempty"` // agent has persisted the conversation at least once
	Status    string  `json:"status"`
	Unread    bool    `json:"unread,omitempty"`
	Preview   string  `json:"preview,omitempty"`
	CostUSD   float64 `json:"costUsd,omitempty"`
	CtxUsed   int64   `json:"ctxUsed,omitempty"`
	CtxMax    int64   `json:"ctxMax,omitempty"`
	Archived  bool    `json:"archived,omitempty"`
	CreatedAt int64   `json:"createdAt"`
	UpdatedAt int64   `json:"updatedAt"`
	// Owner started the session; its agent runs with the owner's agent
	// accounts. In group projects a Shared session is visible to, and can
	// be driven by, every member of the group.
	Owner  string `json:"owner,omitempty"`
	Shared bool   `json:"shared,omitempty"`
	// Tasks is the work the agent has running in the background (commands,
	// sub-agents...), which goes on after its turn has ended. It lives only
	// as long as the agent process.
	Tasks []Task `json:"tasks,omitempty"`
}

// Task is one piece of background work. Drivers replace Session.Tasks as a
// whole, never edit it in place: copies of a Session share the slice.
type Task struct {
	ID      string `json:"id"`
	Kind    string `json:"kind,omitempty"` // the agent's task type: local_bash, local_agent...
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"` // latest progress, if the agent reports any
	ToolID  string `json:"toolId,omitempty"`  // the tool call that started it (a timeline item)
	Started int64  `json:"started"`
}

// Item kinds.
const (
	KindUser     = "user"
	KindText     = "text"
	KindThinking = "thinking"
	KindTool     = "tool"
	KindApproval = "approval"
	KindQuestion = "question"
	KindPlan     = "plan"
	KindNotice   = "notice"
	KindError    = "error"
	KindResult   = "result"
	KindMedia    = "media" // files the agent showed the user (show_media)
)

// Item is one entry of the normalized conversation timeline.
type Item struct {
	ID     string     `json:"id"`
	Kind   string     `json:"kind"`
	TS     int64      `json:"ts"`
	Text   string     `json:"text,omitempty"`
	Parent string     `json:"parent,omitempty"` // owning tool call (sub-agents)
	Done   bool       `json:"done,omitempty"`
	Tool   *Tool      `json:"tool,omitempty"`
	Ask    *Ask       `json:"ask,omitempty"`
	Plan   []PlanStep `json:"plan,omitempty"`
	Result *Result    `json:"result,omitempty"`
	// Images are upload names: attached to a user message, or seen by the
	// agent in a tool's result (screenshots, images it read).
	Images []string `json:"images,omitempty"`
	Media  []Media  `json:"media,omitempty"`
	By     string   `json:"by,omitempty"`  // user who sent a user message
	Via    string   `json:"via,omitempty"` // "home": sent for the user by their Home agent
}

// Media is one file the agent showed the user, copied into the uploads
// when it was shown (later changes to the file do not affect it).
type Media struct {
	ID      string `json:"id"` // upload name (/api/uploads/<id>)
	Mime    string `json:"mime"`
	Caption string `json:"caption,omitempty"`
	Name    string `json:"name,omitempty"` // file name it was shown from
}

type Tool struct {
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
	Diff   string          `json:"diff,omitempty"`
	Status string          `json:"status"` // running | ok | error | denied
}

type Ask struct {
	Tool      string          `json:"tool,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Title     string          `json:"title,omitempty"`
	Body      string          `json:"body,omitempty"`
	BodyKind  string          `json:"bodyKind,omitempty"` // md | code | diff
	Options   []Option        `json:"options,omitempty"`
	Questions []Question      `json:"questions,omitempty"`
	State     string          `json:"state"` // pending | answered | cancelled
	Answer    string          `json:"answer,omitempty"`
}

type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Style string `json:"style,omitempty"` // primary | danger
}

type Question struct {
	ID       string    `json:"id"`
	Header   string    `json:"header,omitempty"`
	Question string    `json:"question"`
	Options  []QOption `json:"options,omitempty"`
	Multi    bool      `json:"multi,omitempty"`
	Other    bool      `json:"other,omitempty"`
	Secret   bool      `json:"secret,omitempty"`
}

type QOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type PlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"` // pending | inProgress | completed
}

type Result struct {
	DurationMs int64   `json:"durationMs,omitempty"`
	CostUSD    float64 `json:"costUsd,omitempty"`
	Turns      int     `json:"turns,omitempty"`
	IsError    bool    `json:"isError,omitempty"`
	Status     string  `json:"status,omitempty"`
}

// Usage is how much of an agent account's plan allowance is used, per
// rate-limit window, as last reported by one of the account's sessions.
type Usage struct {
	Windows []UsageWindow `json:"windows"`
	// Status is Claude's overall state: allowed | allowed_warning | rejected.
	Status    string `json:"status,omitempty"`
	Plan      string `json:"plan,omitempty"`
	UpdatedAt int64  `json:"updatedAt"`
}

type UsageWindow struct {
	// ID names the window: five_hour, seven_day, seven_day_opus… (Claude);
	// primary, secondary (Codex; "<limit id>:primary" for other buckets).
	ID       string  `json:"id"`
	Label    string  `json:"label,omitempty"`    // server-supplied name, when there is one
	Pct      float64 `json:"pct"`                // share used, 0-100 (can run past 100)
	ResetsAt int64   `json:"resetsAt,omitempty"` // unix ms
	Mins     int64   `json:"mins,omitempty"`     // window length, when known
	Status   string  `json:"status,omitempty"`
}

// MergeUsage lays an update over the last known usage: windows it reports
// replace the ones with the same id, the others are kept (updates can be
// sparse, and a fresh agent process knows nothing until its first reply).
func MergeUsage(old, upd Usage) Usage {
	out := Usage{Status: old.Status, Plan: old.Plan, UpdatedAt: upd.UpdatedAt}
	if upd.Status != "" {
		out.Status = upd.Status
	}
	if upd.Plan != "" {
		out.Plan = upd.Plan
	}
	seen := map[string]bool{}
	for _, w := range upd.Windows {
		seen[w.ID] = true
	}
	for _, w := range old.Windows {
		if !seen[w.ID] {
			out.Windows = append(out.Windows, w)
		}
	}
	out.Windows = append(out.Windows, upd.Windows...)
	sort.SliceStable(out.Windows, func(i, j int) bool { return out.Windows[i].ID < out.Windows[j].ID })
	return out
}

// Caps describes what an agent offers (reported once it has started).
type Caps struct {
	Commands []Command  `json:"commands,omitempty"`
	Models   []ModelOpt `json:"models,omitempty"`
}

type ModelOpt struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type Command struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Hint        string `json:"hint,omitempty"`
}

func NowMs() int64 { return time.Now().UnixMilli() }

// NewID returns a short random hex id.
func NewID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// NewUUID returns a random RFC 4122 v4 UUID.
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// Clone returns a deep-enough copy of the item so that it can be handed to
// another goroutine while the original keeps being mutated.
func (it *Item) Clone() *Item {
	c := *it
	if it.Tool != nil {
		t := *it.Tool
		c.Tool = &t
	}
	if it.Ask != nil {
		a := *it.Ask
		c.Ask = &a
	}
	if it.Result != nil {
		r := *it.Result
		c.Result = &r
	}
	if it.Plan != nil {
		c.Plan = append([]PlanStep(nil), it.Plan...)
	}
	if it.Images != nil {
		c.Images = append([]string(nil), it.Images...)
	}
	if it.Media != nil {
		c.Media = append([]Media(nil), it.Media...)
	}
	return &c
}
