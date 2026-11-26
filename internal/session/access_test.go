package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tattsun/collie/internal/agent"
	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/store"
)

type fakeAccess map[string]map[string]string // group -> user -> role

func (f fakeAccess) Role(g, u string) string   { return f[g][u] }
func (f fakeAccess) GroupName(g string) string { return g }
func (f fakeAccess) IsAdmin(u string) bool     { return u == "root" }

func newTestManager(t *testing.T) (*Manager, fakeAccess) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: Config{ProjectRoot: t.TempDir()}, st: st, hub: NewHub(), rts: map[string]*Runtime{}}
	acc := fakeAccess{"team": {"alice": "owner", "bob": "member"}}
	m.SetAccess(acc)
	return m, acc
}

func TestVisibility(t *testing.T) {
	m, acc := newTestManager(t)
	m.st.PutProject(model.Project{ID: "pa", OwnerUser: "alice"})
	m.st.PutProject(model.Project{ID: "pt", OwnerGroup: "team"})
	m.st.PutSession(model.Session{ID: "a1", ProjectID: "pa", Owner: "alice"})
	m.st.PutSession(model.Session{ID: "t-shared", ProjectID: "pt", Owner: "alice", Shared: true})
	m.st.PutSession(model.Session{ID: "t-private", ProjectID: "pt", Owner: "alice"})
	m.st.PutSession(model.Session{ID: "t-bob", ProjectID: "pt", Owner: "bob", Shared: true})

	ids := func(uid string) map[string]bool {
		out := map[string]bool{}
		for _, s := range m.VisibleSessions(uid) {
			out[s.ID] = true
		}
		for _, p := range m.VisibleProjects(uid) {
			out[p.ID] = true
		}
		return out
	}
	alice, bob, carol := ids("alice"), ids("bob"), ids("carol")
	for _, id := range []string{"pa", "pt", "a1", "t-shared", "t-private", "t-bob"} {
		if !alice[id] {
			t.Errorf("alice should see %s", id)
		}
	}
	for id, want := range map[string]bool{"pa": false, "a1": false, "pt": true, "t-shared": true, "t-private": false, "t-bob": true} {
		if bob[id] != want {
			t.Errorf("bob sees %s = %v, want %v", id, bob[id], want)
		}
	}
	if len(carol) != 0 {
		t.Errorf("an outsider sees %v", carol)
	}

	bobA := Actor{ID: "bob", Name: "bob"}
	// bob can drive alice's shared session but not change or delete it
	if _, err := m.Patch(bobA, "t-shared", PatchSessionReq{Shared: new(bool)}); !errors.Is(err, ErrForbidden) {
		t.Errorf("bob unsharing alice's session: %v", err)
	}
	if err := m.DeleteSession(bobA, "t-private", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob deleting an invisible session: %v", err)
	}
	if _, _, err := m.Items(bobA, "a1", 0, 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob reading alice's personal session: %v", err)
	}
	// the group owner manages the group project; members do not
	if err := m.DeleteProject(bobA, "pt"); !errors.Is(err, ErrForbidden) {
		t.Errorf("member deleting group project: %v", err)
	}
	if _, err := m.PatchProject(Actor{ID: "alice"}, "pt", PatchProjectReq{Runtime: new(string)}); err == nil {
		t.Error("non-admin changing the runtime")
	}

	// bob leaves the group: his shared session stays visible to the group
	// but can no longer be driven (his agent identity left)
	delete(acc["team"], "bob")
	if m.SessionVisible("bob", model.Session{ID: "t-shared", ProjectID: "pt", Owner: "alice", Shared: true}) {
		t.Error("ex-member still sees the group's sessions")
	}
	s, _ := m.st.Session("t-bob")
	if !m.SessionVisible("alice", s) || m.sessionDrivable(s) {
		t.Error("an ex-member's shared session should be visible but read-only")
	}
	if err := m.Send(Actor{ID: "alice"}, "t-bob", "hi", nil); !errors.Is(err, errOwnerGone) {
		t.Errorf("driving an ex-member's session: %v", err)
	}
}

