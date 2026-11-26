// Package gitutil wraps the handful of git operations collie needs.
package gitutil

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

func run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("git %s: %s", args[0], msg)
	}
	return out.String(), nil
}

func ctx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
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

func Init(dir string) error {
	c, cancel := ctx(30 * time.Second)
	defer cancel()
	_, err := run(c, dir, "init", "-q")
	return err
}

// CloneOpts restricts a clone made on behalf of a non-admin user: only
// remote HTTPS is allowed and none of the server user's credentials, SSH
// keys or git configuration are used. GitHubToken (the user's own), if set,
// authenticates github.com.
type CloneOpts struct {
	Restricted  bool
	GitHubToken string
}

func Clone(url, dest string, o CloneOpts) error {
	c, cancel := ctx(30 * time.Minute)
	defer cancel()
	cmd := exec.CommandContext(c, "git", "clone", "--quiet", "--", url, dest)
	cmd.Dir = filepath.Dir(dest)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	if o.Restricted {
		tmp, err := os.MkdirTemp("", "collie-clone-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		cfg := [][2]string{
			{"protocol.allow", "never"},
			{"protocol.https.allow", "always"},
			{"credential.helper", ""},
		}
		if o.GitHubToken != "" {
			auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + o.GitHubToken))
			cfg = append(cfg, [2]string{"http.https://github.com/.extraHeader", "Authorization: Basic " + auth})
		}
		cmd.Env = append(cmd.Env, "HOME="+tmp, "XDG_CONFIG_HOME="+tmp, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_SSH_COMMAND=false", "GIT_ASKPASS=/bin/false", "SSH_ASKPASS=/bin/false", "SSH_AUTH_SOCK=",
			fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(cfg)))
		for i, kv := range cfg {
			cmd.Env = append(cmd.Env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
		}
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("git clone: %s", msg)
	}
	return nil
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

// AddWorktree creates a new branch at HEAD checked out in path.
func AddWorktree(repo, path, branch string) error {
	c, cancel := ctx(2 * time.Minute)
	defer cancel()
	_, err := run(c, repo, "worktree", "add", "-q", "-b", branch, path, "HEAD")
	return err
}

func RemoveWorktree(repo, path string) error {
	c, cancel := ctx(time.Minute)
	defer cancel()
	_, err := run(c, repo, "worktree", "remove", "--force", path)
	if err != nil {
		_ = os.RemoveAll(path)
		_, _ = run(c, repo, "worktree", "prune")
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

const maxDiff = 256 << 10

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

	status, err := run(c, dir, "status", "--porcelain=v1", "-z", "-uall")
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
			if out, err := run(c, dir, "diff", "--name-status", "-z", base, "HEAD"); err == nil {
				status = mergeCommitted(status, out)
			}
		}
	}
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
			fc.Diff, fc.Binary = untrackedDiff(dir, path)
		} else if hasHead {
			d, _ := run(c, dir, "diff", "--no-color", "--no-ext-diff", against, "--", path)
			fc.Diff = d
			fc.Binary = strings.Contains(d, "Binary files")
		} else {
			d, _ := run(c, dir, "diff", "--no-color", "--cached", "--", path)
			fc.Diff = d
		}
		if len(fc.Diff) > maxDiff {
			fc.Diff = fc.Diff[:maxDiff] + "\n… diff truncated …\n"
		}
		res.Files = append(res.Files, fc)
	}
	if base != "" && hasHead {
		res.Base = base
		if out, err := run(c, dir, "log", "--oneline", "--no-decorate", base+"..HEAD"); err == nil {
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

func untrackedDiff(dir, path string) (string, bool) {
	full := filepath.Join(dir, path)
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		return "", false
	}
	if st.Size() > maxDiff {
		return fmt.Sprintf("new file, %d bytes (too large to show)\n", st.Size()), false
	}
	b, err := os.ReadFile(full)
	if err != nil {
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
