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
	Group   string `json:"group"`   // owning group id ("" = personal)
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

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

// OwnerRoot is where new projects of a user / group are created. Users and
// groups live in separate subtrees so a name can never alias another
// owner's folder.
func (m *Manager) OwnerRoot(a Actor, groupID string) string {
	if groupID != "" && m.acc != nil {
		return filepath.Join(m.cfg.ProjectRoot, "groups", m.acc.GroupName(groupID))
	}
	return filepath.Join(m.cfg.ProjectRoot, "users", a.Name)
}

func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func (m *Manager) CreateProject(a Actor, req CreateProjectReq) (model.Project, error) {
	if req.Group != "" && m.role(req.Group, a.ID) == "" {
		return model.Project{}, ErrNotFound
	}
	if !a.Admin {
		// Members only get isolated projects inside their own folder.
		if req.Kind == "existing" {
			return model.Project{}, errors.New("only admins can add existing folders")
		}
		if req.Runtime == "host" {
			return model.Project{}, errors.New("only admins can run agents directly on the server")
		}
		req.Runtime = "container"
	}
	if req.Group != "" && req.Runtime == "host" {
		return model.Project{}, errors.New("group projects always run isolated")
	}
	root := m.OwnerRoot(a, req.Group)
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
		if strings.HasPrefix(url, "-") || strings.HasPrefix(url, "file:") || strings.HasPrefix(url, "/") || strings.HasPrefix(url, ".") || strings.Contains(url, "::") {
			return model.Project{}, errors.New("only remote repository URLs can be cloned")
		}
		if !a.Admin && !strings.HasPrefix(url, "https://") {
			return model.Project{}, errors.New("use an https:// URL (private GitHub repositories work with GH_TOKEN in your account environment)")
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
		path = filepath.Join(root, name)
		if _, err := os.Stat(path); err == nil {
			return model.Project{}, fmt.Errorf("%s already exists", path)
		}
	default:
		if !nameRe.MatchString(name) {
			return model.Project{}, errors.New("invalid project name (letters, digits, . _ -)")
		}
		path = filepath.Join(root, name)
		if _, err := os.Stat(path); err == nil {
			return model.Project{}, fmt.Errorf("%s already exists", path)
		}
	}
	// A project must not contain, or sit inside, another project: its
	// containers would expose the other project's files.
	for _, p := range m.st.Projects() {
		if within(path, p.Path) || within(p.Path, path) {
			return model.Project{}, fmt.Errorf("%s overlaps an existing project", path)
		}
	}
	if within(m.cfg.ProjectRoot, path) || path == filepath.Join(m.cfg.ProjectRoot, "groups") || path == filepath.Join(m.cfg.ProjectRoot, "users") {
		return model.Project{}, errors.New("that folder holds other people's projects")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return model.Project{}, err
	}
	p := model.Project{ID: model.NewID(), Name: name, Path: path, CreatedAt: model.NowMs(), CreatedBy: a.ID}
	if req.Group != "" {
		p.OwnerGroup = req.Group
	} else {
		p.OwnerUser = a.ID
	}
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
		opts := gitutil.CloneOpts{Restricted: !a.Admin}
		if !a.Admin {
			if env, err := m.UserEnv(a.ID); err == nil {
				opts.GitHubToken = envValue(env, "GH_TOKEN")
			}
		}
		go func() {
			err := gitutil.Clone(url, path, opts)
			np, ok := m.st.UpdateProject(p.ID, func(x *model.Project) {
				if err != nil {
					x.Status, x.Error = "error", err.Error()
				} else {
					x.Status, x.Error, x.Git = "", "", true
				}
			})
			if ok {
				m.pushProject(np)
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
	m.pushProject(p)
	return p, nil
}

// DeleteProject unregisters the project and deletes its sessions. Files on
// disk are left untouched (except collie-created worktrees).
func (m *Manager) DeleteProject(a Actor, id string) error {
	p, err := m.visibleProject(a.ID, id)
	if err != nil {
		return err
	}
	if !m.ProjectManageable(a.ID, p) {
		return ErrForbidden
	}
	m.deleteProject(id)
	return nil
}

func (m *Manager) deleteProject(id string) {
	_ = m.stopProject(id, true)
	for _, s := range m.st.Sessions() {
		if s.ProjectID == id {
			_ = m.deleteSession(s.ID, true)
		}
	}
	if m.sb != nil {
		m.sb.Remove(id)
		_ = os.Remove(m.sb.WorktreeRoot(id)) // only if empty
	}
	m.st.DeleteProject(id)
	m.pushProjectDeleted(id)
}

func (m *Manager) ProjectChanges(a Actor, id string) (*gitutil.Changes, error) {
	p, err := m.visibleProject(a.ID, id)
	if err != nil {
		return nil, err
	}
	ch, err := gitutil.Diff(p.Path, "")
	if err == nil && ch.Repo != p.Git {
		np, _ := m.st.UpdateProject(id, func(x *model.Project) { x.Git = ch.Repo })
		m.pushProject(np)
	}
	return ch, err
}

// SuggestDirs lists directories under the project root that are not yet
// registered as projects (admins only: members cannot add folders).
func (m *Manager) SuggestDirs(a Actor) []string {
	out := []string{}
	if !a.Admin {
		return out
	}
	entries, err := os.ReadDir(m.cfg.ProjectRoot)
	if err != nil {
		return out
	}
	known := map[string]bool{}
	for _, p := range m.st.Projects() {
		known[p.Path] = true
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || e.Name() == "groups" || e.Name() == "users" {
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

// PatchProject changes a project's settings. Where and with which limits
// agents run is an admin decision; changing it stops the project's agents
// (they resume in the new environment on the next message).
func (m *Manager) PatchProject(a Actor, id string, req PatchProjectReq) (model.Project, error) {
	p, err := m.visibleProject(a.ID, id)
	if err != nil {
		return p, err
	}
	if !m.ProjectManageable(a.ID, p) {
		return p, ErrForbidden
	}
	envReq := req.Runtime != nil || req.Memory != nil || req.CPUs != nil || req.Pids != nil
	if envReq && !a.Admin {
		return p, errors.New("only admins can change where agents run and their limits")
	}
	if req.Runtime != nil && *req.Runtime != "host" && *req.Runtime != "container" {
		return p, fmt.Errorf("unknown runtime %q", *req.Runtime)
	}
	if req.Runtime != nil && *req.Runtime == "host" && p.OwnerGroup != "" {
		return p, errors.New("group projects always run isolated")
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
		if err := m.stopProject(id, false); err != nil {
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
	m.pushProject(np)
	return np, nil
}

// ProjectEnv returns the project's container environment file contents.
func (m *Manager) ProjectEnv(a Actor, id string) (string, error) {
	p, err := m.visibleProject(a.ID, id)
	if err != nil || m.sb == nil {
		return "", ErrNotFound
	}
	if !m.ProjectManageable(a.ID, p) {
		return "", ErrForbidden
	}
	b, err := os.ReadFile(m.sb.ProjectEnvPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

// SetProjectEnv replaces the project's container environment file. It
// applies when the containers are next (re)created.
func (m *Manager) SetProjectEnv(a Actor, id, content string) error {
	p, err := m.visibleProject(a.ID, id)
	if err != nil || m.sb == nil {
		return ErrNotFound
	}
	if !m.ProjectManageable(a.ID, p) {
		return ErrForbidden
	}
	return writeEnvFile(m.sb.ProjectEnvPath(id), content)
}

// SharedEnv / UserEnv: the global (admin) and personal env files.
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

func (m *Manager) SetSharedEnv(content string) error {
	if m.sb == nil {
		return errors.New("isolation is not available")
	}
	return writeEnvFile(m.sb.SharedEnvPath(), content)
}

func (m *Manager) UserEnv(uid string) (string, error) {
	if m.sb == nil {
		return "", errors.New("isolation is not available")
	}
	b, err := os.ReadFile(m.sb.UserEnvPath(uid))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

func (m *Manager) SetUserEnv(uid, content string) error {
	if m.sb == nil {
		return errors.New("isolation is not available")
	}
	return writeEnvFile(m.sb.UserEnvPath(uid), content)
}

func writeEnvFile(path, content string) error {
	if err := validateEnv(content); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
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

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// AdoptOrphans gives projects and sessions from the single-user version an
// owner (the first admin).
func (m *Manager) AdoptOrphans(ownerID string) {
	for _, p := range m.st.Projects() {
		if p.OwnerUser == "" && p.OwnerGroup == "" {
			m.st.UpdateProject(p.ID, func(x *model.Project) { x.OwnerUser = ownerID })
		}
	}
	for _, s := range m.st.Sessions() {
		if s.Owner == "" {
			m.st.UpdateSession(s.ID, func(x *model.Session) { x.Owner = ownerID })
		}
	}
}

// PurgeUser removes a deleted user's personal projects and the sessions
// they started (their agent identity is gone).
func (m *Manager) PurgeUser(uid string) {
	for _, p := range m.st.Projects() {
		if p.OwnerUser == uid {
			m.deleteProject(p.ID)
		}
	}
	for _, s := range m.st.Sessions() {
		if s.Owner == uid {
			_ = m.deleteSession(s.ID, true)
		}
	}
	if m.sb != nil {
		m.sb.RemoveUser(uid)
	}
}

// GroupProjects lists the projects owned by a group.
func (m *Manager) GroupProjects(groupID string) []model.Project {
	var out []model.Project
	for _, p := range m.st.Projects() {
		if p.OwnerGroup == groupID {
			out = append(out, p)
		}
	}
	return out
}

// AllProjects is for admin overviews (names and owners only).
func (m *Manager) AllProjects() []model.Project { return m.st.Projects() }

// envValue reads KEY from KEY=VALUE lines.
func envValue(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && k == key {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}
