package session

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
	"github.com/tattsun/collie/internal/store"
)

// cloneManager is a manager whose containers podman fakes (see podmanFake:
// "inside" is what an exec runs), and whose clones on the server are
// recorded instead of run.
func cloneManager(t *testing.T) (*Manager, podmanFake, *hostClones) {
	t.Helper()
	f := newPodmanFake(t)
	m, _ := newTestManager(t)
	m.sb = sandbox.New(sandbox.Config{Podman: filepath.Join(f.dir, "podman"), DataDir: m.st.Dir()})
	if !m.sb.Available() {
		t.Skip("isolation is not supported here")
	}
	m.ctrActive, m.toolLn = map[string]time.Time{}, map[string]net.Listener{}
	t.Cleanup(func() {
		for _, p := range m.st.Projects() {
			m.closeToolSockets(p.ID)
		}
	})
	f.set("log", "")
	h := &hostClones{}
	old := hostClone
	hostClone = h.clone
	t.Cleanup(func() { hostClone = old })
	return m, f, h
}

// hostClones records the clones run on the server; fail makes them fail
// with that message.
type hostClones struct {
	mu   sync.Mutex
	runs [][2]string // url, dest
	fail string
}

func (h *hostClones) clone(url, dest string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runs = append(h.runs, [2]string{url, dest})
	if h.fail != "" {
		return errors.New("git clone: " + h.fail)
	}
	return os.WriteFile(filepath.Join(dest, "README"), nil, 0o644)
}

func (h *hostClones) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.runs)
}

