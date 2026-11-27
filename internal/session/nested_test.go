package session

import (
	"testing"

	"github.com/tattsun/collie/internal/model"
)

func TestNestedContainersAreAnAdminDecision(t *testing.T) {
	m, _ := newTestManager(t)
	m.st.PutProject(model.Project{ID: "pa", OwnerUser: "alice", Runtime: "container"})
	m.st.PutProject(model.Project{ID: "pr", OwnerUser: "root", Runtime: "container"})
	on, off := true, false
	if _, err := m.PatchProject(Actor{ID: "alice", Name: "alice"}, "pa", PatchProjectReq{Nested: &on}); err == nil {
		t.Error("a member allowed containers inside their project")
	}
	root := Actor{ID: "root", Name: "root", Admin: true}
	p, err := m.PatchProject(root, "pr", PatchProjectReq{Nested: &on})
	if err != nil || !p.Nested {
		t.Fatalf("admin could not turn it on: %v", err)
	}
	host := "host"
	if p, err = m.PatchProject(root, "pr", PatchProjectReq{Runtime: &host, Nested: &on}); err == nil {
		t.Error("nested containers on a host project")
	}
	if p, err = m.PatchProject(root, "pr", PatchProjectReq{Runtime: &host}); err != nil || p.Nested {
		t.Errorf("moving to the host kept nested (%v, %v)", p.Nested, err)
	}
	if p, err = m.PatchProject(root, "pr", PatchProjectReq{Nested: &off}); err != nil || p.Nested {
		t.Errorf("turning it off: %v", err)
	}
}
