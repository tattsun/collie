package sandbox

import (
	"encoding/json"
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
		// 1002 being the user; the rest but 0 is for the inner Podman.
		{1002, 65536, "u:1:1001\nu:1003:64534\n"},
		{1, 65536, "u:2:65535\n"},
		{0, 65536, "u:1:65536\n"},
		{70000, 65536, "u:1:65535\n"},
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
		{"--security-opt", "seccomp=" + filepath.Join(m.nestedDir(), "seccomp.json")},
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

func TestNestedSeccomp(t *testing.T) {
	profile := []byte(`{"defaultAction": "SCMP_ACT_ERRNO", "syscalls": [
	  {"names": ["read", "write"], "action": "SCMP_ACT_ALLOW"},
	  {"names": ["chroot"], "action": "SCMP_ACT_ALLOW", "includes": {"caps": ["CAP_SYS_CHROOT"]}},
	  {"names": ["chroot"], "action": "SCMP_ACT_ERRNO", "excludes": {"caps": ["CAP_SYS_CHROOT"]}, "errnoRet": 1},
	  {"names": ["sethostname", "setns"], "action": "SCMP_ACT_ERRNO", "excludes": {"caps": ["CAP_SYS_ADMIN"]}, "errnoRet": 1}
	]}`)
	out, err := nestedSeccomp(profile)
	if err != nil {
		t.Fatal(err)
	}
	check := func(out []byte) {
		t.Helper()
		var p struct {
			Syscalls []struct {
				Names  []string `json:"names"`
				Action string   `json:"action"`
			} `json:"syscalls"`
		}
		if err := json.Unmarshal(out, &p); err != nil {
			t.Fatal(err)
		}
		allowed := map[string]bool{}
		for _, r := range p.Syscalls {
			for _, n := range r.Names {
				if r.Action != "SCMP_ACT_ALLOW" && slices.Contains(nestedSyscalls, n) {
					t.Errorf("%s still denied by %+v", n, r)
				}
				if r.Action == "SCMP_ACT_ALLOW" {
					allowed[n] = true
				}
			}
		}
		for _, n := range nestedSyscalls {
			if !allowed[n] {
				t.Errorf("%s not allowed", n)
			}
		}
	}
	check(out)
	if !strings.Contains(string(out), `"setns"`) {
		t.Error("other syscalls of a rule were dropped with it")
	}
	// Podman's real profile, where there is one.
	for _, path := range podmanSeccompPaths {
		if b, err := os.ReadFile(path); err == nil {
			out, err := nestedSeccomp(b)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			check(out)
			break
		}
	}
}

func TestSELinuxLabelsOff(t *testing.T) {
	m := newTestManager(t)
	sp := Spec{Project: model.Project{ID: "p1", Path: "/x/p"}, UserID: "u1"}
	plain := m.runArgs(sp, nil)
	if slices.Contains(plain, "label=disable") {
		t.Fatalf("labels turned off without SELinux: %q", plain)
	}
	m.selinux = true
	labeled := m.runArgs(sp, nil)
	i := slices.Index(labeled, "label=disable")
	if i < 1 || labeled[i-1] != "--security-opt" {
		t.Fatalf("SELinux host: container keeps labels that hide the project's files: %q", labeled)
	}
	// everything else stays as it was
	if !slices.Equal(slices.Delete(slices.Clone(labeled), i-1, i+1), plain) {
		t.Errorf("SELinux changed other arguments:\n%q\n%q", plain, labeled)
	}
}
