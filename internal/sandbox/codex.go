package sandbox

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/tattsun/collie/internal/model"
)

// Each of a user's containers has a CODEX_HOME of its own (~/.codex in its
// home for the project): an agent can write everything there, and Codex
// runs what it finds (.env, config, rules), so sharing it would carry one
// project's agent into the user's other projects and Home. Only the login
// is shared, so that signing in once (`codex login` in any of them) works
// in all: ~/.codex/auth.json links to the user's codex-auth directory,
// which holds nothing else (Codex keeps its lock and temporary files next
// to the file a link points to).

// Where a container sees the user's Codex login directory, and what its
// ~/.codex/auth.json links to.
const (
	codexAuthMount = Home + "/.codex-auth"
	codexAuthLink  = codexAuthMount + "/auth.json"
)

// The most of a Codex login file Collie reads.
const maxCodexAuth = 1 << 20

// CodexAuthDir holds the user's Codex login (auth.json), mounted into all
// of their containers.
func (m *Manager) CodexAuthDir(userID string) string {
	return filepath.Join(m.UserDir(userID), "codex-auth")
}

// legacyCodexDir is the CODEX_HOME that all of a user's containers shared
// before. MigrateCodex moves the login and threads out of it; it is not
// mounted anywhere any more.
func (m *Manager) legacyCodexDir(userID string) string {
	return filepath.Join(m.UserDir(userID), "codex")
}

// CodexLoggedIn reports whether the user has a Codex login.
func (m *Manager) CodexLoggedIn(userID string) bool {
	root, err := os.OpenRoot(m.CodexAuthDir(userID))
	if err != nil {
		return false
	}
	defer root.Close()
	b, err := readRegular(root, "auth.json", maxCodexAuth)
	return err == nil && len(strings.TrimSpace(string(b))) > 2
}

