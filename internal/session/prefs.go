package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/tattsun/collie/internal/agent"
)

// Session preferences: what the sessions a user starts get unless they
// choose otherwise, i.e. a model and reasoning effort per agent, and
// instructions for every agent. They belong to that user alone: kept in
// their data directory, shown only to them, and given only to the agents of
// the sessions they started (in a shared group session too, whoever else
// drives it: its agent runs for the member who started it).

// AgentDefaults is the model and reasoning effort a user's new sessions of
// one agent start with ("" = the agent's own default).
type AgentDefaults struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// SessionPrefs are a user's preferences for the sessions they start.
type SessionPrefs struct {
	// Agents holds the defaults by agent ("claude", "codex").
	Agents map[string]AgentDefaults `json:"agents"`
	// Instructions go to every agent the user starts, added to its system
	// prompt (agent.Config.Instructions).
	Instructions string `json:"instructions,omitempty"`
}

// SessionPrefsPatch changes the defaults of the agents it lists, and the
// instructions when given.
type SessionPrefsPatch struct {
	Agents       map[string]AgentDefaults `json:"agents"`
	Instructions *string                  `json:"instructions"`
}

// maxInstructions keeps the instructions well within what one command-line
// argument may hold.
const maxInstructions = 32 << 10

// prefsMu serializes changes to the preference files.
var prefsMu sync.Mutex

func (m *Manager) prefsPath(uid string) string {
	if uid == "" || uid == "." || uid == ".." || strings.ContainsAny(uid, `/\`) {
		return ""
	}
	return filepath.Join(m.st.Dir(), "users", uid, "session-prefs.json")
}

// SessionPrefs returns the user's preferences.
func (m *Manager) SessionPrefs(uid string) SessionPrefs {
	p := SessionPrefs{}
	if path := m.prefsPath(uid); path != "" {
		b, err := os.ReadFile(path)
		if err == nil {
			err = json.Unmarshal(b, &p)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("session preferences of %s: %v", uid, err)
		}
	}
	if p.Agents == nil {
		p.Agents = map[string]AgentDefaults{}
	}
	return p
}

// PatchSessionPrefs changes the user's preferences and returns them.
func (m *Manager) PatchSessionPrefs(uid string, req SessionPrefsPatch) (SessionPrefs, error) {
	path := m.prefsPath(uid)
	if path == "" {
		return SessionPrefs{}, ErrNotFound
	}
	prefsMu.Lock()
	defer prefsMu.Unlock()
	p := m.SessionPrefs(uid)
	for name, d := range req.Agents {
		if _, ok := agent.Modes[name]; !ok {
			return SessionPrefs{}, fmt.Errorf("unknown agent %q", name)
		}
		d.Model, d.Effort = pickSetting(d.Model, ""), pickSetting(d.Effort, "")
		if err := checkEffort(name, d.Effort); err != nil {
			return SessionPrefs{}, err
		}
		if d == (AgentDefaults{}) {
			delete(p.Agents, name)
		} else {
			p.Agents[name] = d
		}
	}
	if req.Instructions != nil {
		text := strings.TrimSpace(strings.ReplaceAll(*req.Instructions, "\r\n", "\n"))
		if len(text) > maxInstructions {
			return SessionPrefs{}, fmt.Errorf("the instructions are too long (at most %d KB)", maxInstructions>>10)
		}
		if strings.ContainsRune(text, 0) {
			return SessionPrefs{}, errors.New("the instructions contain a NUL character")
		}
		p.Instructions = text
	}
	b, err := json.Marshal(p)
	if err != nil {
		return SessionPrefs{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return SessionPrefs{}, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return SessionPrefs{}, err
	}
	m.hub.BroadcastWhere(map[string]any{"t": "sessionPrefs", "prefs": p}, func(id string) bool { return id == uid })
	return p, nil
}

// ResolveModelEffort is the model and reasoning effort a new session of
// agentName starts with for user uid: the ones asked for, else the user's
// defaults for that agent ("default" asks for the agent's own). Everything
// that starts sessions for a user without the new-session form (the Home
// agent, schedules) goes through here, so they all follow the same defaults.
func (m *Manager) ResolveModelEffort(uid, agentName, modelID, effort string) (string, string) {
	d := m.SessionPrefs(uid).Agents[agentName]
	return pickSetting(modelID, d.Model), pickSetting(effort, d.Effort)
}

// pickSetting is the value asked for, or def when none is; "default" is
// the agent's own default ("").
func pickSetting(asked, def string) string {
	switch v := strings.TrimSpace(asked); v {
	case "":
		return def
	case "default":
		return ""
	default:
		return v
	}
}

// checkEffort refuses a reasoning effort the agent does not take.
func checkEffort(agentName, effort string) error {
	list := agent.Efforts[agentName]
	if effort == "" || slices.Contains(list, effort) {
		return nil
	}
	return fmt.Errorf("unknown effort %q for %s (one of: %s)", effort, agentName, strings.Join(list, ", "))
}
