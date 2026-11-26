// Package sandbox runs a project's agents and shells inside a rootless
// Podman container: one long-lived container per project, with only that
// project mounted, resource limits, and a per-project home directory.
package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tattsun/collie/internal/model"
)

//go:embed image
var imageFS embed.FS

// Paths inside the container.
const (
	Home      = "/home/agent"
	SharedDir = "/opt/collie/shared"
)

var ErrUnavailable = errors.New("podman is not installed on the server")

type Config struct {
	Podman  string // podman executable
	DataDir string // collie data dir (homes/, shared/, worktrees/)
	Memory  string // default memory limit, e.g. "4g"
	CPUs    string // default CPU limit, e.g. "2"
	Pids    int    // default process limit
}

type Status struct {
	Available bool   `json:"available"`
	Limits    bool   `json:"limits"` // resource limits can be enforced
	Image     string `json:"image"`
	Ready     bool   `json:"ready"`
	Building  bool   `json:"building"`
	Error     string `json:"error,omitempty"`
	Log       string `json:"log,omitempty"`
	Shared    string `json:"shared"`
	// Server-wide default limits.
	Memory string `json:"memory"`
	CPUs   string `json:"cpus"`
	Pids   int    `json:"pids"`
}

type Manager struct {
	cfg       Config
	available bool
	tag       string
	global    []string // flags for every podman invocation

	mu       sync.Mutex
	building bool
	buildErr string
	buildLog *tail
	built    chan struct{} // closed when the current build finishes
	ready    bool
	locks    map[string]*sync.Mutex

	// OnChange is called when the image status changes.
	OnChange func()
}

func New(cfg Config) *Manager {
	if cfg.Podman == "" {
		cfg.Podman = "podman"
	}
	m := &Manager{cfg: cfg, locks: map[string]*sync.Mutex{}, buildLog: &tail{max: 6 << 10}}
	_, err := exec.LookPath(cfg.Podman)
	m.available = err == nil
	m.tag = "localhost/collie-agent:" + imageHash()
	m.global = []string{"--log-level=error"}
	if !userBusAvailable() {
		// No systemd user session (e.g. WSL, or lingering not enabled):
		// the systemd cgroup manager cannot work, so use cgroupfs.
		m.global = append(m.global, "--cgroup-manager=cgroupfs")
	}
	if m.available {
		m.ready = m.imageExists()
	}
	return m
}

func (m *Manager) Available() bool { return m.available }

// LimitsSupported reports whether rootless containers can get resource
// limits here: that needs the systemd cgroup manager with a user session
// (on a server: `loginctl enable-linger <user>`).
func (m *Manager) LimitsSupported() bool { return userBusAvailable() }
func (m *Manager) SharedPath() string {
	return filepath.Join(m.cfg.DataDir, "shared")
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{Available: m.available, Limits: m.available && m.LimitsSupported(), Image: m.tag, Ready: m.ready,
		Building: m.building, Error: m.buildErr, Shared: m.SharedPath(), Memory: m.cfg.Memory, CPUs: m.cfg.CPUs, Pids: m.cfg.Pids}
	if m.building || m.buildErr != "" {
		st.Log = m.buildLog.String()
	}
	return st
}

func (m *Manager) changed() {
	if m.OnChange != nil {
		go m.OnChange()
	}
}

// imageHash identifies the image definition, so a changed Containerfile
// (i.e. a Collie upgrade) results in a new image tag.
func imageHash() string {
	h := sha256.New()
	_ = fs.WalkDir(imageFS, "image", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := imageFS.ReadFile(p)
			h.Write([]byte(p))
			h.Write(b)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func (m *Manager) podman(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, m.cfg.Podman, append(append([]string(nil), m.global...), args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("podman %s: %s", args[0], msg)
	}
	return out.String(), nil
}

func (m *Manager) imageExists() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := m.podman(ctx, "image", "exists", m.tag)
	return err == nil
}

// ---- image build ----

// Build builds the agent image in the background. With refresh, the agent
// layer is rebuilt to pick up the latest claude / codex releases.
func (m *Manager) Build(refresh bool) error {
	if !m.available {
		return ErrUnavailable
	}
	m.mu.Lock()
	if m.building {
		m.mu.Unlock()
		return nil
	}
	m.building, m.buildErr = true, ""
	m.buildLog = &tail{max: 6 << 10}
	m.built = make(chan struct{})
	done := m.built
	logw := m.buildLog
	m.mu.Unlock()
	m.changed()

	go func() {
		err := m.build(refresh, logw)
		m.mu.Lock()
		m.building = false
		if err != nil {
			m.buildErr = err.Error()
			log.Printf("agent image build failed: %v", err)
		} else {
			m.ready = true
			log.Printf("agent image %s built", m.tag)
		}
		close(done)
		m.mu.Unlock()
		m.changed()
	}()
	return nil
}

func (m *Manager) build(refresh bool, w io.Writer) error {
	dir, err := os.MkdirTemp("", "collie-image-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	err = fs.WalkDir(imageFS, "image", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := imageFS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, filepath.Base(p)), b, 0o755)
	})
	if err != nil {
		return err
	}
	args := append(append([]string(nil), m.global...), "build", "-t", m.tag, "-t", "localhost/collie-agent:latest", "-f", "Containerfile")
	if refresh {
		args = append(args, "--build-arg", "AGENTS_REV="+strconv.FormatInt(time.Now().Unix(), 10))
	}
	args = append(args, ".")
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.cfg.Podman, args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("image build failed: %w", err)
	}
	return nil
}

