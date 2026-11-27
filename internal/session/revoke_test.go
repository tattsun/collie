package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
)

// Admins' clones must name remote repositories: anything else git takes
// as a path on the server (another user's project too) or a helper.
func TestRemoteURL(t *testing.T) {
	for u, ok := range map[string]bool{
		"https://github.com/a/b.git":   true,
		"http://git.lan/a/b":           true,
		"ssh://git@github.com/a/b.git": true,
		"git://example.com/a/b":        true,
		"git@github.com:a/b.git":       true,
		"p1/../../bob/secret":          false,
		"bob/secret":                   false,
		"github.com:a/b":               false, // scp-like without a user: also a relative path's look
		"file:///etc":                  false,
		"ext::sh -c id":                false,
		"ssh://-oProxyCommand=x/a":     false,
		"ssh://-o@x/a":                 false,
		"-oProxyCommand=x@host:a":      false,
		"https://github.com/a b":       false,
		"https://github.com/a\nb":      false,
		"https:///nohost":              false,
		"C:/x":                         false,
	} {
		if got := remoteURL(u); got != ok {
			t.Errorf("remoteURL(%q) = %v", u, got)
		}
	}
}

// revokeFake is a podman listing two running containers of bob's: one for
// the group project, one for his Home (which mounts the group project
// read-only). It logs what it is told to do.
func revokeFake(t *testing.T, groupPath string) string {
	t.Helper()
	dir := t.TempDir()
	mounts := `[{"Destination":"/home/agent","RW":true},{"Destination":"` + groupPath + `","RW":false},{"Destination":"/etc/hosts","RW":false}]`
	script := `#!/bin/sh
d=` + dir + `
while [ $# -gt 0 ]; do case "$1" in --*) shift ;; *) break ;; esac; done
echo "$*" >> "$d/log"
case "$1" in
ps) echo '[{"Names":["collie-gp-bob"],"State":"running","Labels":{"collie.project":"gp","collie.user":"bob"}},{"Names":["collie-home-bob"],"State":"running","Labels":{"collie.project":"home-bob","collie.user":"bob"}},{"Names":["collie-pa-alice"],"State":"running","Labels":{"collie.project":"pa","collie.user":"alice"}}]' ;;
container) echo '` + mounts + `' ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Removed from a group, a member loses their container on its project and
// the Home container that could still read it; other containers stay.
func TestEnforceAccessStopsWhatWasLost(t *testing.T) {
	m, acc := newTestManager(t)
	group := t.TempDir()
	dir := revokeFake(t, group)
	m.sb = sandbox.New(sandbox.Config{Podman: filepath.Join(dir, "podman"), DataDir: m.st.Dir()})
	if !m.sb.Available() {
		t.Skip("isolation is not supported here")
	}
	m.ctrActive = map[string]time.Time{}
	m.st.PutProject(model.Project{ID: "gp", Name: "app", Path: group, OwnerGroup: "team", Runtime: "container"})
	m.st.PutProject(model.Project{ID: "pa", Name: "mine", Path: t.TempDir(), OwnerUser: "alice", Runtime: "container"})
	m.st.PutProject(model.Project{ID: "home-bob", Name: "Home", Path: t.TempDir(), OwnerUser: "bob", Home: true, Runtime: "container"})
	log := func() string { b, _ := os.ReadFile(filepath.Join(dir, "log")); return string(b) }

	m.EnforceAccess()
	if strings.Contains(log(), "kill") {
		t.Fatalf("killed while everyone may:\n%s", log())
	}
	delete(acc["team"], "bob")
	m.EnforceAccess()
	l := log()
	for _, name := range []string{"collie-gp-bob", "collie-home-bob"} {
		if !strings.Contains(l, "kill "+name) {
			t.Errorf("%s not stopped:\n%s", name, l)
		}
	}
	if strings.Contains(l, "kill collie-pa-alice") {
		t.Errorf("alice's container stopped:\n%s", l)
	}
}

// Host-run projects stay an admin's: a demoted admin keeps no agents there.
func TestMayUse(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetAccess(allAdmins{fakeAccess{"team": {"alice": "owner"}}})
	host := model.Project{ID: "h", OwnerUser: "alice", Runtime: "host"}
	if !m.MayUse("alice", host) {
		t.Fatal("an admin's host project")
	}
	m.SetAccess(fakeAccess{"team": {"alice": "owner"}})
	if m.MayUse("alice", host) {
		t.Fatal("a member's host project")
	}
	if !m.MayUse("alice", model.Project{ID: "home", OwnerUser: "alice", Home: true, Runtime: "host"}) {
		t.Fatal("Home on a server without podman")
	}
	m.UserActive = func(string) bool { return false }
	if m.MayUse("alice", model.Project{ID: "c", OwnerUser: "alice", Runtime: "container"}) {
		t.Fatal("a disabled user")
	}
}
