// Package gitutil wraps the handful of git operations collie needs.
package gitutil

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

func run(ctx context.Context, dir string, args ...string) (string, error) {
	return runEnv(ctx, dir, nil, args...)
}

func runEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"), env...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("git %s: %s", command(args), msg)
	}
	return out.String(), nil
}

// command is the git command in args, after any -c options.
func command(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		return args[i]
	}
	return ""
}

func ctx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// guarded is git that only reads the working tree (status, diff) or
// removes a worktree, kept from running what the repository's
// configuration names: an agent can write it, and in a group project
// another member's container runs this for the Changes tab. No fsmonitor,
// hooks or external diff (callers add --no-ext-diff --no-textconv to
// diffs, and keep out of submodules, whose configuration isn't looked at
// here), and every filter driver the configuration defines is emptied: git
// has no switch for them all. The emptied drivers go both as -c options
// and as GIT_CONFIG_KEY_n variables, which also take names with a "=" in
// them.
type guarded struct {
	args []string
	env  []string
}

func guard(c context.Context, dir string) guarded {
	g := guarded{args: []string{
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "diff.external=",
		"-c", "submodule.recurse=false",
		// Showing signatures runs gpg.program (or its ssh/x509 variants).
		"-c", "log.showSignature=false",
	}}
	// Reading the configuration runs nothing.
	out, _ := run(c, dir, "config", "-z", "--name-only", "--get-regexp", `^filter\.`)
	seen := map[string]bool{}
	n := 0
	for _, key := range strings.Split(out, "\x00") {
		i := strings.LastIndexByte(key, '.')
		if i <= len("filter.") {
			continue
		}
		drv := key[:i]
		if seen[drv] {
			continue
		}
		seen[drv] = true
		for _, kv := range [][2]string{{".clean", ""}, {".smudge", ""}, {".process", ""}, {".required", "false"}} {
			if !strings.Contains(drv, "=") {
				g.args = append(g.args, "-c", drv+kv[0]+"="+kv[1])
			}
			g.env = append(g.env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s%s", n, drv, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n, kv[1]))
			n++
		}
	}
	if n > 0 {
		g.env = append(g.env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", n))
	}
	return g
}

func (g guarded) run(c context.Context, dir string, args ...string) (string, error) {
	return runEnv(c, dir, g.env, append(slices.Clone(g.args), args...)...)
}

func IsRepo(dir string) bool {
	c, cancel := ctx(5 * time.Second)
	defer cancel()
	out, err := run(c, dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

func HasCommits(dir string) bool {
	c, cancel := ctx(5 * time.Second)
	defer cancel()
	_, err := run(c, dir, "rev-parse", "--verify", "-q", "HEAD")
	return err == nil
}

// HasFiles reports whether the working tree holds anything git would track:
// files that are committed, staged or untracked (ignored ones don't count).
// It errs on the side of true.
func HasFiles(dir string) bool {
	c, cancel := ctx(30 * time.Second)
	defer cancel()
	out, err := guard(c, dir).run(c, dir, "status", "--porcelain", "--untracked-files=normal", "--ignore-submodules=dirty")
	return err != nil || strings.TrimSpace(out) != ""
}

// HasIdentity reports whether git knows who to commit as in dir.
func HasIdentity(dir string) bool {
	c, cancel := ctx(5 * time.Second)
	defer cancel()
	_, err := run(c, dir, "var", "GIT_COMMITTER_IDENT")
	return err == nil
}

// ConfigValue reads key from a git config file ("" if unset).
func ConfigValue(file, key string) string {
	c, cancel := ctx(5 * time.Second)
	defer cancel()
	out, err := run(c, filepath.Dir(file), "config", "--file", file, "--get", key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// InitialCommit gives a repository with no commits an empty root commit on
// its current branch, so there is something to branch off. The index and
// working tree are left alone. name and email author it if given (else
// git's configured identity does). Losing a race to another caller is fine.
func InitialCommit(dir, name, email string) error {
	c, cancel := ctx(30 * time.Second)
	defer cancel()
	tree, err := run(c, dir, "mktree") // empty stdin: the empty tree
	if err != nil {
		return err
	}
	var env []string
	if name != "" && email != "" {
		env = []string{"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email, "GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email}
	}
	commit, err := runEnv(c, dir, env, "commit-tree", strings.TrimSpace(tree), "-m", "Initial commit")
	if err != nil {
		return err
	}
	// The empty old value: only while the branch is still unborn.
	_, err = run(c, dir, "update-ref", "-m", "commit (initial): Initial commit", "HEAD", strings.TrimSpace(commit), "")
	if err != nil && HasCommits(dir) {
		return nil
	}
	return err
}

func Init(dir string) error {
	c, cancel := ctx(30 * time.Second)
	defer cancel()
	_, err := run(c, dir, "init", "-q")
	return err
}

// Clone clones url into dest on the server, with the server user's git
// configuration and credentials (admins only; members clone inside their
// container).
func Clone(url, dest string) error {
	c, cancel := ctx(30 * time.Minute)
	defer cancel()
	// After "--", neither can be taken for an option (--upload-pack=…).
	args := []string{"clone", "--quiet", "--", url, dest}
	_, err := run(c, filepath.Dir(dest), args...)
	return err
}

func CurrentBranch(dir string) string {
	c, cancel := ctx(5 * time.Second)
	defer cancel()
	out, err := run(c, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// FreshBase returns the commit a new worktree should start from: local HEAD,
// unless HEAD is strictly behind the current branch's upstream or, failing
// that, the remote's default branch (e.g. the project sits on an old feature
// branch with no usable upstream). In that case the newer tip is used so
// agents don't start from stale code. It fetches first, best effort: offline,
// no credentials or no remote just fall back to the refs already known.
// Unpushed or diverged local commits keep HEAD, and neither the local branch
// nor its checkout is ever modified.
func FreshBase(repo string) string {
	head := HeadCommit(repo)
	c, cancel := ctx(20 * time.Second)
	defer cancel()
	var refs []string
	if up, err := run(c, repo, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); err == nil {
		refs = append(refs, strings.TrimSpace(up))
	}
	if def, err := run(c, repo, "symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); err == nil {
		refs = append(refs, strings.TrimSpace(def))
	}
	fetched := map[string]bool{}
	for _, ref := range refs {
		if remote, _, ok := strings.Cut(ref, "/"); ok && remote != "" && !fetched[remote] {
			fetched[remote] = true
			_, _ = run(c, repo, "fetch", "-q", remote)
		}
		tip, err := run(c, repo, "rev-parse", "--verify", "-q", ref+"^{commit}")
		if err != nil {
			continue
		}
		tip = strings.TrimSpace(tip)
		if tip == head {
			return head
		}
		// Strictly ahead of HEAD: head is an ancestor of tip.
		if _, err := run(c, repo, "merge-base", "--is-ancestor", head, tip); err == nil {
			return tip
		}
	}
	return head
}

// AddWorktree creates a new branch at base (a commit; "" means HEAD) checked
// out in path.
func AddWorktree(repo, path, branch, base string) error {
	if base == "" {
		base = "HEAD"
	}
	c, cancel := ctx(2 * time.Minute)
	defer cancel()
	// Hooks and filters run as the repository wants (Git LFS needs them):
	// the session's own agent works there next anyway.
	_, err := run(c, repo, "-c", "core.fsmonitor=false", "worktree", "add", "-q", "--no-track", "-b", branch, path, base)
	return err
}

func RemoveWorktree(repo, path string) error {
	c, cancel := ctx(time.Minute)
	defer cancel()
	g := guard(c, repo)
	_, err := g.run(c, repo, "worktree", "remove", "--force", path)
	if err != nil {
		_ = os.RemoveAll(path)
		_, _ = g.run(c, repo, "worktree", "prune")
	}
	return err
}

type FileChange struct {
	Path   string `json:"path"`
	Status string `json:"status"` // M, A, D, R, ?, ...
	Diff   string `json:"diff"`
	Binary bool   `json:"binary,omitempty"`
}

type Changes struct {
	Repo    bool         `json:"repo"`
	Branch  string       `json:"branch,omitempty"`
	Files   []FileChange `json:"files"`
	Commits []string     `json:"commits,omitempty"`
	Base    string       `json:"base,omitempty"`
}

// What Diff shows of one file, and of all of them.
const (
	maxDiff      = 256 << 10
	maxDiffTotal = 8 << 20
)

// Diff returns working-tree changes against HEAD, including untracked
// files, and (for worktree branches) the commits made on top of base.
func Diff(dir, base string) (*Changes, error) {
	res := &Changes{Files: []FileChange{}}
	if !IsRepo(dir) {
		return res, nil
	}
	res.Repo = true
	res.Branch = CurrentBranch(dir)
	c, cancel := ctx(30 * time.Second)
	defer cancel()

	g := guard(c, dir)
	status, err := g.run(c, dir, "status", "--porcelain=v1", "-z", "-uall", "--ignore-submodules=dirty")
	if err != nil {
		return nil, err
	}
	hasHead := HasCommits(dir)
	// For a session branch, show everything it changed since it was forked
	// (committed or not); otherwise the uncommitted changes.
	against := "HEAD"
	if base != "" && hasHead {
		if _, err := run(c, dir, "cat-file", "-e", base+"^{commit}"); err == nil {
			against = base
			if out, err := g.run(c, dir, "diff", "--no-ext-diff", "--no-textconv", "--name-status", "-z", base, "HEAD"); err == nil {
				status = mergeCommitted(status, out)
			}
		}
	}
	// Untracked files are read through root: an agent may have swapped
	// one for a symlink (or a FIFO) since git listed it.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	total := 0
	entries := strings.Split(status, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		code, path := e[:2], e[3:]
		if code[0] == 'R' || code[0] == 'C' {
			i++ // skip the source path
		}
		fc := FileChange{Path: path, Status: strings.TrimSpace(code)}
		if code == "??" {
			fc.Status = "?"
			fc.Diff, fc.Binary = untrackedDiff(root, path)
		} else if hasHead {
			d, _ := g.run(c, dir, "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--ignore-submodules=dirty", against, "--", path)
			fc.Diff = d
			fc.Binary = strings.Contains(d, "Binary files")
		} else {
			d, _ := g.run(c, dir, "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--ignore-submodules=dirty", "--cached", "--", path)
			fc.Diff = d
		}
		if len(fc.Diff) > maxDiff {
			fc.Diff = fc.Diff[:maxDiff] + "\n… diff truncated …\n"
		}
		if total += len(fc.Diff); total > maxDiffTotal {
			fc.Diff = "… diff left out: too many changes to show …\n"
		}
		res.Files = append(res.Files, fc)
	}
	if base != "" && hasHead {
		res.Base = base
		if out, err := g.run(c, dir, "log", "--oneline", "--no-decorate", "--no-show-signature", base+"..HEAD"); err == nil {
			for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
				if l != "" {
					res.Commits = append(res.Commits, l)
				}
			}
		}
	}
	return res, nil
}

// mergeCommitted adds files changed by commits (git diff --name-status -z
// output) to a porcelain -z status listing, skipping ones already present.
func mergeCommitted(status, nameStatus string) string {
	seen := map[string]bool{}
	for _, e := range strings.Split(status, "\x00") {
		if len(e) > 3 {
			seen[e[3:]] = true
		}
	}
	f := strings.Split(nameStatus, "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		code := f[i]
		path := f[i+1]
		if code != "" && (code[0] == 'R' || code[0] == 'C') && i+2 < len(f) {
			path = f[i+2]
			i++
		}
		if code == "" || seen[path] {
			continue
		}
		seen[path] = true
		status += code[:1] + "  " + path + "\x00"
	}
	return status
}

// untrackedDiff shows an untracked file as added. A symlink shows as its
// target, as git shows committed ones.
func untrackedDiff(root *os.Root, path string) (string, bool) {
	st, err := root.Lstat(path)
	if err != nil {
		return "", false
	}
	var b []byte
	switch {
	case st.Mode()&fs.ModeSymlink != 0:
		target, err := root.Readlink(path)
		if err != nil {
			return "", false
		}
		b = []byte(target + "\n")
	case st.Mode().IsRegular():
		if st.Size() > maxDiff {
			return fmt.Sprintf("new file, %d bytes (too large to show)\n", st.Size()), false
		}
		// Not following a symlink, and not waiting on a FIFO, in case it
		// was swapped for one since the Lstat.
		f, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return "", false
		}
		defer f.Close()
		if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
			return "", false
		}
		if b, err = io.ReadAll(io.LimitReader(f, maxDiff)); err != nil {
			return "", false
		}
	default:
		return "", false
	}
	if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
		return "", true
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- /dev/null\n+++ b/%s\n", path)
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	fmt.Fprintf(&sb, "@@ -0,0 +1,%d @@\n", len(lines))
	for _, l := range lines {
		sb.WriteString("+" + l + "\n")
	}
	return sb.String(), false
}

// CommonDir returns the absolute path of the repository's shared .git
// directory (for a linked worktree this lives in the main checkout).
func CommonDir(dir string) string {
	c, cancel := ctx(5 * time.Second)
	defer cancel()
	out, err := run(c, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// HeadCommit returns the full sha of HEAD.
func HeadCommit(dir string) string {
	c, cancel := ctx(5 * time.Second)
	defer cancel()
	out, err := run(c, dir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
