package session

import (
	"errors"
	"log"

	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
)

// Claude Code plugins (see sandbox/plugins.go for the levels). Only
// isolated projects get them: on the host, Claude Code uses the server
// user's own ~/.claude, which Collie leaves alone.

var errNoIsolation = errors.New("isolation is not available")

// syncPlugins installs the plugins the user's agents in p should have
// (the container must be up); progress and problems go to notify.
func (m *Manager) syncPlugins(p model.Project, uid, cwd string, notify func(string)) {
	if m.sb == nil || !p.Isolated() {
		return
	}
	levels, errs := m.sb.PluginLevels(p, uid, cwd)
	for _, err := range errs {
		log.Printf("project %s, user %s: %v", p.ID, uid, err)
	}
	if err := m.sb.SyncPlugins(p, uid, levels, notify); err != nil {
		log.Printf("project %s, user %s: plugins: %v", p.ID, uid, err)
	}
}

// SyncPluginsAsync is syncPlugins for a terminal: Claude Code started there
// gets the plugins too.
func (m *Manager) SyncPluginsAsync(p model.Project, uid, cwd string) {
	go m.syncPlugins(p, uid, cwd, nil)
}

// ProjectPlugins returns the project's own plugin definition and whether a
// may change it (the project's managers may).
func (m *Manager) ProjectPlugins(a Actor, id string) (sandbox.PluginSet, bool, error) {
	p, err := m.visibleProject(a.ID, id)
	if err != nil {
		return sandbox.PluginSet{}, false, err
	}
	if m.sb == nil {
		return sandbox.PluginSet{}, false, errNoIsolation
	}
	s, err := sandbox.ReadPluginSet(m.sb.ProjectPluginsPath(p.ID))
	return s, m.ProjectManageable(a.ID, p), err
}

func (m *Manager) SetProjectPlugins(a Actor, id string, s sandbox.PluginSet) error {
	p, err := m.visibleProject(a.ID, id)
	if err != nil {
		return err
	}
	if m.sb == nil {
		return errNoIsolation
	}
	if !m.ProjectManageable(a.ID, p) {
		return ErrForbidden
	}
	return sandbox.WritePluginSet(m.sb.ProjectPluginsPath(p.ID), s)
}

// GroupProject reports whether a sees the project and it is a group's
// (its plugins then run in every member's container).
func (m *Manager) GroupProject(a Actor, id string) bool {
	p, err := m.visibleProject(a.ID, id)
	return err == nil && p.OwnerGroup != ""
}

// EffectivePlugins is what a's agents in the project get, by plugin, with
// the level that decided; Problems lists skipped entries and unreadable
// files.
type EffectivePlugins struct {
	Plugins  []sandbox.EffectivePlugin `json:"plugins"`
	Problems []string                  `json:"problems"`
}

func (m *Manager) ProjectEffectivePlugins(a Actor, id string) (EffectivePlugins, error) {
	out := EffectivePlugins{Plugins: []sandbox.EffectivePlugin{}, Problems: []string{}}
	p, err := m.visibleProject(a.ID, id)
	if err != nil {
		return out, err
	}
	if m.sb == nil {
		return out, errNoIsolation
	}
	levels, errs := m.sb.PluginLevels(p, a.ID, p.Path)
	out.Plugins, _ = sandbox.MergePlugins(levels)
	for _, e := range errs {
		out.Problems = append(out.Problems, e.Error())
	}
	return out, nil
}