// BuildSync builds the image in the foreground, streaming the log to w,
// refreshing the agents layer.
func (m *Manager) BuildSync(w io.Writer) error {
	if !m.available {
		return ErrUnavailable
	}
	return m.build(true, w)
}

// WaitImage makes sure the image exists, building it if needed, and waits
// (up to the timeout) for the build to finish. notify is called once if a
// build has to be waited for.
func (m *Manager) WaitImage(timeout time.Duration, notify func()) error {
	if !m.available {
		return ErrUnavailable
	}
	m.mu.Lock()
	ready, building := m.ready, m.building
	m.mu.Unlock()
	if ready {
		return nil
	}
	if !building {
		if err := m.Build(false); err != nil {
			return err
		}
	}
	if notify != nil {
		notify()
	}
	m.mu.Lock()
	done := m.built
	m.mu.Unlock()
	select {
	case <-done:
	case <-time.After(timeout):
		return errors.New("timed out waiting for the agent image build")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ready {
		return fmt.Errorf("agent image build failed: %s", m.buildErr)
	}
	return nil
}

// ---- containers ----

func Name(projectID string) string { return "collie-" + projectID }

func (m *Manager) HomeDir(projectID string) string {
	return filepath.Join(m.cfg.DataDir, "homes", projectID)
}

// WorktreeRoot is where session worktrees of a project live; it is mounted
// into the project's container at the same path.
func (m *Manager) WorktreeRoot(projectID string) string {
	return filepath.Join(m.cfg.DataDir, "worktrees", projectID)
}

func (m *Manager) lock(projectID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[projectID]
	if !ok {
		l = &sync.Mutex{}
		m.locks[projectID] = l
	}
	return l
}

type containerState struct {
	exists  bool
	running bool
	config  string
}

func (m *Manager) inspect(ctx context.Context, name string) containerState {
	out, err := m.podman(ctx, "container", "inspect", "--format", `{{.State.Running}} {{index .Config.Labels "collie.config"}}`, name)
	if err != nil {
		return containerState{}
	}
	f := strings.Fields(out)
	st := containerState{exists: true, running: len(f) > 0 && f[0] == "true"}
	if len(f) > 1 {
		st.config = f[1]
	}
	return st
}

// Ensure starts the project's container, creating (or re-creating, when
// its configuration changed and recreate is allowed) it as needed.
func (m *Manager) Ensure(p model.Project, recreate bool) error {
	if !m.available {
		return ErrUnavailable
	}
	l := m.lock(p.ID)
	l.Lock()
	defer l.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := m.prepare(p); err != nil {
		return err
	}
	args := m.runArgs(p)
	want := configHash(args)
	name := Name(p.ID)
	st := m.inspect(ctx, name)
	if st.exists && (st.config == want || !recreate) {
		if !st.running {
			if _, err := m.podman(ctx, "start", name); err != nil {
				return err
			}
		}
		return nil
	}
	if st.exists {
		if _, err := m.podman(ctx, "rm", "-f", "-t", "5", name); err != nil {
			return err
		}
	}
	full := append([]string{"run", "-d", "--label", "collie.config=" + want}, args...)
	if _, err := m.podman(ctx, full...); err != nil {
		return err
	}
	return nil
}

func (m *Manager) runArgs(p model.Project) []string {
	name := Name(p.ID)
	home := m.HomeDir(p.ID)
	shared := m.SharedPath()
	user := currentUser()
	mem, cpus, pids := m.cfg.Memory, m.cfg.CPUs, m.cfg.Pids
	if p.Memory != "" {
		mem = p.Memory
	}
	if p.CPUs != "" {
		cpus = p.CPUs
	}
	if p.Pids > 0 {
		pids = p.Pids
	}
	args := []string{
		"--name", name,
		"--label", "collie.project=" + p.ID,
		"--hostname", hostname(p.Name),
		"--init",
		// Run as the server user (files stay owned by it) with its own home.
		"--userns=keep-id",
		"--passwd-entry", fmt.Sprintf("%s:*:$UID:$GID::%s:/bin/bash", user, Home),
		"--security-opt", "no-new-privileges",
		"--cap-drop", "all",
		// No shortcuts to the host: neither its loopback nor the
		// host.containers.internal alias is mapped into the container. (The
		// host's other addresses are blocked by deploy/setup-host.sh's
		// firewall.)
		"--network", "pasta:--map-host-loopback,none,--map-guest-addr,none",
		// Our own minimal /etc/hosts: podman's would add host aliases
		// (host.containers.internal / host.docker.internal) and copy the
		// server's entries.
		"--no-hosts",
		"-v", m.hostsPath(p.ID) + ":/etc/hosts:ro",
		"-e", "HOME=" + Home,
		"-e", "COLLIE_PROJECT=" + p.Name,
		"-v", home + ":" + Home,
		"-v", p.Path + ":" + p.Path,
		"-v", m.WorktreeRoot(p.ID) + ":" + m.WorktreeRoot(p.ID),
		"-v", shared + ":" + SharedDir + ":ro",
		"-w", p.Path,
	}
	if mem != "" && mem != "0" {
		args = append(args, "--memory", mem, "--memory-swap", mem)
	}
	if cpus != "" && cpus != "0" {
		args = append(args, "--cpus", cpus)
	}
	if pids > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(pids))
	}
	// Shared profile pieces at the places the agents look for them.
	ro := func(src, dst string) {
		if _, err := os.Stat(src); err == nil {
			args = append(args, "-v", src+":"+dst+":ro")
		}
	}
	ro(filepath.Join(shared, "claude", "CLAUDE.md"), Home+"/.claude/CLAUDE.md")
	ro(filepath.Join(shared, "claude", "skills"), Home+"/.claude/skills")
	ro(filepath.Join(shared, "claude", "agents"), Home+"/.claude/agents")
	ro(filepath.Join(shared, "claude", "commands"), Home+"/.claude/commands")
	ro(filepath.Join(shared, "codex", "AGENTS.md"), Home+"/.codex/AGENTS.md")
	ro(filepath.Join(shared, "codex", "skills"), Home+"/.codex/skills")
	ro(filepath.Join(shared, "gitconfig"), Home+"/.gitconfig")
	// Codex's login is shared with the server user so token refreshes are
	// seen by every project.
	if h, err := os.UserHomeDir(); err == nil {
		auth := filepath.Join(h, ".codex", "auth.json")
		if _, err := os.Stat(auth); err == nil {
			args = append(args, "-v", auth+":"+Home+"/.codex/auth.json")
		}
	}
	if fi, err := os.Stat(filepath.Join(shared, "env")); err == nil && fi.Size() > 0 {
		args = append(args, "--env-file", filepath.Join(shared, "env"))
	}
	if env := projectEnvPath(m.cfg.DataDir, p.ID); fileNonEmpty(env) {
		args = append(args, "--env-file", env)
	}
	args = append(args, m.tag, "sleep", "infinity")
	return args
}

