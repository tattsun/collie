package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tattsun/collie/internal/gitutil"
	"github.com/tattsun/collie/internal/model"
)

// A project made from a repository is registered at once and cloned in the
// background. An isolated project's clone runs in the container of whoever
// asked for it, like their agents there: the same network policy, their
// own GH_TOKEN and none of the server's credentials, whether they are an
// admin or not. Only a host-run project (admins only) is cloned on the
// server, with the collie user's git setup. A failed clone leaves the
// project registered, with the reason, until it's cloned again or removed.

var (
	errCloning       = errors.New("project is still being cloned")
	errCloneFailed   = errors.New("this project's clone failed: open the project to clone it again, or remove it")
	errNoFailedClone = errors.New("this project has no failed clone to retry")
)

// cloneInterrupted is the reason of a clone that was running when Collie
// stopped.
const cloneInterrupted = "Collie stopped before the clone finished"

// hostClone clones on the server (a variable for tests).
var hostClone = gitutil.Clone

// checkCloneURL checks a URL to clone. It must name a remote repository
// (remoteURL), and in an isolated project be https://, for admins too:
// that clone runs in a container, which has neither the server's SSH keys
// nor its git setup, and git:// or http:// would let anyone on the way
// change the code its agents then run with the user's tokens.
func checkCloneURL(raw string, isolated bool) error {
	if raw == "" {
		return errors.New("repository URL is required")
	}
	if strings.HasPrefix(raw, "-") || strings.HasPrefix(raw, "file:") || strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, ".") || strings.Contains(raw, "::") || !remoteURL(raw) {
		return errors.New("only remote repository URLs can be cloned (https://, ssh://, git:// or user@host:path)")
	}
	if isolated && !strings.HasPrefix(raw, "https://") {
		return errors.New("use an https:// URL (private GitHub repositories work with GH_TOKEN in your account environment)")
	}
	return nil
}

// redactURL is the URL without credentials, to keep and show: http(s) and
// git:// lose their whole user part (a token can stand in for the user
// name), ssh:// its password. The scp-like user@host:path has none.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil || u.Scheme == "" {
		return raw
	}
	if u.Scheme == "ssh" {
		if _, ok := u.User.Password(); !ok {
			return raw
		}
		u.User = url.User(u.User.Username())
	} else {
		u.User = nil
	}
	return u.String()
}

// urlSecrets are the credentials in a clone URL, as written and decoded.
func urlSecrets(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return nil
	}
	out := []string{u.User.String(), u.User.Username()}
	if pw, ok := u.User.Password(); ok {
		out = append(out, pw)
		if _, enc, ok := strings.Cut(u.User.String(), ":"); ok {
			out = append(out, enc)
		}
	}
	return out
}

