package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tattsun/collie/internal/model"
)

func TestSubIDRanges(t *testing.T) {
	cases := []struct {
		id, n int
		want  string
	}{
		// keep-id with 65536 subordinate ids: the container has 0..65536,
		// 1002 being the user; the rest is for the inner Podman.
		{1002, 65536, "u:0:1002\nu:1003:64534\n"},
		{0, 65536, "u:1:65536\n"},
		{70000, 65536, "u:0:65536\n"},
	}
	for _, c := range cases {
		if got := subIDRanges("u", c.id, c.n); got != c.want {
			t.Errorf("subIDRanges(%d, %d) = %q, want %q", c.id, c.n, got, c.want)
		}
	}
}

func TestHostSubIDs(t *testing.T) {
	f := filepath.Join(t.TempDir(), "subuid")
	os.WriteFile(f, []byte("other:100000:65536\nme:165536:65536\n1002:300000:1000\nbroken\n"), 0o644)
	if n := hostSubIDs(f, "me", 1002); n != 66536 {
		t.Fatalf("counted %d ids, want the user's by name and by uid (66536)", n)
	}
	if n := hostSubIDs(filepath.Join(t.TempDir(), "none"), "me", 1002); n != 0 {
		t.Fatalf("missing file gave %d", n)
	}
}

func TestNestedRunArgs(t *testing.T) {
	m := newTestManager(t)
	has := func(args []string, want ...string) bool {
		for i := range args {
			if i+len(want) <= len(args) && slices.Equal(args[i:i+len(want)], want) {
				return true
			}
		}
		return false
	}
	plain := m.runArgs(Spec{Project: model.Project{ID: "p1", Path: "/x/p"}, UserID: "u1"}, nil)
	if !has(plain, "--security-opt", "no-new-privileges") || slices.Contains(plain, "--cap-add") || has(plain, "--security-opt", "unmask=ALL") {
		t.Fatalf("a normal project's container lost its restrictions: %q", plain)
	}
	nested := m.runArgs(Spec{Project: model.Project{ID: "p1", Path: "/x/p", Nested: true}, UserID: "u1"}, nil)
	for _, want := range [][]string{
		{"--cap-drop", "all"},
		{"--cap-add", "SETUID,SETGID"},
		{"--security-opt", "unmask=ALL"},
		{"-v", filepath.Join(m.nestedDir(), "subuid") + ":/etc/subuid:ro"},
		{"-v", m.NestedStorage("p1", "u1") + ":" + Home + "/.local/share/containers"},
	} {
		if !has(nested, want...) {
			t.Errorf("nested container lacks %q", want)
		}
	}
	if has(nested, "--security-opt", "no-new-privileges") {
		t.Error("nested container keeps no-new-privileges (newuidmap is setuid)")
	}
	// the inner storage is outside the home, whose ownership Collie repairs
	if strings.HasPrefix(m.NestedStorage("p1", "u1"), m.HomeDir("p1", "u1")) {
		t.Error("inner storage inside the home")
	}
	if configHash(plain) == configHash(nested) {
		t.Error("toggling nested would not re-create the container")
	}
}
