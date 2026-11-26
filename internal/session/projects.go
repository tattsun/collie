package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/tattsun/collie/internal/gitutil"
	"github.com/tattsun/collie/internal/model"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

type CreateProjectReq struct {
	Kind string `json:"kind"` // empty | clone | existing
	Name string `json:"name"`
	URL  string `json:"url"`
	Path string `json:"path"`
	Git  *bool  `json:"git"` // for "empty": run git init (default true)
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

func (m *Manager) broadcastProject(p model.Project) {
	m.hub.Broadcast(map[string]any{"t": "project", "project": p})
}

func (m *Manager) expand(p string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "~/") || p == "~" {
		home, _ := os.UserHomeDir()
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(m.cfg.ProjectRoot, p)
	}
	return filepath.Clean(p)
}

func (m *Manager) CreateProject(req CreateProjectReq) (model.Project, error) {
	name := strings.TrimSpace(req.Name)
	var path string
	switch req.Kind {
	case "existing":
		if strings.TrimSpace(req.Path) == "" {
			return model.Project{}, errors.New("path is required")
		}
		path = m.expand(req.Path)
		st, err := os.Stat(path)
		if err != nil || !st.IsDir() {
			return model.Project{}, fmt.Errorf("%s is not a directory", path)
		}
		if name == "" {
			name = filepath.Base(path)
		}
	case "clone":
		url := strings.TrimSpace(req.URL)
		if url == "" {
			return model.Project{}, errors.New("repository URL is required")
		}
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(strings.TrimRight(url, "/")), ".git")
			if i := strings.LastIndex(name, ":"); i >= 0 {
				name = name[i+1:]
			}
		}
		if !nameRe.MatchString(name) {
			return model.Project{}, errors.New("invalid project name (letters, digits, . _ -)")
		}
		path = filepath.Join(m.cfg.ProjectRoot, name)
		if _, err := os.Stat(path); err == nil {
			return model.Project{}, fmt.Errorf("%s already exists", path)
		}
	default:
		if !nameRe.MatchString(name) {
			return model.Project{}, errors.New("invalid project name (letters, digits, . _ -)")
		}
		path = filepath.Join(m.cfg.ProjectRoot, name)
		if _, err := os.Stat(path); err == nil {
			return model.Project{}, fmt.Errorf("%s already exists — use \"Open existing folder\"", path)
		}
	}
	for _, p := range m.st.Projects() {
		if p.Path == path {
			return model.Project{}, fmt.Errorf("%s is already registered as %q", path, p.Name)
		}
	}
	if err := os.MkdirAll(m.cfg.ProjectRoot, 0o755); err != nil {
		return model.Project{}, err
	}
	p := model.Project{ID: model.NewID(), Name: name, Path: path, CreatedAt: model.NowMs()}
	switch req.Kind {
	case "existing":
		p.Git = gitutil.IsRepo(path)
	case "clone":
		p.Status = "cloning"
		url := strings.TrimSpace(req.URL)
		go func() {
			err := gitutil.Clone(url, path)
			np, ok := m.st.UpdateProject(p.ID, func(x *model.Project) {
				if err != nil {
					x.Status, x.Error = "error", err.Error()
				} else {
					x.Status, x.Error, x.Git = "", "", true
				}
			})
			if ok {
				m.broadcastProject(np)
			}
		}()
	default:
		if err := os.MkdirAll(path, 0o755); err != nil {
			return model.Project{}, err
		}
		if req.Git == nil || *req.Git {
			if err := gitutil.Init(path); err != nil {
				return model.Project{}, err
			}
			p.Git = true
		}
	}
	m.st.PutProject(p)
	m.broadcastProject(p)
	return p, nil
}

// DeleteProject unregisters the project and deletes its sessions. Files on
// disk are left untouched (except collie-created worktrees).
func (m *Manager) DeleteProject(id string) error {
	if _, ok := m.st.Project(id); !ok {
		return errors.New("project not found")
	}
	for _, s := range m.st.Sessions() {
		if s.ProjectID == id {
			_ = m.DeleteSession(s.ID, true)
		}
	}
	m.st.DeleteProject(id)
	m.hub.Broadcast(map[string]any{"t": "project_del", "id": id})
	return nil
}

func (m *Manager) ProjectChanges(id string) (*gitutil.Changes, error) {
	p, ok := m.st.Project(id)
	if !ok {
		return nil, errors.New("project not found")
	}
	ch, err := gitutil.Diff(p.Path, "")
	if err == nil && ch.Repo != p.Git {
		np, _ := m.st.UpdateProject(id, func(x *model.Project) { x.Git = ch.Repo })
		m.broadcastProject(np)
	}
	return ch, err
}

// SuggestDirs lists directories under the project root that are not yet
// registered as projects.
func (m *Manager) SuggestDirs() []string {
	entries, err := os.ReadDir(m.cfg.ProjectRoot)
	if err != nil {
		return []string{}
	}
	known := map[string]bool{}
	for _, p := range m.st.Projects() {
		known[p.Path] = true
	}
	out := []string{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		full := filepath.Join(m.cfg.ProjectRoot, e.Name())
		if !known[full] {
			out = append(out, full)
		}
	}
	sort.Strings(out)
	return out
}
