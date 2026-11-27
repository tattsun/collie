package session

import (
	"errors"
	"slices"
	"testing"

	"github.com/tattsun/collie/internal/model"
)

func TestNetworkAllowlistSettings(t *testing.T) {
	m, _ := newTestManager(t)
	m.st.PutProject(model.Project{ID: "pa", OwnerUser: "alice", Runtime: "container"})
	m.st.PutProject(model.Project{ID: "pt", OwnerGroup: "team", Runtime: "container"})
	m.st.PutProject(model.Project{ID: "ph", OwnerUser: "root", Runtime: "host"})
	alice, bob := Actor{ID: "alice", Name: "alice"}, Actor{ID: "bob", Name: "bob"}
	root := Actor{ID: "root", Name: "root", Admin: true}
	allowlist, internet := model.NetAllowlist, ""
	list := []string{"GitHub.com", "https://pypi.org/simple", "", "# packages"}

	// A project's own managers decide (no admin needed): it only narrows
	// what its agents reach.
	p, err := m.PatchProject(alice, "pa", PatchProjectReq{Network: &allowlist, NetAllow: &list})
	if err != nil || p.Network != model.NetAllowlist || !slices.Equal(p.NetAllow, []string{"github.com", "pypi.org"}) {
		t.Fatalf("owner setting an allowlist: %+v, %v", p, err)
	}
	if _, err := m.PatchProject(alice, "pt", PatchProjectReq{Network: &allowlist}); err != nil {
		t.Errorf("group owner: %v", err)
	}
	if _, err := m.PatchProject(bob, "pt", PatchProjectReq{Network: &internet}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a group member opened the network: %v", err)
	}
	more := []string{"pypi.org", "*.example.com"}
	if _, err := m.PatchProject(bob, "pt", PatchProjectReq{NetAllow: &more}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a group member changed the list: %v", err)
	}
	if _, err := m.NetworkLog(bob, "pt"); !errors.Is(err, ErrForbidden) {
		t.Errorf("a group member read the connection log: %v", err)
	}
	if _, err := m.NetworkLog(Actor{ID: "carol"}, "pa"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an outsider read the connection log: %v", err)
	}

	bad := []string{"github.com", "*"}
	if _, err := m.PatchProject(alice, "pa", PatchProjectReq{NetAllow: &bad}); err == nil {
		t.Error("a list allowing everything was accepted")
	}
	weird := "open"
	if _, err := m.PatchProject(alice, "pa", PatchProjectReq{Network: &weird}); err == nil {
		t.Error("an unknown network setting was accepted")
	}
	if _, err := m.PatchProject(root, "ph", PatchProjectReq{Network: &allowlist}); err == nil {
		t.Error("an allowlist on a host-run project (it would not apply)")
	}

	// Moving to the host drops the restriction (and says so in the UI),
	// but keeps the list for coming back.
	host := "host"
	m.st.PutProject(model.Project{ID: "pr", OwnerUser: "root", Runtime: "container", Network: model.NetAllowlist, NetAllow: []string{"github.com"}})
	p, err = m.PatchProject(root, "pr", PatchProjectReq{Runtime: &host})
	if err != nil || p.Network != "" || !slices.Equal(p.NetAllow, []string{"github.com"}) {
		t.Errorf("moving to the host: %+v, %v", p, err)
	}

	// Home has no settings of its own.
	m.st.PutProject(model.Project{ID: "home", OwnerUser: "alice", Runtime: "container", Home: true})
	if _, err := m.PatchProject(alice, "home", PatchProjectReq{Network: &allowlist}); !errors.Is(err, errHomeFixed) {
		t.Errorf("Home on an allowlist: %v", err)
	}
}