// CodexLogout removes the user's Codex login, for all of their projects at
// once. (`codex logout` in a container only removes that container's link
// to it, which Collie makes again.)
func (m *Manager) CodexLogout(userID string) error {
	root, err := os.OpenRoot(m.CodexAuthDir(userID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Remove("auth.json"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// linkCodexAuth makes ~/.codex/auth.json in a container home (root) the
// link to the user's Codex login, whatever the agent left there.
func (m *Manager) linkCodexAuth(home *os.Root, userID string) error {
	const name = ".codex/auth.json"
	if err := home.MkdirAll(".codex", 0o700); err != nil {
		return err
	}
	fi, err := home.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	case fi.Mode()&fs.ModeSymlink != 0:
		if t, err := home.Readlink(name); err == nil && t == codexAuthLink {
			return nil
		}
	case fi.Mode().IsRegular():
		// `codex logout` removes the link itself, so a later `codex login`
		// in this container writes a file of its own: the newest login is
		// the user's.
		m.adoptCodexLogin(home, name, fi.ModTime(), userID)
	}
	if err == nil {
		if err := home.RemoveAll(name); err != nil {
			return err
		}
	}
	return home.Symlink(codexAuthLink, name)
}

// adoptCodexLogin makes the login file name in a container home the user's
// login if it is newer than theirs.
func (m *Manager) adoptCodexLogin(home *os.Root, name string, mtime time.Time, userID string) {
	b, err := readRegular(home, name, maxCodexAuth)
	if err != nil || !json.Valid(b) || len(strings.TrimSpace(string(b))) <= 2 {
		return
	}
	dir := m.CodexAuthDir(userID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return
	}
	defer root.Close()
	if cur, err := root.Lstat("auth.json"); err == nil && cur.Mode().IsRegular() && !cur.ModTime().Before(mtime) {
		return
	}
	if err := writeBytesInto(root, "auth.json", b); err != nil {
		log.Printf("codex login of %s: %v", userID, err)
	}
}

// A marker in a legacy CODEX_HOME that MigrateCodex has been through it.
const codexMigrated = ".collie-migrated"

// MigrateCodex moves what matters out of the CODEX_HOME that all of a
// user's containers used to share: the login to their codex-auth
// directory, and each thread to the CODEX_HOME of the project it ran in
// (by its working directory), so its session still resumes there. The rest
// (prompt history, memories, settings) stays behind, mounted nowhere. The
// old directory was the agents' to write, so it is read through os.Root,
// regular files only.
func (m *Manager) MigrateCodex(projects []model.Project) {
	users, _ := os.ReadDir(filepath.Join(m.cfg.DataDir, "users"))
	for _, u := range users {
		if !u.IsDir() {
			continue
		}
		uid := u.Name()
		dir := m.legacyCodexDir(uid)
		if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, codexMigrated)); err == nil {
			continue
		}
		m.repairOwnership(dir)
		root, err := os.OpenRoot(dir)
		if err != nil {
			log.Printf("migrate %s: %v", dir, err)
			continue
		}
		m.migrateCodexLogin(root, uid)
		m.migrateCodexThreads(root, uid, projects)
		if f, err := root.OpenFile(codexMigrated, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
			f.Close()
		}
		root.Close()
	}
}

func (m *Manager) migrateCodexLogin(old *os.Root, userID string) {
	b, err := readRegular(old, "auth.json", maxCodexAuth)
	if err != nil {
		return
	}
	dir := m.CodexAuthDir(userID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("migrate codex login of %s: %v", userID, err)
		return
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return
	}
	defer root.Close()
	if _, err := root.Lstat("auth.json"); errors.Is(err, fs.ErrNotExist) {
		if err := writeBytesInto(root, "auth.json", b); err != nil {
			log.Printf("migrate codex login of %s: %v", userID, err)
			return
		}
	}
	_ = old.Remove("auth.json")
}

// The most of a thread's first line (its session_meta, with Codex's base
// instructions in it) read to learn where it ran.
const maxRolloutMeta = 4 << 20

func (m *Manager) migrateCodexThreads(old *os.Root, userID string, projects []model.Project) {
	homes := map[string]*os.Root{}
	defer func() {
		for _, r := range homes {
			r.Close()
		}
	}()
	for _, top := range []string{"sessions", "archived_sessions"} {
		_ = fs.WalkDir(old.FS(), top, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !d.Type().IsRegular() || !strings.HasSuffix(p, ".jsonl") {
				return nil
			}
			proj, ok := m.projectAt(projects, rolloutCwd(old, p))
			if !ok {
				return nil
			}
			home := homes[proj.ID]
			if home == nil {
				dir := m.HomeDir(proj.ID, userID)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return nil
				}
				if home, err = os.OpenRoot(dir); err != nil {
					return nil
				}
				homes[proj.ID] = home
			}
			dst := path.Join(".codex", p)
			if _, err := home.Lstat(dst); err == nil {
				return nil
			}
			f, _, err := openRegular(old, p)
			if err != nil {
				return nil
			}
			err = writeInto(home, dst, f)
			f.Close()
			if err != nil {
				log.Printf("migrate codex thread %s: %v", p, err)
				return nil
			}
			_ = old.Remove(p)
			return nil
		})
	}
}

// rolloutCwd is the working directory a Codex thread file says it ran in
// ("" if it can't tell).
func rolloutCwd(root *os.Root, name string) string {
	f, _, err := openRegular(root, name)
	if err != nil {
		return ""
	}
	defer f.Close()
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(io.LimitReader(f, maxRolloutMeta)).Decode(&meta); err != nil || meta.Type != "session_meta" {
		return ""
	}
	return meta.Payload.Cwd
}

// projectAt is the project whose folder or worktrees hold dir.
func (m *Manager) projectAt(projects []model.Project, dir string) (model.Project, bool) {
	if dir == "" || !filepath.IsAbs(dir) {
		return model.Project{}, false
	}
	dir = filepath.Clean(dir)
	for _, p := range projects {
		if p.Path != "" && within(dir, p.Path) || within(dir, m.WorktreeRoot(p.ID)) {
			return p, true
		}
	}
	return model.Project{}, false
}