func TestMemberProjectRestrictions(t *testing.T) {
	m, _ := newTestManager(t)
	bob := Actor{ID: "bob", Name: "bob"}
	if _, err := m.CreateProject(bob, CreateProjectReq{Kind: "existing", Path: "/etc"}); err == nil {
		t.Error("member registered an arbitrary folder")
	}
	if _, err := m.CreateProject(bob, CreateProjectReq{Kind: "empty", Name: "x", Runtime: "host"}); err == nil {
		t.Error("member created a host project")
	}
	for _, url := range []string{"/home/collie/.collie", "file:///etc", "../x", "--upload-pack=x", "ext::sh -c id", "git@github.com:a/b.git"} {
		if _, err := m.CreateProject(bob, CreateProjectReq{Kind: "clone", URL: url, Name: "c"}); err == nil {
			t.Errorf("member cloned %q", url)
		}
	}
	if _, err := m.CreateProject(bob, CreateProjectReq{Kind: "empty", Name: "x", Group: "other"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("creating in a group you are not in: %v", err)
	}
}

func TestHostProjectsAreAdminOnly(t *testing.T) {
	m, _ := newTestManager(t)
	// an admin turned a group project into a host project (legacy data)
	m.st.PutProject(model.Project{ID: "ph", OwnerGroup: "team", Runtime: "host"})
	m.st.PutSession(model.Session{ID: "hs", ProjectID: "ph", Owner: "alice", Shared: true})
	bob := Actor{ID: "bob", Name: "bob"}
	if _, err := m.CreateSession(bob, CreateSessionReq{ProjectID: "ph", Agent: "claude"}); !errors.Is(err, errHostOnly) {
		t.Errorf("member created a session in a host project: %v", err)
	}
	if err := m.Send(bob, "hs", "rm -rf ~", nil); !errors.Is(err, errHostOnly) {
		t.Errorf("member drove a host agent: %v", err)
	}
	if err := m.Reply(bob, "hs", "ask-1", agent.Reply{Option: "allow"}); !errors.Is(err, errHostOnly) {
		t.Errorf("member approved a host agent's tool use: %v", err)
	}
	if _, err := m.CreateProject(Actor{ID: "root", Name: "root", Admin: true}, CreateProjectReq{Kind: "empty", Name: "g", Group: "team", Runtime: "host"}); err == nil {
		t.Error("host group project created")
	}
}

func TestProjectFoldersNeverOverlap(t *testing.T) {
	m, _ := newTestManager(t)
	root := m.cfg.ProjectRoot
	alice := Actor{ID: "alice", Name: "alice", Admin: true}
	m.st.PutProject(model.Project{ID: "pt", OwnerGroup: "team", Path: filepath.Join(root, "groups", "team", "app")})
	// personal and group folders live in separate subtrees
	if got := m.OwnerRoot(Actor{Name: "groups"}, ""); got != filepath.Join(root, "users", "groups") {
		t.Errorf("personal root = %s", got)
	}
	for _, p := range []string{filepath.Join(root, "groups", "team"), filepath.Join(root, "groups", "team", "app", "sub"), root, filepath.Join(root, "groups")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := m.CreateProject(alice, CreateProjectReq{Kind: "existing", Path: p})
		if err == nil || !strings.Contains(err.Error(), "overlaps") && !strings.Contains(err.Error(), "other people") {
			t.Errorf("registering %s: %v", p, err)
		}
	}
	// an unrelated folder is fine
	other := filepath.Join(root, "users", "alice", "mine")
	os.MkdirAll(other, 0o755)
	if _, err := m.CreateProject(alice, CreateProjectReq{Kind: "existing", Path: other}); err != nil {
		t.Errorf("unrelated folder rejected: %v", err)
	}
}
