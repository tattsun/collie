package session

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"

	"github.com/tattsun/collie/internal/model"
)

// Plan usage per user and agent. Agents run with their session owner's
// accounts, so what a session reports belongs to its owner, and is only
// shown to them. It is kept in the user's data directory so it survives
// restarts (and goes away with the user).

// usagePath is "" without a data directory for users (then it is only
// kept in memory).
func (m *Manager) usagePath(uid string) string {
	if m.sb == nil {
		return ""
	}
	return filepath.Join(m.sb.UserDir(uid), "usage.json")
}

// usageLocked returns the user's usage by agent, loading it on first use.
// m.umu must be held.
func (m *Manager) usageLocked(uid string) map[string]model.Usage {
	u, ok := m.usage[uid]
	if ok {
		return u
	}
	u = map[string]model.Usage{}
	if p := m.usagePath(uid); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &u)
		}
	}
	m.usage[uid] = u
	return u
}

// UsageFor is the user's plan usage by agent ("claude", "codex").
func (m *Manager) UsageFor(uid string) map[string]model.Usage {
	m.umu.Lock()
	defer m.umu.Unlock()
	out := map[string]model.Usage{}
	for k, v := range m.usageLocked(uid) {
		out[k] = v
	}
	return out
}

func (m *Manager) reportUsage(uid, agentName string, upd model.Usage) {
	if uid == "" || agentName == "" {
		return
	}
	m.umu.Lock()
	all := m.usageLocked(uid)
	cur := model.MergeUsage(all[agentName], upd)
	all[agentName] = cur
	var err error
	if p := m.usagePath(uid); p != "" {
		var b []byte
		if b, err = json.Marshal(all); err == nil {
			if err = os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
				err = os.WriteFile(p, b, 0o600)
			}
		}
	}
	m.umu.Unlock()
	if err != nil {
		log.Printf("save usage of %s: %v", uid, err)
	}
	m.hub.BroadcastWhere(map[string]any{"t": "usage", "agent": agentName, "usage": cur}, func(id string) bool { return id == uid })
}

func (m *Manager) forgetUsage(uid string) {
	m.umu.Lock()
	delete(m.usage, uid)
	m.umu.Unlock()
}
