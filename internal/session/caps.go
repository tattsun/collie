package session

import (
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/tattsun/collie/internal/agent"
	"github.com/tattsun/collie/internal/model"
)

// What agents offer, by user and agent: the models and slash commands
// their agents last reported. Like plan usage, they belong to the user
// whose agent reported them and are only shown to them. A list names the
// user's own skills and commands, and what an agent prints is not
// trusted: another user's agent must never put words into someone's model
// picker or their Home's tool descriptions.
//
// Before a user's own agents have reported, they see what Collie's own
// probe found (the Codex models, asked by Collie itself, not by an agent).

const (
	maxCapModels   = 64
	maxCapCommands = 300
)

var (
	// Model ids are names, never sentences (they also go into Home's tool
	// descriptions).
	capModelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/+\[\]-]{0,99}$`)
	// Commands are the user's own (~/.claude/commands, skills), in any
	// script and with spaces; they never reach Home's tool descriptions.
	capCommandRe = regexp.MustCompile(`^[\p{L}\p{N}_][\p{L}\p{N}\p{M} ._:/+-]{0,99}$`)
)

// capText is one line of display text from an agent, of at most n runes.
func capText(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	return clip(strings.Join(strings.Fields(s), " "), n)
}

// cleanCaps keeps what an agent reported only as names and short labels.
// Nil lists stay nil (not reported).
func cleanCaps(c model.Caps) model.Caps {
	var out model.Caps
	if c.Models != nil {
		out.Models = []model.ModelOpt{}
		for _, mo := range c.Models {
			if len(out.Models) >= maxCapModels {
				break
			}
			if !capModelRe.MatchString(mo.ID) {
				continue
			}
			out.Models = append(out.Models, model.ModelOpt{ID: mo.ID, Label: capText(mo.Label, 80), Description: capText(mo.Description, 300)})
		}
	}
	if c.Commands != nil {
		out.Commands = []model.Command{}
		for _, cmd := range c.Commands {
			if len(out.Commands) >= maxCapCommands {
				break
			}
			if !capCommandRe.MatchString(cmd.Name) {
				continue
			}
			out.Commands = append(out.Commands, model.Command{Name: cmd.Name, Description: capText(cmd.Description, 300), Hint: capText(cmd.Hint, 100)})
		}
	}
	return out
}

// capsPath is "" without a data directory for users (then they are only
// kept in memory).
func (m *Manager) capsPath(uid string) string {
	if m.sb == nil {
		return ""
	}
	return filepath.Join(m.sb.UserDir(uid), "caps.json")
}

func (m *Manager) probedPath() string { return filepath.Join(m.st.Dir(), "probed-caps.json") }

// loadCaps reads what Collie's probe found. The one list for everyone that
// earlier versions kept (caps.json) came from agents: it goes. Only its
// Codex model ids (cleaned) stay, as the probe's answer until Collie probes
// itself, which needs Codex on the server (the image has its own).
func (m *Manager) loadCaps() {
	legacy := filepath.Join(m.st.Dir(), "caps.json")
	var old map[string]model.Caps
	if b, err := os.ReadFile(legacy); err == nil {
		_ = json.Unmarshal(b, &old)
	}
	_ = os.Remove(legacy)
	defer func() {
		if cx := cleanCaps(model.Caps{Models: old["codex"].Models}); len(cx.Models) > 0 {
			m.cmu.Lock()
			if _, have := m.probed["codex"]; !have {
				if m.probed == nil {
					m.probed = map[string]model.Caps{}
				}
				m.probed["codex"] = cx
				if b, err := json.Marshal(m.probed); err == nil {
					_ = os.WriteFile(m.probedPath(), b, 0o600)
				}
			}
			m.cmu.Unlock()
		}
	}()
	if b, err := os.ReadFile(m.probedPath()); err == nil {
		var p map[string]model.Caps
		if json.Unmarshal(b, &p) == nil {
			m.cmu.Lock()
			for k, v := range p {
				if m.probed == nil {
					m.probed = map[string]model.Caps{}
				}
				m.probed[k] = cleanCaps(v)
			}
			m.cmu.Unlock()
		}
	}
}

// capsLocked returns the user's caps by agent, loading them on first use.
// m.cmu must be held.
func (m *Manager) capsLocked(uid string) map[string]model.Caps {
	c, ok := m.caps[uid]
	if ok {
		return c
	}
	c = map[string]model.Caps{}
	if m.caps == nil {
		m.caps = map[string]map[string]model.Caps{}
	}
	if p := m.capsPath(uid); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			var saved map[string]model.Caps
			if json.Unmarshal(b, &saved) == nil {
				for k, v := range saved {
					c[k] = cleanCaps(v)
				}
			}
		}
	}
	m.caps[uid] = c
	return c
}

// AgentCapsFor is what the user's agents offer, by agent ("claude",
// "codex"), with Collie's probe filling in what they have not reported.
func (m *Manager) AgentCapsFor(uid string) map[string]model.Caps {
	m.cmu.Lock()
	defer m.cmu.Unlock()
	out := map[string]model.Caps{}
	for k, v := range m.probed {
		out[k] = v
	}
	if uid == "" {
		return out
	}
	for k, v := range m.capsLocked(uid) {
		cur := out[k]
		if len(v.Models) > 0 {
			cur.Models = v.Models
		}
		if v.Commands != nil {
			cur.Commands = v.Commands
		}
		out[k] = cur
	}
	return out
}

// reportCaps notes what one of the user's agents offers and tells the
// user's clients.
func (m *Manager) reportCaps(uid, agentName string, c model.Caps) {
	if uid == "" || agentName == "" {
		return
	}
	c = cleanCaps(c)
	m.cmu.Lock()
	all := m.capsLocked(uid)
	cur := all[agentName]
	if c.Commands != nil {
		cur.Commands = c.Commands
	}
	if c.Models != nil {
		cur.Models = c.Models
	}
	all[agentName] = cur
	var err error
	if p := m.capsPath(uid); p != "" {
		var b []byte
		if b, err = json.Marshal(all); err == nil {
			if err = os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
				err = os.WriteFile(p, b, 0o600)
			}
		}
	}
	m.cmu.Unlock()
	if err != nil {
		log.Printf("save caps of %s: %v", uid, err)
	}
	shown := m.AgentCapsFor(uid)[agentName]
	m.hub.BroadcastWhere(map[string]any{"t": "agentcaps", "agent": agentName, "caps": shown}, func(id string) bool { return id == uid })
}

func (m *Manager) forgetCaps(uid string) {
	m.cmu.Lock()
	delete(m.caps, uid)
	m.cmu.Unlock()
}

// probeCaps fills in the Codex model list once, so the new-session form
// can offer models before the user's first Codex session. (Claude's list
// arrives with each user's first Claude session; until then the UI offers
// the usual aliases.)
func (m *Manager) probeCaps() {
	m.cmu.Lock()
	_, have := m.probed["codex"]
	m.cmu.Unlock()
	if have {
		return
	}
	if _, err := exec.LookPath(m.cfg.CodexBin); err != nil {
		return
	}
	home, _ := os.UserHomeDir()
	caps, err := agent.ProbeCodex(m.cfg.CodexBin, home)
	if err != nil {
		log.Printf("codex model probe: %v", err)
		return
	}
	caps = cleanCaps(model.Caps{Models: caps.Models})
	if len(caps.Models) == 0 {
		return
	}
	m.cmu.Lock()
	if m.probed == nil {
		m.probed = map[string]model.Caps{}
	}
	m.probed["codex"] = caps
	b, err := json.Marshal(m.probed)
	m.cmu.Unlock()
	if err == nil {
		_ = os.WriteFile(m.probedPath(), b, 0o600)
	}
	// Everyone who has no list of their own yet sees it now.
	m.hub.each(func(c *Client) {
		c.enqueue(mustJSON(map[string]any{"t": "agentcaps", "agent": "codex", "caps": m.AgentCapsFor(c.UserID)["codex"]}))
	})
}
