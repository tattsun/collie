// Package model holds the data types shared between the store, the agent
// drivers, the session runtime and the HTTP layer.
package model

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	// Runtime-only fields.
	Git    bool   `json:"git"`
	Status string `json:"status,omitempty"` // "", "cloning", "error"
	Error  string `json:"error,omitempty"`
}

// Isolated reports whether the project's agents run in a container.
func (p *Project) Isolated() bool { return p.Runtime == "container" }

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
	Images []string   `json:"images,omitempty"`
	By     string     `json:"by,omitempty"` // user who sent a user message
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
	return &c
}
