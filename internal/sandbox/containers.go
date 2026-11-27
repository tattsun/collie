package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tattsun/collie/internal/model"
)

// Every (project, user) pair gets its own container: the project's files
// are shared between the members of a group project, but each member's
// agents run with their own accounts, history and home directory.

// Name is the container of a project for a user.
func Name(projectID, userID string) string { return "collie-" + projectID + "-" + userID }

// HomeDir is the container home of a user within a project (Claude Code
// state, transcripts, misc dotfiles).
func (m *Manager) HomeDir(projectID, userID string) string {
	return filepath.Join(m.cfg.DataDir, "homes", projectID, userID)
}

// UserDir holds a user's profile, agent accounts and settings.
func (m *Manager) UserDir(userID string) string { return filepath.Join(m.cfg.DataDir, "users", userID) }

// CodexDir is the user's CODEX_HOME, shared by all of the user's containers
// (its login, threads and memories), like several Codex processes on one
// machine.
func (m *Manager) CodexDir(userID string) string { return filepath.Join(m.UserDir(userID), "codex") }

// UserEnvPath holds the user's own variables (e.g. CLAUDE_CODE_OAUTH_TOKEN).
func (m *Manager) UserEnvPath(userID string) string { return filepath.Join(m.UserDir(userID), "env") }

// WorktreeRoot is where session worktrees of a project live; it is mounted
// into the project's containers at the same path.
func (m *Manager) WorktreeRoot(projectID string) string {
	return filepath.Join(m.cfg.DataDir, "worktrees", projectID)
}

// MemoryDir is the Claude Code memory shared by the members of a group
// project.
func (m *Manager) MemoryDir(projectID string) string {
	return filepath.Join(m.cfg.DataDir, "projmem", projectID)
}

func projectEnvPath(dataDir, projectID string) string {
	return filepath.Join(dataDir, "homes", projectID+".env")
}

// ProjectEnvPath is a per-project env file (KEY=VALUE lines) passed to the
// project's containers, e.g. a GH_TOKEN scoped to that repository.
func (m *Manager) ProjectEnvPath(projectID string) string {
	return projectEnvPath(m.cfg.DataDir, projectID)
}

// SharedEnvPath is the env file exported in every container.
func (m *Manager) SharedEnvPath() string { return filepath.Join(m.SharedPath(), "env") }

func (m *Manager) hostsPath(projectID string) string {
	return filepath.Join(m.cfg.DataDir, "homes", projectID+".hosts")
}

func (m *Manager) lock(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[key]
	if !ok {
		l = &sync.Mutex{}
		m.locks[key] = l
	}
	return l
}

type containerState struct {
	exists  bool
	running bool
	config  string
	network string // collie.network label ("" for the internet)
}

func (m *Manager) inspect(ctx context.Context, name string) containerState {
	out, err := m.podman(ctx, "container", "inspect", "--format",
		`{{.State.Running}}|{{index .Config.Labels "collie.config"}}|{{index .Config.Labels "collie.network"}}`, name)
	if err != nil {
		return containerState{}
	}
	f := strings.Split(strings.TrimSpace(out), "|")
	st := containerState{exists: true, running: f[0] == "true"}
	if len(f) > 1 {
		st.config = f[1]
	}
	if len(f) > 2 && f[2] != "<no value>" {
		st.network = f[2]
	}
	return st
}

// Spec describes the container to run for a user in a project.
type Spec struct {
	Project     model.Project
	UserID      string
	SessionDirs []string // working dirs of the project's sessions (legacy worktrees get mounted)
	// MigrateHostState copies the server user's Claude Code state for this
	// project into the home on first use; only for the user who owns that
	// state (the server's owner), never for anyone else.
	MigrateHostState bool
	// Home containers: the user's projects, mounted read-only at their paths.
	ReadOnly []string
	// The host directory holding Collie's socket for the agents' tools
	// (mounted at CollieRunDir), and Collie's own binary, which bridges an
	// agent's MCP stdio to that socket (at CollieBinPath).
	CollieRun string
	CollieBin string
	// OnSlowStart, if set, is called when creating the container takes a
	// while (see containerCreateTimeout).
	OnSlowStart func()
	// Image is the tag to run (from WaitImage); "" = the standard image.
	Image string
}

func (m *Manager) imageTag(sp Spec) string {
	if sp.Image != "" {
		return sp.Image
	}
	return m.tag
}

