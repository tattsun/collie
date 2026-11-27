package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tattsun/collie/internal/egress"
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

// errNotInOwnerRoot: new folders are only ever created inside the owner's.
var errNotInOwnerRoot = errors.New("new projects go inside their owner's folder")

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

// ownerNameRe matches the user and group names that name owners' folders:
// one folder name, which can't step out of (or alias) the folder it's in.
var ownerNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,99}$`)

// OwnerRoot is where new projects of a user / group are created. Users and
// groups live in separate subtrees so a name can never alias another
// owner's folder.
func (m *Manager) OwnerRoot(a Actor, groupID string) (string, error) {
	base, name := filepath.Join(m.cfg.ProjectRoot, "users"), a.Name
	if groupID != "" && m.acc != nil {
		base, name = filepath.Join(m.cfg.ProjectRoot, "groups"), m.acc.GroupName(groupID)
	}
	if !ownerNameRe.MatchString(name) {
		return "", fmt.Errorf("%q can't name a folder", name)
	}
	return filepath.Join(base, name), nil
}

func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// DefaultRuntime is the runtime of a new project that doesn't choose one:
// isolated where podman is available, the host otherwise.
func (m *Manager) DefaultRuntime() string {
	if m.sb != nil && m.sb.Available() {
		return "container"
	}
	return "host"
}

// RuntimeFor is the runtime a project created with req gets.
func (m *Manager) RuntimeFor(a Actor, req CreateProjectReq) string {
	switch {
	case !a.Admin:
		return "container"
	case req.Runtime == "":
		return m.DefaultRuntime()
	}
	return req.Runtime
}

// remoteURL checks that an admin's clone URL names a remote repository:
// https://, http://, ssh:// or git://, or scp-like user@host:path. Git
// would take anything else as a path on the server (relative ones too,
// such as another user's project), or run a transport helper (ext::).
func remoteURL(raw string) bool {
	for _, c := range raw {
		if c <= ' ' || c == 0x7f {
			return false
		}
	}
	if scpURLRe.MatchString(raw) {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "https", "http", "ssh", "git":
	default:
		return false
	}
	host := u.Hostname()
	return host != "" && !strings.HasPrefix(host, "-") && !strings.HasPrefix(u.User.Username(), "-") && u.Opaque == ""
}

// scpURLRe is git's scp-like syntax with a user: user@host:path.
var scpURLRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*@[A-Za-z0-9][A-Za-z0-9.-]*:[^:]+$`)

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
	}
	// Decided first, so that every check below (and the caller's passkey
	// check) sees the runtime the project really gets.
	req.Runtime = m.RuntimeFor(a, req)
	root, err := m.OwnerRoot(a, req.Group)
	if err != nil {
		return model.Project{}, err
	}
	// Also where there is no podman: then a group project can't be made.
	if req.Group != "" && req.Runtime == "host" {
		return model.Project{}, errors.New("group projects always run isolated")
	}
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
		if strings.HasPrefix(url, "-") || strings.HasPrefix(url, "file:") || strings.HasPrefix(url, "/") || strings.HasPrefix(url, ".") || strings.Contains(url, "::") || !remoteURL(url) {
			return model.Project{}, errors.New("only remote repository URLs can be cloned (https://, ssh://, git:// or user@host:path)")
		}
		if !a.Admin && !strings.HasPrefix(url, "https://") {
			return model.Project{}, errors.New("use an https:// URL (private GitHub repositories work with GH_TOKEN in your account environment)")
		}
		if !a.Admin && (m.sb == nil || !m.sb.Available()) {
			return model.Project{}, errors.New("cloning needs the isolated runtime (podman) on the server")
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
	default:
		return model.Project{}, fmt.Errorf("unknown runtime %q", req.Runtime)
	}
	switch req.Kind {
	case "existing":
		p.Git = isRepo(p)
	case "clone":
		p.Status = "cloning"
		url := strings.TrimSpace(req.URL)
		if !a.Admin {
			// A member's clone runs inside their own container for the
			// project: the same network policy as their agents (no host
			// loopback, no LAN/tailnet behind the firewall), their own
			// GH_TOKEN, and none of the server's credentials.
			if !strings.HasPrefix(path, root+string(filepath.Separator)) {
				return model.Project{}, errNotInOwnerRoot
			}
			if err := os.MkdirAll(path, 0o755); err != nil {
				return model.Project{}, err
			}
		}
		member := !a.Admin
		uid := a.ID
		go func() {
			var err error
			if member {
				err = m.cloneInContainer(p, uid, url)
			} else {
				err = gitutil.Clone(url, path)
			}
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
		if !strings.HasPrefix(path, root+string(filepath.Separator)) {
			return model.Project{}, errNotInOwnerRoot
		}
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
	if p.Home {
		return errHomeFixed
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
	m.closeToolSockets(id)
	m.deleteSchedulesWhere(func(sc model.Schedule) bool { return sc.ProjectID == id })
	m.st.DeleteProject(id)
	m.pushProjectDeleted(id)
}

func (m *Manager) ProjectChanges(a Actor, id string) (*gitutil.Changes, error) {
	p, err := m.visibleProject(a.ID, id)
	if err != nil {
		return nil, err
	}
	ch, err := m.git(p, a.ID, true).Diff(p.Path, "")
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
	Nested  *bool   `json:"nested"` // allow Podman inside the containers
	// What the containers can connect to (the project's managers decide):
	// "" the internet, "allowlist" only NetAllow's hosts.
	Network  *string   `json:"network"`
	NetAllow *[]string `json:"netAllow"`
	Image    *string   `json:"image"` // agent image (the project's managers pick it)
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
	if p.Home && (req.Name != nil || req.Runtime != nil || req.Nested != nil || req.Network != nil || req.NetAllow != nil || req.Image != nil) {
		return p, errHomeFixed
	}
	if !m.ProjectManageable(a.ID, p) {
		return p, ErrForbidden
	}
	envReq := req.Runtime != nil || req.Memory != nil || req.CPUs != nil || req.Pids != nil || req.Nested != nil
	if envReq && !a.Admin {
		return p, errors.New("only admins can change where agents run and their limits")
	}
	runtime, nested := p.Runtime, p.Nested
	if req.Runtime != nil {
		runtime = *req.Runtime
	}
	if req.Nested != nil {
		nested = *req.Nested
	} else if runtime != "container" {
		nested = false // moving to the host drops it
	}
	if nested && runtime != "container" {
		return p, errors.New("containers inside are only for isolated projects")
	}
	network, netAllow := p.Network, p.NetAllow
	if req.Network != nil {
		network = *req.Network
	} else if runtime != "container" {
		network = "" // a host-run agent has the server's network
	}
	if network != "" && network != model.NetAllowlist {
		return p, fmt.Errorf("unknown network setting %q", network)
	}
	if network != "" && runtime != "container" {
		return p, errors.New("a network allowlist is only for isolated projects")
	}
	if req.NetAllow != nil {
		list, err := egress.NormalizeList(*req.NetAllow)
		if err != nil {
			return p, err
		}
		netAllow = list
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
	image := p.Image
	if req.Image != nil {
		image = *req.Image
		if image != p.Image && (runtime != "container" || m.sb == nil || !m.sb.ValidImage(image)) {
			if runtime != "container" {
				return p, errors.New("agent images are for isolated projects")
			}
			return p, sandbox.ErrNoImage
		}
	}
	envChanged := (req.Runtime != nil && *req.Runtime != p.Runtime) ||
		(req.Memory != nil && strings.TrimSpace(*req.Memory) != p.Memory) ||
		(req.CPUs != nil && strings.TrimSpace(*req.CPUs) != p.CPUs) ||
		(req.Pids != nil && *req.Pids != p.Pids) ||
		nested != p.Nested || image != p.Image ||
		network != p.Network // the containers restart with the other network
	if envChanged {
		if err := m.stopProject(id, false); err != nil {
			log.Printf("stop project %s: %v", id, err)
		}
	}
	// Moving to the host pauses the project's schedules first, so that none
	// starts there in between.
	if req.Runtime != nil && p.Isolated() && *req.Runtime != "container" {
		m.pauseProjectSchedules(id)
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
		x.Nested = nested
		// The list applies at once: the proxy looks it up for every
		// connection, and ends open ones it no longer allows (below).
		x.Network, x.NetAllow = network, netAllow
		x.Image = image
	})
	if m.sb != nil && (req.NetAllow != nil || req.Network != nil) {
		m.sb.Egress().Recheck(id)
	}
	m.pushProject(np)
	return np, nil
}

// NetworkLog lists where the project's containers tried to connect since
// Collie started, for its managers to adjust the allowlist.
func (m *Manager) NetworkLog(a Actor, id string) ([]egress.Entry, error) {
	p, err := m.visibleProject(a.ID, id)
	if err != nil {
		return nil, err
	}
	if !m.ProjectManageable(a.ID, p) {
		return nil, ErrForbidden
	}
	if m.sb == nil {
		return []egress.Entry{}, nil
	}
	return m.sb.NetworkLog(id), nil
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

// DeleteImage removes a custom agent image (admins); the projects that
// picked it follow the default from now on.
func (m *Manager) DeleteImage(id string) error {
	if m.sb == nil {
		return errors.New("isolation is not available")
	}
	if err := m.sb.DeleteImage(id); err != nil {
		return err
	}
	for _, p := range m.st.Projects() {
		if p.Image == id {
			if np, ok := m.st.UpdateProject(p.ID, func(x *model.Project) { x.Image = sandbox.ImageDefault }); ok {
				m.pushProject(np)
			}
		}
	}
	return nil
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
	m.deleteSchedulesWhere(func(sc model.Schedule) bool { return sc.Owner == uid })
	if m.sb != nil {
		m.sb.RemoveUser(uid)
	}
	m.forgetUsage(uid)
	m.forgetCaps(uid)
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

// cloneInContainer clones url into the (empty) project directory from
// inside the user's container for the project.
func (m *Manager) cloneInContainer(p model.Project, uid, url string) error {
	if err := m.ensureContainer(p, uid, nil); err != nil {
		return err
	}
	argv, kill := m.sb.Exec(p.ID, uid, p.Path, "clone-"+model.NewID(), false,
		[]string{"GIT_TERMINAL_PROMPT=0"}, []string{"git", "clone", "--quiet", "--", url, "."})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	// argv[0] is podman; name it from the configuration, not the list that
	// also carries the URL.
	cmd := exec.CommandContext(ctx, m.sb.Podman(), argv[1:]...)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			kill("KILL")
		}
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		if lines := strings.Split(msg, "\n"); len(lines) > 4 {
			msg = strings.Join(lines[len(lines)-4:], "\n")
		}
		return fmt.Errorf("git clone: %s", msg)
	}
	return nil
}
