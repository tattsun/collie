package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/tattsun/collie/internal/gitutil"
	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

type CreateProjectReq struct {
	Kind    string `json:"kind"` // empty | clone | existing
	Name    string `json:"name"`
	URL     string `json:"url"`
	Path    string `json:"path"`
	Git     *bool  `json:"git"`     // for "empty": run git init (default true)
	Runtime string `json:"runtime"` // container | host ("" = container when available)
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
	switch req.Runtime {
	case "host":
		p.Runtime = "host"
	case "container":
		if m.sb == nil || !m.sb.Available() {
			return model.Project{}, errors.New("isolated projects need podman on the server")
		}
		p.Runtime = "container"
	case "":
		p.Runtime = "host"
		if m.sb != nil && m.sb.Available() {
			p.Runtime = "container"
		}
	default:
		return model.Project{}, fmt.Errorf("unknown runtime %q", req.Runtime)
	}
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
	_ = m.StopProject(id, true)
	for _, s := range m.st.Sessions() {
		if s.ProjectID == id {
			_ = m.DeleteSession(s.ID, true)
		}
	}
	if m.sb != nil {
		m.sb.Remove(id)
		_ = os.Remove(m.sb.WorktreeRoot(id)) // only if empty
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

type PatchProjectReq struct {
	Name    *string `json:"name"`
	Runtime *string `json:"runtime"`
	Memory  *string `json:"memory"`
	CPUs    *string `json:"cpus"`
	Pids    *int    `json:"pids"`
}

var limitRe = regexp.MustCompile(`^(|[0-9]+(\.[0-9]+)?[kmgKMG]?)$`)

// PatchProject changes a project's settings. Changing where or how its
// agents run stops them; they resume in the new environment on the next
// message.
func (m *Manager) PatchProject(id string, req PatchProjectReq) (model.Project, error) {
	p, ok := m.st.Project(id)
	if !ok {
		return p, errors.New("project not found")
	}
	if req.Runtime != nil && *req.Runtime != "host" && *req.Runtime != "container" {
		return p, fmt.Errorf("unknown runtime %q", *req.Runtime)
	}
	if req.Runtime != nil && *req.Runtime == "container" && (m.sb == nil || !m.sb.Available()) {
		return p, errors.New("isolated projects need podman on the server")
	}
	for _, v := range []*string{req.Memory, req.CPUs} {
		if v != nil && !limitRe.MatchString(strings.TrimSpace(*v)) {
			return p, fmt.Errorf("invalid limit %q", *v)
		}
	}
	if req.Pids != nil && *req.Pids < 0 {
		return p, errors.New("invalid process limit")
	}
	envChanged := (req.Runtime != nil && *req.Runtime != p.Runtime) ||
		(req.Memory != nil && strings.TrimSpace(*req.Memory) != p.Memory) ||
		(req.CPUs != nil && strings.TrimSpace(*req.CPUs) != p.CPUs) ||
		(req.Pids != nil && *req.Pids != p.Pids)
	if envChanged {
		// Agents must restart to run in the new environment.
		if err := m.StopProject(id, false); err != nil {
			log.Printf("stop project %s: %v", id, err)
		}
	}
	np, _ := m.st.UpdateProject(id, func(x *model.Project) {
		if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
			x.Name = strings.TrimSpace(*req.Name)
		}
		if req.Runtime != nil {
			x.Runtime = *req.Runtime
		}
		if req.Memory != nil {
			x.Memory = strings.TrimSpace(*req.Memory)
		}
		if req.CPUs != nil {
			x.CPUs = strings.TrimSpace(*req.CPUs)
		}
		if req.Pids != nil {
			x.Pids = *req.Pids
		}
	})
	m.broadcastProject(np)
	return np, nil
}

// ProjectEnv returns the project's container environment file contents.
func (m *Manager) ProjectEnv(id string) (string, error) {
	if _, ok := m.st.Project(id); !ok || m.sb == nil {
		return "", errors.New("project not found")
	}
	b, err := os.ReadFile(m.sb.ProjectEnvPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

// SharedEnv returns the env file exported in every isolated project.
func (m *Manager) SharedEnv() (string, error) {
	if m.sb == nil {
		return "", errors.New("isolation is not available")
	}
	if err := sandbox.SeedShared(m.sb.SharedPath()); err != nil {
		return "", err
	}
	b, err := os.ReadFile(m.sb.SharedEnvPath())
	return string(b), err
}

// SetSharedEnv replaces the shared env file. Containers pick it up when they
// are next (re)created; idle ones are re-created on their next use.
func (m *Manager) SetSharedEnv(content string) error {
	if m.sb == nil {
		return errors.New("isolation is not available")
	}
	if err := validateEnv(content); err != nil {
		return err
	}
	return os.WriteFile(m.sb.SharedEnvPath(), []byte(content), 0o600)
}

func validateEnv(content string) error {
	for i, line := range strings.Split(content, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if k, _, ok := strings.Cut(l, "="); !ok || !envKeyRe.MatchString(k) {
			return fmt.Errorf("line %d: expected KEY=VALUE", i+1)
		}
	}
	return nil
}

// SetProjectEnv replaces the project's container environment file. It
// applies when the container is next (re)created.
func (m *Manager) SetProjectEnv(id, content string) error {
	if _, ok := m.st.Project(id); !ok || m.sb == nil {
		return errors.New("project not found")
	}
	if err := validateEnv(content); err != nil {
		return err
	}
	p := m.sb.ProjectEnvPath(id)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o600)
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