// SharedEnvPath is the env file exported in every project container.
func (m *Manager) SharedEnvPath() string { return filepath.Join(m.SharedPath(), "env") }

func projectEnvPath(dataDir, projectID string) string {
	return filepath.Join(dataDir, "homes", projectID+".env")
}

// ProjectEnvPath is a per-project env file (KEY=VALUE lines) passed to the
// project's container, e.g. a GH_TOKEN scoped to that repository.
func (m *Manager) ProjectEnvPath(projectID string) string {
	return projectEnvPath(m.cfg.DataDir, projectID)
}

func fileNonEmpty(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Size() > 0
}

func configHash(args []string) string {
	h := sha256.New()
	for _, a := range args {
		h.Write([]byte(a))
		h.Write([]byte{0})
	}
	// env files are read at creation time; include their contents
	for i, a := range args {
		if a == "--env-file" && i+1 < len(args) {
			b, _ := os.ReadFile(args[i+1])
			h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func hostname(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 50 {
		s = s[:50]
	}
	if s == "" {
		s = "project"
	}
	return s
}

func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "agent"
}

// prepare creates the host directories the container mounts and seeds the
// shared profile on first use.
func (m *Manager) prepare(p model.Project) error {
	home := m.HomeDir(p.ID)
	for _, d := range []string{home, m.WorktreeRoot(p.ID)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if err := SeedShared(m.SharedPath()); err != nil {
		return err
	}
	// Create every mount point inside the home ourselves. Otherwise podman
	// creates them as the namespace's root (a subuid on the host) and the
	// agent, running as the server user, cannot write to e.g. ~/.codex.
	m.repairOwnership(home)
	for _, d := range []string{".claude", ".claude/skills", ".claude/agents", ".claude/commands", ".codex", ".codex/skills"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			return err
		}
	}
	for _, f := range []string{".claude/CLAUDE.md", ".codex/AGENTS.md", ".gitconfig", ".codex/auth.json"} {
		fp := filepath.Join(home, f)
		if _, err := os.Lstat(fp); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(fp, nil, 0o600); err != nil {
				return err
			}
		}
	}
	hosts := fmt.Sprintf("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n127.0.1.1\t%s\n", hostname(p.Name))
	if err := os.WriteFile(m.hostsPath(p.ID), []byte(hosts), 0o644); err != nil {
		return err
	}
	return migrateClaudeState(p, home, m.WorktreeRoot(p.ID))
}

func (m *Manager) hostsPath(projectID string) string {
	return filepath.Join(m.cfg.DataDir, "homes", projectID+".hosts")
}

// repairOwnership fixes a home whose entries were created by the
// container's root (owned by a subuid): inside `podman unshare`, uid 0 is
// the server user.
func (m *Manager) repairOwnership(home string) {
	uid := os.Getuid()
	bad := false
	_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil || bad {
			return nil
		}
		if info, err := d.Info(); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid {
				bad = true
			}
		}
		return nil
	})
	if !bad {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := m.podman(ctx, "unshare", "chown", "-R", "0:0", home); err != nil {
		log.Printf("repair ownership of %s: %v", home, err)
	}
}

