package session

import (
	"errors"

	"github.com/tattsun/collie/internal/model"
)

// ClaudeLoginShell prepares a process in the user's Home container for the
// guided Claude sign-in: the argv to run (with a pty) and a function that
// signals it. Variables that would change how `claude setup-token` behaves
// (an existing token, a Bedrock switch) are blanked.
func (m *Manager) ClaudeLoginShell(a Actor, id string, command []string) (model.Project, []string, func(string), error) {
	if m.sb == nil || !m.sb.Available() {
		return model.Project{}, nil, nil, errors.New("signing in from here needs podman on the server — run `claude setup-token` yourself and paste the token")
	}
	home, err := m.EnsureHome(a)
	if err != nil {
		return home, nil, nil, err
	}
	if err := m.EnsureContainer(home, a.ID); err != nil {
		return home, nil, nil, err
	}
	env := []string{"CLAUDE_CODE_OAUTH_TOKEN=", "ANTHROPIC_API_KEY=", "CLAUDE_CODE_USE_BEDROCK=", "BROWSER=true"}
	argv, kill := m.sb.Exec(home.ID, a.ID, home.Path, id, true, env, command)
	return home, argv, kill, nil
}

// SetClaudeProjectAccount picks the user's Claude account for a project
// they can see.
func (m *Manager) SetClaudeProjectAccount(uid, projectID, accountID string) error {
	if m.sb == nil {
		return errors.New("isolation is not available")
	}
	p, ok := m.st.Project(projectID)
	if !ok || !m.ProjectVisible(uid, p) {
		return ErrNotFound
	}
	return m.sb.SetProjectAccount(uid, projectID, accountID)
}
