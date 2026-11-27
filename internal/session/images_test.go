package session

import (
	"path/filepath"
	"testing"

	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
)

func TestProjectImageChoice(t *testing.T) {
	m, _ := newTestManager(t)
	m.sb = sandbox.New(sandbox.Config{Podman: filepath.Join(t.TempDir(), "no-podman"), DataDir: t.TempDir()})
	img, err := m.sb.PutImage(sandbox.ImageDef{Name: "Android", Containerfile: "RUN true"})
	if err != nil {
		t.Fatal(err)
	}
	m.st.PutProject(model.Project{ID: "pa", OwnerUser: "alice", Runtime: "container"})
	m.st.PutProject(model.Project{ID: "pt", OwnerGroup: "team", Runtime: "container"})
	m.st.PutProject(model.Project{ID: "ph", OwnerUser: "alice", Runtime: "host"})
	m.st.PutProject(model.Project{ID: "home", OwnerUser: "alice", Runtime: "container", Home: true})
	alice, bob := Actor{ID: "alice", Name: "alice"}, Actor{ID: "bob", Name: "bob"}

	// Whoever manages a project picks its image; no admin needed.
	if p, err := m.PatchProject(alice, "pa", PatchProjectReq{Image: &img.ID}); err != nil || p.Image != img.ID {
		t.Fatalf("the owner could not pick an image: %v", err)
	}
	if _, err := m.PatchProject(bob, "pt", PatchProjectReq{Image: &img.ID}); err == nil {
		t.Error("a group member (not owner) picked the image")
	}
	if p, err := m.PatchProject(alice, "pt", PatchProjectReq{Image: &img.ID}); err != nil || p.Image != img.ID {
		t.Errorf("the group owner could not pick an image: %v", err)
	}
	for id, choice := range map[string]string{"pa": "nope", "ph": img.ID, "home": img.ID} {
		if _, err := m.PatchProject(alice, id, PatchProjectReq{Image: &choice}); err == nil {
			t.Errorf("%s accepted image %q", id, choice)
		}
	}
	std := sandbox.ImageStandard
	if p, err := m.PatchProject(alice, "pa", PatchProjectReq{Image: &std}); err != nil || p.Image != std {
		t.Errorf("the standard image: %v", err)
	}

	// A deleted image's projects follow the default.
	if err := m.DeleteImage(img.ID); err != nil {
		t.Fatal(err)
	}
	if p, _ := m.st.Project("pt"); p.Image != sandbox.ImageDefault {
		t.Errorf("project still on the deleted image: %q", p.Image)
	}
	if p, _ := m.st.Project("pa"); p.Image != std {
		t.Errorf("an explicit choice changed: %q", p.Image)
	}
}