// Creating a container can take minutes: the first one on a new image gets
// a copy of the image's files owned by the container's ids, unless the
// kernel can map them on the fly (not when Podman itself runs in a
// container, nor on some filesystems). The other podman calls are quick.
const containerCreateTimeout = 20 * time.Minute

var slowStartAfter = 15 * time.Second // a variable for tests

// Where a container sees Collie's socket directory and binary.
const (
	CollieRunDir  = "/run/collie"
	CollieBinPath = "/usr/local/lib/collie/collie"
)

// Ensure starts the container, creating (or re-creating, when its
// configuration changed and recreate is allowed) it as needed.
func (m *Manager) Ensure(sp Spec, recreate bool) error {
	if !m.available {
		return ErrUnavailable
	}
	p := sp.Project
	l := m.lock(p.ID + "/" + sp.UserID)
	l.Lock()
	defer l.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := m.prepare(sp); err != nil {
		return err
	}
	if sp.CollieBin == "" {
		// Collie runs git in the container with it (gitutil.Helper).
		if exe, err := os.Executable(); err == nil {
			sp.CollieBin = exe
		}
	}
	if err := m.prepareNetwork(&sp); err != nil {
		return fmt.Errorf("start the network proxy: %w", err)
	}
	args := m.runArgs(sp, m.extraMounts(p, sp.SessionDirs))
	want := configHash(append(args, "image-id="+m.imageID(ctx, m.imageTag(sp))))
	name := Name(p.ID, sp.UserID)
	st := m.inspect(ctx, name)
	if st.exists && st.network != networkMode(p) {
		// What the container can reach changed: that can't wait until
		// nothing uses it.
		recreate = true
	}
	m.mu.Lock()
	m.stale[name] = st.exists && st.config != want && !recreate
	m.mu.Unlock()
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
	runCtx, cancelRun := context.WithTimeout(context.Background(), containerCreateTimeout)
	defer cancelRun()
	if sp.OnSlowStart != nil {
		slow := time.AfterFunc(slowStartAfter, sp.OnSlowStart)
		defer slow.Stop()
	}
	if _, err := m.podman(runCtx, full...); err != nil {
		return err
	}
	if st.exists {
		m.background(m.PruneImages) // the old container may have held on to an old image
	}
	return nil
}

