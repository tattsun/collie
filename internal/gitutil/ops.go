package gitutil

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

// Ops are the git operations Collie runs on a project's files for its
// sessions. Local runs git on this machine. Helper runs it in the
// project's container instead: an agent there can write everything in the
// repository, .git/config and .git/hooks included, and git runs what those
// name (hooks, core.fsmonitor, filters, diff drivers, ssh commands …), so
// git on the server must never touch a container's repositories.
type Ops interface {
	// CheckWorktrees says why sessions in the repository can't get a
	// worktree of their own (nil if they can).
	CheckWorktrees(repo string) error
	// NewWorktree checks out a new branch in path for a session, from
	// the freshest base (FreshBase). A repository without commits first
	// gets an empty one, authored by author. It returns the base.
	NewWorktree(repo, path, branch string, author Author) (base string, err error)
	RemoveWorktree(repo, path string) error
	Diff(dir, base string) (*Changes, error)
}

// Author authors the empty initial commit of NewWorktree: git's own
// identity in the repository if it has one and PreferGit is set, else Name
// and Email.
type Author struct {
	Name      string `json:"name"`
	Email     string `json:"email"`
	PreferGit bool   `json:"preferGit,omitempty"`
}

var (
	ErrNoRepo    = errors.New("an isolated worktree needs a git repository")
	ErrNoCommits = errors.New("an isolated worktree starts from the latest commit, and this repository has files but no commits yet: commit them first")
)

// CheckWorktrees implements Ops.CheckWorktrees with git on this machine. A
// repository without commits (a new project, a clone of an empty
// repository) is fine while it holds no files: NewWorktree gives it an
// empty initial commit to branch off. With files, a worktree from that
// commit would leave them out.
func CheckWorktrees(repo string) error {
	if !IsRepo(repo) {
		return ErrNoRepo
	}
	if !HasCommits(repo) && HasFiles(repo) {
		return ErrNoCommits
	}
	return nil
}

// HasGitDir reports whether dir has a .git of its own, without running
// git: for repositories whose configuration can't be trusted.
func HasGitDir(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// HeadBranch reads the branch checked out in dir from its .git/HEAD,
// without running git (see HasGitDir): "HEAD" when detached, "" if dir
// has no .git directory.
func HeadBranch(dir string) string {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return ""
	}
	defer root.Close()
	f, err := root.OpenFile(filepath.Join(".git", "HEAD"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(f, 512))
	head := strings.TrimSpace(string(b))
	if isCommitID(head) {
		return "HEAD"
	}
	branch, ok := strings.CutPrefix(head, "ref: refs/heads/")
	if !ok || len(branch) > 255 || !utf8.ValidString(branch) || strings.ContainsAny(branch, " \t\n\r") {
		return ""
	}
	return branch
}

// Local runs Ops with git on this machine.
type Local struct{}

func (Local) CheckWorktrees(repo string) error { return CheckWorktrees(repo) }

func (Local) NewWorktree(repo, path, branch string, author Author) (string, error) {
	if err := CheckWorktrees(repo); err != nil {
		return "", err
	}
	if !HasCommits(repo) {
		name, email := author.Name, author.Email
		if author.PreferGit && HasIdentity(repo) {
			name, email = "", ""
		}
		if err := InitialCommit(repo, name, email); err != nil {
			return "", err
		}
	}
	base := FreshBase(repo)
	return base, AddWorktree(repo, path, branch, base)
}

func (Local) RemoveWorktree(repo, path string) error  { return RemoveWorktree(repo, path) }
func (Local) Diff(dir, base string) (*Changes, error) { return Diff(dir, base) }