// waitClone waits for the project's clone to end.
func waitClone(t *testing.T, m *Manager, id string) model.Project {
	t.Helper()
	for i := 0; i < 500; i++ {
		p, ok := m.st.Project(id)
		if !ok {
			t.Fatal("project gone")
		}
		if p.Status != model.ProjectCloning {
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the clone did not end")
	return model.Project{}
}

// insideClones makes an exec that runs `git clone` into dir succeed (it
// leaves a README) or, with fail, print fail and exit 128 like git.
func insideClones(f podmanFake, dir, fail string) {
	if fail == "" {
		f.set("inside", `case "$*" in *" git clone --quiet -- "*) : > "`+dir+`/README" ;; esac`)
		return
	}
	f.set("failmsg", fail)
	f.set("inside", `case "$*" in *" git clone --quiet -- "*) cat "$d/failmsg" >&2; exit 128 ;; esac`)
}

// The issue: an admin's clone into an isolated project ran git on the
// server, with the collie user's credentials instead of the admin's
// GH_TOKEN. It now runs in the admin's container for the project, with
// their env files, like a member's.
func TestAdminIsolatedCloneRunsInContainer(t *testing.T) {
	m, f, h := cloneManager(t)
	root := Actor{ID: "root", Name: "root", Admin: true}
	if err := m.SetUserEnv(root.ID, "GH_TOKEN=github_pat_rootsecret0123456789\n"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(m.cfg.ProjectRoot, "users", "root", "mejiro")
	insideClones(f, dir, "")
	const url = "https://github.com/tattsun/mejiro.git"
	p, err := m.CreateProject(root, CreateProjectReq{Kind: "clone", URL: url, Runtime: "container"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != model.ProjectCloning || p.Path != dir || p.CloneURL != url {
		t.Fatalf("new project: %+v", p)
	}
	if p = waitClone(t, m, p.ID); p.Status != "" || p.Error != "" || !p.Git || p.CloneURL != "" {
		t.Fatalf("after the clone: %+v", p)
	}
	if _, err := os.Stat(filepath.Join(dir, "README")); err != nil {
		t.Fatalf("the clone didn't land in the project's folder: %v", err)
	}
	if n := h.count(); n != 0 {
		t.Fatalf("git ran on the server %d time(s)", n)
	}
	var clone string
	for _, l := range strings.Split(f.get("log"), "\n") {
		if strings.HasPrefix(l, "exec ") && strings.Contains(l, " git clone ") {
			clone = l
		}
	}
	for _, want := range []string{
		"-w " + dir + " ",
		"--env-file " + m.sb.UserEnvPath(root.ID) + " ", // the admin's GH_TOKEN
		sandbox.Name(p.ID, root.ID) + " ",               // in the admin's container
		"git clone --quiet -- " + url + " .",
	} {
		if !strings.Contains(clone, want) {
			t.Errorf("the clone's podman exec %q lacks %q", clone, want)
		}
	}
}

// A host-run project (admins only) is still cloned on the server, and no
// container is involved.
func TestHostCloneRunsOnServer(t *testing.T) {
	m, f, h := cloneManager(t)
	root := Actor{ID: "root", Name: "root", Admin: true}
	const url = "git@github.com:tattsun/mejiro.git" // any remote URL, as before
	p, err := m.CreateProject(root, CreateProjectReq{Kind: "clone", URL: url, Runtime: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if p = waitClone(t, m, p.ID); p.Status != "" || !p.Git {
		t.Fatalf("after the clone: %+v", p)
	}
	if len(h.runs) != 1 || h.runs[0] != [2]string{url, p.Path} {
		t.Fatalf("clones on the server: %v", h.runs)
	}
	if f.count("exec") != 0 || f.count("run") != 0 {
		t.Fatalf("a host clone used podman: %s", f.get("log"))
	}
}

// Isolated clones take https:// URLs only, from admins too; host-run ones
// any remote URL.
func TestCloneURLs(t *testing.T) {
	for _, c := range []struct {
		url      string
		isolated bool
		ok       bool
	}{
		{"https://github.com/a/b.git", true, true},
		{"https://github.com/a/b.git", false, true},
		{"git@github.com:a/b.git", true, false},
		{"ssh://git@github.com/a/b.git", true, false},
		{"git://example.com/a/b.git", true, false},
		{"http://example.com/a/b.git", true, false},
		{"git@github.com:a/b.git", false, true},
		{"ssh://git@github.com/a/b.git", false, true},
		{"", false, false},
		{"/etc", false, false},
		{"file:///etc", false, false},
		{"ext::sh -c id", false, false},
		{"--upload-pack=x", false, false},
	} {
		if err := checkCloneURL(c.url, c.isolated); (err == nil) != c.ok {
			t.Errorf("checkCloneURL(%q, isolated=%v) = %v", c.url, c.isolated, err)
		}
	}
	m, _, _ := cloneManager(t)
	root := Actor{ID: "root", Name: "root", Admin: true}
	if _, err := m.CreateProject(root, CreateProjectReq{Kind: "clone", URL: "git@github.com:a/b.git", Runtime: "container"}); err == nil || !strings.Contains(err.Error(), "https://") {
		t.Errorf("an admin's isolated clone over ssh: %v", err)
	}
}

// A failed clone keeps the project, with the reason (no secrets in it),
// across a restart. No session starts there, and it can be cloned again,
// once its folder is empty.
func TestCloneFailureAndRetry(t *testing.T) {
	m, f, _ := cloneManager(t)
	bob := Actor{ID: "bob", Name: "bob"}
	const envToken = "github_pat_bobsecret0123456789"
	if err := m.SetUserEnv(bob.ID, "GH_TOKEN="+envToken+"\n"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(m.cfg.ProjectRoot, "users", "bob", "mejiro")
	const urlToken = "ghp_urlsecret0123456789abcdef"
	url := "https://bob:" + urlToken + "@github.com/tattsun/mejiro.git"
	insideClones(f, dir, "remote: Repository not found.\nfatal: repository '"+url+"/' not found (credential helper said "+envToken+")")
	p, err := m.CreateProject(bob, CreateProjectReq{Kind: "clone", URL: url})
	if err != nil {
		t.Fatal(err)
	}
	if p.CloneURL != "https://github.com/tattsun/mejiro.git" {
		t.Errorf("kept URL %q", p.CloneURL)
	}
	p = waitClone(t, m, p.ID)
	if p.Status != model.ProjectCloneFailed || !strings.HasPrefix(p.Error, "git clone: ") || !strings.Contains(p.Error, "Repository not found") {
		t.Fatalf("after a failed clone: %+v", p)
	}
	for _, secret := range []string{urlToken, envToken, "bob:"} {
		if strings.Contains(p.Error, secret) || strings.Contains(p.CloneURL, secret) {
			t.Errorf("%q shows %q", p.Error, secret)
		}
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("the project's folder is gone: %v", err)
	}

	// No session starts in it, and the message says why.
	if _, err := m.CreateSession(bob, CreateSessionReq{ProjectID: p.ID, Agent: "claude"}); !errors.Is(err, errCloneFailed) {
		t.Errorf("a session in a project whose clone failed: %v", err)
	}

	// It stays failed after a restart.
	if err := m.st.Flush(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(m.st.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if q, _ := st.Project(p.ID); q.Status != p.Status || q.Error != p.Error || q.CloneURL != p.CloneURL {
		t.Errorf("after a restart: %+v", q)
	}

	// Not into a folder with something in it.
	stray := filepath.Join(dir, "stray")
	if err := os.WriteFile(stray, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RetryClone(bob, p.ID, ""); err == nil || !strings.Contains(err.Error(), "is not empty") {
		t.Errorf("clone again into a folder that isn't empty: %v", err)
	}
	if q, _ := m.st.Project(p.ID); q.Status != model.ProjectCloneFailed {
		t.Errorf("a refused retry changed the project: %+v", q)
	}
	os.Remove(stray)

	// Only https:// in an isolated project, and only by its owner.
	if _, err := m.RetryClone(bob, p.ID, "git@github.com:tattsun/mejiro.git"); err == nil {
		t.Error("cloned again over ssh")
	}
	if _, err := m.RetryClone(Actor{ID: "alice", Name: "alice"}, p.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("someone else cloned bob's project again: %v", err)
	}

	// Again, from the URL kept (without the credentials), now working.
	insideClones(f, dir, "")
	f.set("log", "")
	q, err := m.RetryClone(bob, p.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if q.Status != model.ProjectCloning || q.Error != "" {
		t.Errorf("retrying: %+v", q)
	}
	if _, err := m.RetryClone(bob, p.ID, ""); !errors.Is(err, errCloning) && !errors.Is(err, errNoFailedClone) {
		t.Errorf("a second retry meanwhile: %v", err)
	}
	if q = waitClone(t, m, p.ID); q.Status != "" || q.Error != "" || q.CloneURL != "" || !q.Git {
		t.Fatalf("after cloning again: %+v", q)
	}
	if !strings.Contains(f.get("log"), "git clone --quiet -- https://github.com/tattsun/mejiro.git .") {
		t.Errorf("cloned again from elsewhere: %s", f.get("log"))
	}
	if _, err := m.RetryClone(bob, p.ID, ""); !errors.Is(err, errNoFailedClone) {
		t.Errorf("cloning a cloned project again: %v", err)
	}
}

// A host-run project's failed clone: the server's secrets are hidden too,
// and only an admin can clone it again (on the server).
func TestHostCloneFailureAndRetry(t *testing.T) {
	m, _, h := cloneManager(t)
	t.Setenv("COLLIE_TEST_TOKEN", "serversecret0123456789")
	h.fail = "fatal: could not read Username for 'https://github.com': terminal prompts disabled (serversecret0123456789)"
	root := Actor{ID: "root", Name: "root", Admin: true}
	p, err := m.CreateProject(root, CreateProjectReq{Kind: "clone", URL: "https://github.com/tattsun/mejiro.git", Runtime: "host"})
	if err != nil {
		t.Fatal(err)
	}
	p = waitClone(t, m, p.ID)
	if p.Status != model.ProjectCloneFailed || strings.Contains(p.Error, "serversecret") || !strings.Contains(p.Error, "could not read Username") {
		t.Fatalf("after a failed clone: %+v", p)
	}
	if _, err := m.CreateSession(root, CreateSessionReq{ProjectID: p.ID, Agent: "claude"}); !errors.Is(err, errCloneFailed) {
		t.Errorf("a session in a project whose clone failed: %v", err)
	}
	if _, err := m.RetryClone(Actor{ID: "root", Name: "root"}, p.ID, ""); !errors.Is(err, errHostOnly) {
		t.Errorf("a non-admin cloned on the server: %v", err)
	}
	h.fail = ""
	if _, err := m.RetryClone(root, p.ID, ""); err != nil {
		t.Fatal(err)
	}
	if p = waitClone(t, m, p.ID); p.Status != "" || !p.Git {
		t.Fatalf("after cloning again: %+v", p)
	}
	if len(h.runs) != 2 || h.runs[1] != [2]string{"https://github.com/tattsun/mejiro.git", p.Path} {
		t.Errorf("clones on the server: %v", h.runs)
	}
}

// A clone that was running when Collie stopped is a failed one after the
// restart.
func TestCloneInterruptedByRestart(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st.PutProject(model.Project{ID: "p1", Name: "x", Path: t.TempDir(), OwnerUser: "bob", Runtime: "container", Status: model.ProjectCloning, CloneURL: "https://github.com/a/b.git"})
	NewManager(Config{ClaudeBin: "/nonexistent/claude", CodexBin: "/nonexistent/codex", ProjectRoot: t.TempDir()}, st, NewHub(), nil)
	if p, _ := st.Project("p1"); p.Status != model.ProjectCloneFailed || p.Error != cloneInterrupted || p.CloneURL == "" {
		t.Errorf("after the restart: %+v", p)
	}
}

// A project whose folder is gone says so, rather than podman's "statfs …:
// no such file or directory", and no container is made for it.
func TestMissingFolderIsExplained(t *testing.T) {
	m, f, _ := cloneManager(t)
	p := model.Project{ID: "p1", Name: "app", Path: filepath.Join(t.TempDir(), "gone"), OwnerUser: "alice", Runtime: "container"}
	m.st.PutProject(p)
	if err := m.ensureContainer(p, "alice", nil); err == nil || !strings.Contains(err.Error(), "is missing on the server") {
		t.Errorf("container for a missing folder: %v", err)
	}
	if n := f.count("run"); n != 0 {
		t.Errorf("podman run was tried %d time(s)", n)
	}
	if _, err := m.CreateSession(Actor{ID: "alice", Name: "alice"}, CreateSessionReq{ProjectID: p.ID, Agent: "claude"}); err == nil || !strings.Contains(err.Error(), "is missing on the server") {
		t.Errorf("session in a project whose folder is missing: %v", err)
	}
	if len(m.st.Sessions()) != 0 {
		t.Error("a session was made anyway")
	}
}

func TestRedactAndScrub(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/a/b.git":                  "https://github.com/a/b.git",
		"https://ghp_abcdefghijklmnop@github.com/a/b": "https://github.com/a/b",
		"https://user:pass@example.com/a/b.git":       "https://example.com/a/b.git",
		"git://u@example.com/a":                       "git://example.com/a",
		"ssh://git@github.com/a/b.git":                "ssh://git@github.com/a/b.git",
		"ssh://git:pw@github.com/a/b.git":             "ssh://git@github.com/a/b.git",
		"git@github.com:a/b.git":                      "git@github.com:a/b.git",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}

	url := "https://me:p%40ss%2Fw0rd@example.com/a.git"
	msg := "fatal: unable to access 'https://me:p%40ss%2Fw0rd@example.com/a.git/': p@ss/w0rd rejected\n" +
		"token ghp_0123456789abcdefghij and github_pat_0123456789abcdefghij_x, env " + "envvalue-12345 and short"
	got := scrub(msg, append(urlSecrets(url), "envvalue-12345", "short"))
	for _, secret := range []string{"p%40ss", "p@ss/w0rd", "me:", "ghp_0123", "github_pat_0123", "envvalue"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrub left %q in %q", secret, got)
		}
	}
	if !strings.Contains(got, "https://***@example.com/a.git") || !strings.Contains(got, "and short") {
		t.Errorf("scrub hid too much: %q", got)
	}
}