// Exec returns the argv that runs command inside the project's container
// (in dir, attached to stdio), and a function that signals it. id must be
// unique per running process.
func (m *Manager) Exec(projectID, dir, id string, tty bool, env []string, command []string) ([]string, func(sig string)) {
	argv := append([]string{m.cfg.Podman}, m.global...)
	argv = append(argv, "exec", "-i", "-w", dir)
	if tty {
		argv = append(argv, "-t", "-e", "TERM=xterm-256color", "-e", "COLORTERM=truecolor")
	}
	for _, e := range env {
		argv = append(argv, "-e", e)
	}
	argv = append(argv, Name(projectID), "collie-exec", id)
	argv = append(argv, command...)
	kill := func(sig string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = m.podman(ctx, "exec", Name(projectID), "collie-kill", id, sig)
	}
	return argv, kill
}

// Stop stops the project's container (everything in it ends).
func (m *Manager) Stop(projectID string, kill bool) error {
	if !m.available {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if kill {
		_, err := m.podman(ctx, "kill", Name(projectID))
		if err != nil && !strings.Contains(err.Error(), "no such container") && !strings.Contains(err.Error(), "not running") {
			return err
		}
		return nil
	}
	_, err := m.podman(ctx, "stop", "-t", "5", Name(projectID))
	if err != nil && strings.Contains(err.Error(), "no such container") {
		return nil
	}
	return err
}

// Remove deletes the project's container and its home directory.
func (m *Manager) Remove(projectID string) {
	if m.available {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		_, _ = m.podman(ctx, "rm", "-f", "-t", "3", Name(projectID))
		cancel()
	}
	_ = os.RemoveAll(m.HomeDir(projectID))
	_ = os.Remove(projectEnvPath(m.cfg.DataDir, projectID))
	_ = os.Remove(m.hostsPath(projectID))
}

type Info struct {
	Running bool   `json:"running"`
	Exists  bool   `json:"exists"`
	Mem     string `json:"mem,omitempty"`
	CPU     string `json:"cpu,omitempty"`
	Pids    string `json:"pids,omitempty"`
	// LimitsEnforced is false when the host cannot apply resource limits
	// to rootless containers (no systemd user session / cgroup delegation).
	LimitsEnforced bool `json:"limitsEnforced"`
}

// Inspect reports the container state and current resource usage.
func (m *Manager) Inspect(projectID string) Info {
	if !m.available {
		return Info{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st := m.inspect(ctx, Name(projectID))
	info := Info{Exists: st.exists, Running: st.running}
	if st.running {
		if out, err := m.podman(ctx, "exec", Name(projectID), "cat", "/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/pids.max"); err == nil {
			f := strings.Fields(out)
			info.LimitsEnforced = len(f) == 2 && (f[0] != "max" || f[1] != "max") && m.LimitsSupported()
		}
		out, err := m.podman(ctx, "stats", "--no-stream", "--no-reset", "--format", "{{.MemUsage}}|{{.CPUPerc}}|{{.PIDs}}", Name(projectID))
		if err == nil {
			f := strings.Split(strings.TrimSpace(out), "|")
			if len(f) == 3 {
				info.Mem, info.CPU, info.Pids = strings.TrimSpace(f[0]), strings.TrimSpace(f[1]), strings.TrimSpace(f[2])
			}
		}
	}
	return info
}

// ---- helpers ----

func userBusAvailable() bool {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	_, err := os.Stat(filepath.Join(dir, "bus"))
	return err == nil
}

type tail struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(b), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