// extraMounts lists session directories not covered by the standard mounts.
func (m *Manager) extraMounts(p model.Project, sessionDirs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range sessionDirs {
		d = filepath.Clean(d)
		if d == "" || seen[d] || within(d, p.Path) || within(d, m.WorktreeRoot(p.ID)) {
			continue
		}
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func (m *Manager) runArgs(sp Spec, extra []string) []string {
	p, uid := sp.Project, sp.UserID
	home := m.HomeDir(p.ID, uid)
	eff := m.EffectiveDir(uid)
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
		"--name", Name(p.ID, uid),
		"--label", "collie.project=" + p.ID,
		"--label", "collie.user=" + uid,
		"--hostname", hostname(p.Name),
		"--init",
		// Run as the server user (files stay owned by it) with its own home.
		"--userns=keep-id",
		"--passwd-entry", fmt.Sprintf("%s:*:$UID:$GID::%s:/bin/bash", currentUser(), Home),
		"--cap-drop", "all",
	}
	if m.selinux {
		// The container shares the project's files with the host by design,
		// and SELinux would keep it from them (or relabel them for
		// containers only). Its other walls stay: the user namespace, no
		// capabilities, seccomp, the mounts.
		args = append(args, "--security-opt", "label=disable")
	}
	args = append(args, m.networkArgs(p, uid)...)
	args = append(args,
		// Our own minimal /etc/hosts: podman's would add host aliases and
		// copy the server's entries.
		"--no-hosts",
		"-v", m.hostsPath(p.ID)+":/etc/hosts:ro",
		"-e", "HOME="+Home,
		"-e", "COLLIE_PROJECT="+p.Name,
		"-v", home+":"+Home,
		"-v", m.CodexDir(uid)+":"+Home+"/.codex",
		"-v", p.Path+":"+p.Path,
		"-v", m.WorktreeRoot(p.ID)+":"+m.WorktreeRoot(p.ID),
		"-v", eff+":"+ProfileDir+":ro",
		"-w", p.Path,
	)
	args = append(args, m.nestedArgs(p, uid)...)
	for _, d := range extra {
		args = append(args, "-v", d+":"+d)
	}
	for _, d := range sp.ReadOnly {
		args = append(args, "-v", d+":"+d+":ro")
	}
	if sp.CollieRun != "" {
		// Read-only: connecting needs no write access, and the agent can't
		// swap the socket for something else.
		args = append(args, "-v", sp.CollieRun+":"+CollieRunDir+":ro")
	}
	if sp.CollieBin != "" {
		args = append(args, "-v", sp.CollieBin+":"+CollieBinPath+":ro")
	}
	// The merged (global + personal) profile where the agents look for it.
	ro := func(src, dst string) { args = append(args, "-v", src+":"+dst+":ro") }
	ro(filepath.Join(eff, "claude", "CLAUDE.md"), Home+"/.claude/CLAUDE.md")
	ro(filepath.Join(eff, "claude", "skills"), Home+"/.claude/skills")
	ro(filepath.Join(eff, "claude", "agents"), Home+"/.claude/agents")
	ro(filepath.Join(eff, "claude", "commands"), Home+"/.claude/commands")
	ro(filepath.Join(eff, "codex", "AGENTS.md"), Home+"/.codex/AGENTS.md")
	ro(filepath.Join(eff, "codex", "skills"), Home+"/.codex/skills")
	ro(filepath.Join(eff, "gitconfig"), Home+"/.gitconfig")
	if p.OwnerGroup != "" {
		// Members of a group project share Claude Code's project memory.
		args = append(args, "-v", m.MemoryDir(p.ID)+":"+Home+"/.claude/projects/"+claudeKey(p.Path)+"/memory")
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
	if tag := m.imageTag(sp); tag != m.tag {
		// A custom image runs like the standard one: as the server user
		// (keep-id alone would honor the image's USER) and Collie's command.
		args = append(args, "--user", userSpec(), "--entrypoint", "")
	}
	// No env files here: Exec passes them to every process instead.
	args = append(args, m.imageTag(sp))
	return append(args, mainCommand(p)...)
}

// envFiles lists the env files for the user's container for the project.
// Later files override earlier ones: global < user < the user's Claude
// account for the project < project.
func (m *Manager) envFiles(projectID, userID string) []string {
	var out []string
	for _, f := range []string{m.SharedEnvPath(), m.UserEnvPath(userID), m.accountFile(projectID, userID), m.ProjectEnvPath(projectID)} {
		if f != "" && fileNonEmpty(f) {
			out = append(out, f)
		}
	}
	return out
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

// prepare creates the host directories the container mounts.
func (m *Manager) prepare(sp Spec) error {
	p, uid := sp.Project, sp.UserID
	home := m.HomeDir(p.ID, uid)
	codex := m.CodexDir(uid)
	for _, d := range []string{home, codex, m.WorktreeRoot(p.ID)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if p.OwnerGroup != "" {
		if err := os.MkdirAll(m.MemoryDir(p.ID), 0o700); err != nil {
			return err
		}
	}
	if p.Nested {
		if err := m.prepareNested(p, uid); err != nil {
			return err
		}
	}
	if err := m.BuildEffective(uid); err != nil {
		return fmt.Errorf("build profile: %w", err)
	}
	m.repairOwnership(home)
	m.repairOwnership(codex)
	// Create every mount point ourselves (podman would create them as the
	// namespace's root, i.e. a subuid the agent cannot write as), and do it
	// through os.Root: the agent controls these directories and may have
	// planted symlinks.
	mk := func(dir string, dirs, files []string) error {
		root, err := os.OpenRoot(dir)
		if err != nil {
			return err
		}
		defer root.Close()
		for _, d := range dirs {
			if err := root.MkdirAll(d, 0o700); err != nil {
				return fmt.Errorf("prepare %s: %w", d, err)
			}
		}
		for _, f := range files {
			if _, err := root.Lstat(f); errors.Is(err, os.ErrNotExist) {
				fh, err := root.OpenFile(f, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
				if err != nil {
					return fmt.Errorf("prepare %s: %w", f, err)
				}
				fh.Close()
			}
		}
		return nil
	}
	homeDirs := []string{".claude", ".claude/skills", ".claude/agents", ".claude/commands", ".codex"}
	if p.OwnerGroup != "" {
		homeDirs = append(homeDirs, ".claude/projects/"+claudeKey(p.Path)+"/memory")
	}
	if p.Nested {
		homeDirs = append(homeDirs, strings.TrimPrefix(nestedStorageMount, Home+"/"))
	}
	if err := mk(home, homeDirs, []string{".claude/CLAUDE.md", ".gitconfig"}); err != nil {
		return err
	}
	if err := mk(codex, []string{"skills"}, []string{"AGENTS.md"}); err != nil {
		return err
	}
	hosts := fmt.Sprintf("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n127.0.1.1\t%s\n", hostname(p.Name))
	if err := os.WriteFile(m.hostsPath(p.ID), []byte(hosts), 0o644); err != nil {
		return err
	}
	if sp.MigrateHostState {
		root, err := os.OpenRoot(home)
		if err != nil {
			return err
		}
		defer root.Close()
		hostHome, _ := os.UserHomeDir()
		return migrateClaudeState(p, root, sp.SessionDirs, hostHome)
	}
	return nil
}

// repairOwnership fixes a directory whose entries were created by the
// container's root (owned by a subuid): inside `podman unshare`, uid 0 is
// the server user.
func (m *Manager) repairOwnership(dir string) {
	uid := os.Getuid()
	bad := false
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
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
	// -h: change symlinks themselves, never what they point to.
	if _, err := m.podman(ctx, "unshare", "chown", "-R", "-h", "0:0", dir); err != nil {
		log.Printf("repair ownership of %s: %v", dir, err)
	}
}

// Exec returns the argv that runs command inside the user's container for
// the project (in dir, attached to stdio), and a function that signals it.
// id must be unique per running process.
func (m *Manager) Exec(projectID, userID, dir, id string, tty bool, env []string, command []string) ([]string, func(sig string)) {
	argv := append([]string{m.cfg.Podman}, m.global...)
	argv = append(argv, "exec", "-i", "-w", dir)
	// The env files are read at every exec rather than baked into the
	// container, so saved variables (a new Claude token, say) reach the next
	// agent or shell even while an open terminal keeps the container from
	// being re-created. Explicit -e values below take precedence.
	for _, f := range m.envFiles(projectID, userID) {
		argv = append(argv, "--env-file", f)
	}
	if tty {
		argv = append(argv, "-t", "-e", "TERM=xterm-256color", "-e", "COLORTERM=truecolor")
	}
	for _, e := range env {
		argv = append(argv, "-e", e)
	}
	name := Name(projectID, userID)
	argv = append(argv, name, "collie-exec", id)
	argv = append(argv, command...)
	kill := func(sig string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = m.podman(ctx, "exec", name, "collie-kill", id, sig)
	}
	return argv, kill
}

// BridgeArgv is the command line that runs Collie's port bridge in the
// user's container for the project (see package preview): HTTP/2 on its
// stdio, which reaches the servers listening in the container. It gets
// none of the env files (no secrets) and no detach keys, which would
// otherwise cut the binary stream.
func (m *Manager) BridgeArgv(p model.Project, userID string) []string {
	argv := append([]string{m.cfg.Podman}, m.global...)
	argv = append(argv, "exec", "-i", "--detach-keys=", Name(p.ID, userID), CollieBinPath, "port-bridge")
	if p.Allowlisted() {
		// The bridge to Collie's network proxy: not a server of the agent's.
		_, port, _ := strings.Cut(ProxyAddr, ":")
		argv = append(argv, "--skip", port)
	}
	return argv
}

// Running reports whether the user's container for the project is running.
func (m *Manager) Running(projectID, userID string) bool {
	if !m.available {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return m.inspect(ctx, Name(projectID, userID)).running
}

// Stale reports whether the user's container for the project kept an older
// configuration when it was last started (it was in use when that changed),
// so it may lack newer mounts.
func (m *Manager) Stale(projectID, userID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stale[Name(projectID, userID)]
}

// Exists reports whether the user has a container for the project (running
// or not).
func (m *Manager) Exists(projectID, userID string) bool {
	if !m.available {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return m.inspect(ctx, Name(projectID, userID)).exists
}

// ReadFile reads a file as the user's agents see it in their container for
// the project, with the agents' permissions. It fails for files larger
// than limit bytes.
func (m *Manager) ReadFile(projectID, userID, path string, limit int64) ([]byte, error) {
	out, err := m.readEnd(projectID, userID, "head", path, limit)
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > limit {
		return nil, fmt.Errorf("%s is larger than %d MB", path, limit>>20)
	}
	return out, nil
}

// TailFile reads the last limit bytes of a file as the user's agents see
// it in their container for the project; more reports that the file holds
// more before them.
func (m *Manager) TailFile(projectID, userID, path string, limit int64) (data []byte, more bool, err error) {
	out, err := m.readEnd(projectID, userID, "tail", path, limit)
	if err != nil {
		return nil, false, err
	}
	if int64(len(out)) > limit {
		return out[1:], true, nil
	}
	return out, false, nil
}

// readEnd runs head or tail in the container for up to limit+1 bytes of
// one end of a file, so the caller can tell whether there is more.
func (m *Manager) readEnd(projectID, userID, end, path string, limit int64) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("path must be absolute")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	args := append(append([]string(nil), m.global...), "exec", Name(projectID, userID),
		end, "-c", strconv.FormatInt(limit+1, 10), "--", path)
	cmd := exec.CommandContext(ctx, m.cfg.Podman, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(strings.TrimPrefix(msg, end+": "))
	}
	return out.Bytes(), nil
}

// containers lists collie's containers, optionally only one project's.
func (m *Manager) containers(ctx context.Context, projectID string) []ContainerInfo {
	args := []string{"ps", "-a", "--format", "json", "--filter", "label=collie.project"}
	if projectID != "" {
		args = append(args, "--filter", "label=collie.project="+projectID)
	}
	out, err := m.podman(ctx, args...)
	if err != nil {
		return nil
	}
	var raw []struct {
		Names  []string          `json:"Names"`
		State  string            `json:"State"`
		Labels map[string]string `json:"Labels"`
	}
	if json.Unmarshal([]byte(out), &raw) != nil {
		return nil
	}
	var res []ContainerInfo
	for _, c := range raw {
		ci := ContainerInfo{ProjectID: c.Labels["collie.project"], UserID: c.Labels["collie.user"], Running: c.State == "running", Exists: true}
		if len(c.Names) > 0 {
			ci.Name = c.Names[0]
		}
		res = append(res, ci)
	}
	return res
}

// Stop stops (or kills) one user's container of a project; with userID ""
// all of the project's containers.
func (m *Manager) Stop(projectID, userID string, kill bool) error {
	if !m.available {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var names []string
	if userID != "" {
		names = []string{Name(projectID, userID)}
	} else {
		for _, c := range m.containers(ctx, projectID) {
			if c.Running {
				names = append(names, c.Name)
			}
		}
	}
	var firstErr error
	for _, n := range names {
		var err error
		if kill {
			_, err = m.podman(ctx, "kill", n)
		} else {
			_, err = m.podman(ctx, "stop", "-t", "5", n)
		}
		if err != nil && !strings.Contains(err.Error(), "no such container") && !strings.Contains(err.Error(), "not running") && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Remove deletes the project's containers and per-project data.
func (m *Manager) Remove(projectID string) {
	if m.available {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		for _, c := range m.containers(ctx, projectID) {
			_, _ = m.podman(ctx, "rm", "-f", "-t", "3", c.Name)
		}
		cancel()
	}
	_ = os.RemoveAll(filepath.Join(m.cfg.DataDir, "homes", projectID))
	_ = os.RemoveAll(m.MemoryDir(projectID))
	_ = os.Remove(projectEnvPath(m.cfg.DataDir, projectID))
	_ = os.Remove(m.hostsPath(projectID))
	_ = os.Remove(m.ProjectPluginsPath(projectID))
	m.removeNested(filepath.Join(m.nestedDir(), projectID))
	m.egress.Close(projectID, "")
	_ = os.RemoveAll(filepath.Join(m.cfg.DataDir, "netsock", projectID))
}

// RemoveUser deletes a user's containers and data (not project files).
func (m *Manager) RemoveUser(userID string) {
	if m.available {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		for _, c := range m.containers(ctx, "") {
			if c.UserID == userID {
				_, _ = m.podman(ctx, "rm", "-f", "-t", "3", c.Name)
			}
		}
		cancel()
	}
	_ = os.RemoveAll(m.UserDir(userID))
	homes, _ := os.ReadDir(filepath.Join(m.cfg.DataDir, "homes"))
	for _, h := range homes {
		if h.IsDir() {
			_ = os.RemoveAll(filepath.Join(m.cfg.DataDir, "homes", h.Name(), userID))
		}
	}
	nested, _ := os.ReadDir(m.nestedDir())
	for _, n := range nested {
		if n.IsDir() {
			m.removeNested(m.NestedStorage(n.Name(), userID))
		}
	}
	m.egress.CloseUser(userID)
	socks, _ := os.ReadDir(filepath.Join(m.cfg.DataDir, "netsock"))
	for _, d := range socks {
		if d.IsDir() {
			_ = os.RemoveAll(m.netDir(d.Name(), userID))
		}
	}
}

type ContainerInfo struct {
	Name      string `json:"name"`
	ProjectID string `json:"projectId"`
	UserID    string `json:"userId"`
	Running   bool   `json:"running"`
	Exists    bool   `json:"exists"`
	Mem       string `json:"mem,omitempty"`
	CPU       string `json:"cpu,omitempty"`
	Pids      string `json:"pids,omitempty"`
	// LimitsEnforced is false when the host cannot apply resource limits
	// to rootless containers (no systemd user session / cgroup delegation).
	LimitsEnforced bool `json:"limitsEnforced"`
}

// Inspect reports the state and usage of the containers of a project (all
// users, or one).
func (m *Manager) Inspect(projectID, userID string) []ContainerInfo {
	if !m.available {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out []ContainerInfo
	for _, c := range m.containers(ctx, projectID) {
		if userID != "" && c.UserID != userID {
			continue
		}
		out = append(out, m.withStats(ctx, c))
	}
	return out
}

// All lists every collie container with usage (admin overview).
func (m *Manager) All() []ContainerInfo {
	if !m.available {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out []ContainerInfo
	for _, c := range m.containers(ctx, "") {
		out = append(out, m.withStats(ctx, c))
	}
	return out
}

func (m *Manager) withStats(ctx context.Context, c ContainerInfo) ContainerInfo {
	if !c.Running {
		return c
	}
	if out, err := m.podman(ctx, "exec", c.Name, "cat", "/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/pids.max"); err == nil {
		f := strings.Fields(out)
		c.LimitsEnforced = len(f) == 2 && (f[0] != "max" || f[1] != "max") && m.LimitsSupported()
	}
	if out, err := m.podman(ctx, "stats", "--no-stream", "--no-reset", "--format", "{{.MemUsage}}|{{.CPUPerc}}|{{.PIDs}}", c.Name); err == nil {
		f := strings.Split(strings.TrimSpace(out), "|")
		if len(f) == 3 {
			c.Mem, c.CPU, c.Pids = strings.TrimSpace(f[0]), strings.TrimSpace(f[1]), strings.TrimSpace(f[2])
		}
	}
	if !c.LimitsEnforced {
		// Without its own cgroup, podman reports the parent's usage (the
		// whole service or host); add up the container's processes instead.
		// CPU usage has no per-container figure there; leave it out.
		c.CPU, c.Pids = "", ""
		if out, err := m.podman(ctx, "exec", c.Name, "ps", "-eo", "rss="); err == nil {
			var kb int64
			procs := strings.Fields(out)
			for _, f := range procs {
				n, _ := strconv.ParseInt(f, 10, 64)
				kb += n
			}
			// minus the ps process itself
			c.Mem, c.Pids = fmt.Sprintf("%.1fMB", float64(kb)*1024/1e6), strconv.Itoa(max(len(procs)-1, 0))
		}
	}
	return c
}

// MigrateLayout converts data from the single-user version: per-project
// homes move under the given owner, the server user's Codex login is
// copied to the owner, and old-style containers are removed.
func (m *Manager) MigrateLayout(ownerID string) {
	homes := filepath.Join(m.cfg.DataDir, "homes")
	entries, _ := os.ReadDir(homes)
	for _, e := range entries {
		dir := filepath.Join(homes, e.Name())
		if !e.IsDir() {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, ".claude")); err != nil {
			continue // already per-user
		}
		m.repairOwnership(dir)
		tmp := dir + ".migrating"
		if err := os.Rename(dir, tmp); err != nil {
			log.Printf("migrate %s: %v", dir, err)
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err == nil {
			if err := os.Rename(tmp, filepath.Join(dir, ownerID)); err != nil {
				log.Printf("migrate %s: %v", dir, err)
			}
		}
		// The old per-project ~/.codex was replaced by the per-user one.
		_ = os.RemoveAll(filepath.Join(dir, ownerID, ".codex"))
	}
	if h, err := os.UserHomeDir(); err == nil {
		src := filepath.Join(h, ".codex", "auth.json")
		dst := filepath.Join(m.CodexDir(ownerID), "auth.json")
		if fileNonEmpty(src) && !fileNonEmpty(dst) {
			if err := os.MkdirAll(m.CodexDir(ownerID), 0o700); err == nil {
				if b, err := os.ReadFile(src); err == nil {
					_ = os.WriteFile(dst, b, 0o600)
				}
			}
		}
	}
	if m.available {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, c := range m.containers(ctx, "") {
			if c.UserID == "" {
				_, _ = m.podman(ctx, "rm", "-f", "-t", "3", c.Name)
			}
		}
	}
}
