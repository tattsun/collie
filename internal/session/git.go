package session

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tattsun/collie/internal/gitutil"
	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
)

// git is how to run git on a project's files for a user: on the server for
// a host-run project, whose agents run there anyway; in the user's
// container for an isolated one, where the agents can rewrite the
// repository's hooks and configuration (see gitutil.Ops). With create
// unset, a container that doesn't exist yet isn't made for it.
func (m *Manager) git(p model.Project, uid string, create bool) gitutil.Ops {
	if !p.Isolated() {
		return gitutil.Local{}
	}
	if m.gitHelper != nil {
		return m.gitHelper(p, uid)
	}
	return gitutil.Helper{Run: func(timeout time.Duration, req []byte) ([]byte, error) {
		return m.runGitHelper(p, uid, create, timeout, req)
	}}
}

// What the server takes from `collie git-helper` at most (Diff keeps its
// output well below).
const maxHelperOutput = 64 << 20

// runGitHelper runs `collie git-helper` in the user's container for the
// project, starting the container if it isn't running (stopped as idle or
// by a restart of Collie, only created, or, with create, not there yet).
func (m *Manager) runGitHelper(p model.Project, uid string, create bool, timeout time.Duration, req []byte) ([]byte, error) {
	if m.sb == nil || !m.sb.Available() {
		return nil, errors.New("this project runs isolated, but podman is not installed on the server")
	}
	run := func() ([]byte, error) {
		argv, kill := m.sb.Exec(p.ID, uid, sandbox.Home, "git-"+model.NewID(), false, nil,
			[]string{sandbox.CollieBinPath, "git-helper"})
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, m.sb.Podman(), argv[1:]...)
		cmd.Stdin = bytes.NewReader(req)
		out := &cappedBuffer{max: maxHelperOutput}
		errb := &cappedBuffer{max: 16 << 10}
		cmd.Stdout, cmd.Stderr = out, errb
		err := cmd.Run()
		if ctx.Err() != nil {
			kill("KILL")
			return nil, errors.New("git in the project's container took too long")
		}
		if out.full {
			return nil, errors.New("git in the project's container answered with too much")
		}
		if err != nil {
			msg := strings.TrimSpace(errb.String())
			if msg == "" {
				msg = err.Error()
			}
			return nil, &helperError{msg: msg}
		}
		return out.Bytes(), nil
	}
	out, err := run()
	var he *helperError
	if errors.As(err, &he) {
		// Podman's exit code doesn't tell its own failures from the
		// helper's (it passes the helper's on), so ask about the container.
		exists, running := m.sb.State(p.ID, uid)
		switch {
		case running && !sandbox.ExecSaysNotRunning(he.msg):
			return nil, err // the helper's own failure (git's come in its answer)
		case running:
			// Started since (by an agent, or another call like this one).
		case !exists && !create:
			return nil, err
		default:
			if _, err := m.sb.WaitImage(p.Image, 0, nil); err != nil {
				return nil, err
			}
			if err := m.ensureContainer(p, uid, nil); err != nil {
				return nil, err
			}
		}
		out, err = run()
	}
	if err == nil {
		m.ctrMu.Lock()
		m.ctrActive[p.ID+"/"+uid] = time.Now()
		m.ctrMu.Unlock()
	}
	return out, err
}

// helperError is podman's or the helper's failure, with what it printed.
type helperError struct{ msg string }

func (e *helperError) Error() string { return "git in the project's container: " + e.msg }

// cappedBuffer keeps up to max bytes and then fails the writes, which
// stops the process writing.
type cappedBuffer struct {
	bytes.Buffer
	max  int
	full bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		b.full = true
		return 0, io.ErrShortWrite
	}
	return b.Buffer.Write(p)
}

// removeWorktree removes a session's worktree (keeping its branch). For
// an isolated project, git does that in the owner's container when there
// is one; otherwise, or if that fails, the server deletes the folder
// itself (it is Collie's own, under the project's worktree root) without
// running git, and git forgets it with the next `git worktree prune`.
func (m *Manager) removeWorktree(p model.Project, s model.Session) {
	err := m.git(p, s.Owner, false).RemoveWorktree(p.Path, s.Cwd)
	if err == nil {
		return
	}
	log.Printf("remove worktree %s: %v", s.Cwd, err)
	if !p.Isolated() || m.sb == nil {
		return
	}
	root := m.sb.WorktreeRoot(p.ID)
	if rel, rerr := filepath.Rel(root, s.Cwd); rerr != nil || rel == "." || strings.Contains(rel, string(filepath.Separator)) || strings.HasPrefix(rel, "..") {
		return
	}
	// RemoveAll doesn't follow symlinks, and root itself (the mount's
	// source) is out of the agents' reach.
	if err := os.RemoveAll(s.Cwd); err != nil {
		log.Printf("remove worktree %s: %v", s.Cwd, err)
	}
}