var (
	// The user part of any URL in a message.
	urlUserRe = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s@'"]+@`)
	// GitHub's and GitLab's tokens.
	tokenRe = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,}|glpat-[A-Za-z0-9_-]{16,})`)
	// Server environment variables whose values are hidden from host-run
	// clones' messages.
	secretNameRe = regexp.MustCompile(`(?i)token|secret|passw|key|auth|cred`)
)

// minSecret: shorter values aren't hidden (they would hide words all over
// the message, and hardly are secrets).
const minSecret = 6

// scrub takes secrets out of a message that everyone who sees the project
// reads: each of secrets, the user part of any URL, and anything that looks
// like a GitHub or GitLab token.
func scrub(msg string, secrets []string) string {
	s := append([]string(nil), secrets...)
	// Longest first, so that a secret containing another goes whole.
	sort.Slice(s, func(i, j int) bool { return len(s[i]) > len(s[j]) })
	for _, v := range s {
		if len(v) >= minSecret {
			msg = strings.ReplaceAll(msg, v, "***")
		}
	}
	msg = urlUserRe.ReplaceAllString(msg, "${1}***@")
	return tokenRe.ReplaceAllString(msg, "***")
}

// serverSecrets are the values of the server's environment variables that
// look like secrets, which a host-run clone's git sees.
func serverSecrets() []string {
	var out []string
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && secretNameRe.MatchString(k) {
			out = append(out, v)
		}
	}
	return out
}

// lastLines keeps the end of a long message (git's reason comes last).
func lastLines(msg string, n int) string {
	msg = strings.TrimSpace(msg)
	if lines := strings.Split(msg, "\n"); len(lines) > n {
		msg = strings.Join(lines[len(lines)-n:], "\n")
	}
	return msg
}

// cloneFolder makes sure the project's folder exists and is empty, to
// clone into. A failed clone leaves it so (git removes what it made), and
// the container of an isolated project mounts it, so it is never removed.
func cloneFolder(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	if _, err := d.Readdirnames(1); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("%s is not empty: empty it (in the project's Terminal, say), then clone again", path)
	}
	return nil
}

// startClone clones rawURL into the (empty) folder of the stored project
// id in the background, as uid, and records how it went. The project is
// read from the store, never taken from a request.
func (m *Manager) startClone(id, uid, rawURL string) {
	go func() {
		p, ok := m.st.Project(id)
		if !ok {
			return // removed meanwhile
		}
		err := m.clone(p, uid, rawURL)
		np, ok := m.st.UpdateProject(id, func(x *model.Project) {
			if err != nil {
				x.Status, x.Error = model.ProjectCloneFailed, err.Error()
			} else {
				x.Status, x.Error, x.CloneURL, x.Git = "", "", "", true
			}
		})
		if ok {
			m.pushProject(np)
		}
	}()
}

// clone runs the clone, in uid's container for an isolated project, on the
// server for a host-run one. Its error is fit to show to everyone who sees
// the project: the end of what git said, without secrets.
func (m *Manager) clone(p model.Project, uid, rawURL string) error {
	var err error
	if p.Isolated() {
		err = m.cloneInContainer(p, uid, rawURL)
	} else {
		err = hostClone(rawURL, p.Path)
	}
	if err == nil {
		return nil
	}
	secrets := urlSecrets(rawURL)
	if p.Isolated() {
		if m.sb != nil {
			secrets = append(secrets, m.sb.EnvValues(p.ID, uid)...)
		}
	} else {
		secrets = append(secrets, serverSecrets()...)
	}
	msg, prefix := err.Error(), ""
	if rest, ok := strings.CutPrefix(msg, "git clone: "); ok {
		msg, prefix = rest, "git clone: "
	}
	return errors.New(scrub(prefix+lastLines(msg, 4), secrets))
}

// cloneInContainer clones rawURL into the (empty) project folder from
// inside the user's container for the project, with their env files.
func (m *Manager) cloneInContainer(p model.Project, uid, rawURL string) error {
	if m.sb == nil || !m.sb.Available() {
		return errors.New("isolated projects need podman on the server")
	}
	if err := m.ensureContainer(p, uid, nil); err != nil {
		return err
	}
	argv, kill := m.sb.Exec(p.ID, uid, p.Path, "clone-"+model.NewID(), false,
		[]string{"GIT_TERMINAL_PROMPT=0"}, []string{"git", "clone", "--quiet", "--", rawURL, "."})
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
		return fmt.Errorf("git clone: %s", msg)
	}
	return nil
}

// RetryClone clones a project whose clone failed again, as a, from rawURL
// ("" = where it was cloned from): in a's container for the project if it
// is isolated, on the server if not (admins only). It needs the project's
// folder empty.
func (m *Manager) RetryClone(a Actor, id, rawURL string) (model.Project, error) {
	p, err := m.visibleProject(a.ID, id)
	if err != nil {
		return p, err
	}
	if !m.ProjectManageable(a.ID, p) {
		return p, ErrForbidden
	}
	if !p.Isolated() && !a.Admin {
		return p, errHostOnly
	}
	if p.Status != model.ProjectCloneFailed {
		return p, errNoFailedClone
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		rawURL = p.CloneURL
	}
	if err := checkCloneURL(rawURL, p.Isolated()); err != nil {
		return p, err
	}
	if p.Isolated() && (m.sb == nil || !m.sb.Available()) {
		return p, errors.New("isolated projects need podman on the server")
	}
	if err := cloneFolder(p.Path); err != nil {
		return p, err
	}
	again := true
	np, ok := m.st.UpdateProject(id, func(x *model.Project) {
		if again = x.Status == model.ProjectCloneFailed; again {
			x.Status, x.Error, x.CloneURL = model.ProjectCloning, "", redactURL(rawURL)
		}
	})
	switch {
	case !ok:
		return p, ErrNotFound
	case !again:
		return np, errCloning // another request started it just now
	}
	m.pushProject(np)
	m.startClone(id, a.ID, rawURL)
	return np, nil
}

// HostProject reports whether a's project id runs on the host (for the
// server's passkey check).
func (m *Manager) HostProject(a Actor, id string) bool {
	p, err := m.visibleProject(a.ID, id)
	return err == nil && !p.Isolated()
}

// projectReady says why no session can start in p, if so: its clone is
// running or failed, or its folder is gone.
func projectReady(p model.Project) error {
	switch p.Status {
	case model.ProjectCloning:
		return errCloning
	case model.ProjectCloneFailed:
		return errCloneFailed
	}
	return folderThere(p)
}

// folderThere: a container mounting a missing folder wouldn't start
// ("statfs …: no such file or directory").
func folderThere(p model.Project) error {
	if p.Home {
		return nil // made when Home starts
	}
	if st, err := os.Stat(p.Path); err != nil || !st.IsDir() {
		return fmt.Errorf("the project's folder %s is missing on the server", p.Path)
	}
	return nil
}
