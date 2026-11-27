package session

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
)

// podmanFake is a podman with one container, which says what podman 5.7
// says for an exec into a container that is stopped or only created
// (exit 255) or doesn't exist (exit 125). In its directory: "state"
// (missing, created, exited or running), "log" (the podman commands run),
// "inside" (shell run for an exec into the running container, which by
// default answers like `collie git-helper` for a diff) and "refuse-once"
// (the next exec finds the container not running, as if it was started
// only just after).
type podmanFake struct {
	t   *testing.T
	dir string
}

func newPodmanFake(t *testing.T) podmanFake {
	t.Helper()
	f := podmanFake{t, t.TempDir()}
	script := `#!/bin/sh
d=` + f.dir + `
while [ $# -gt 0 ]; do case "$1" in --*) shift ;; *) break ;; esac; done
echo "$*" >> "$d/log"
state=$(cat "$d/state")
case "$1" in
image) echo sha256:fake ;;
container)
  [ "$state" = missing ] && { echo "Error: no such container" >&2; exit 125; }
  [ "$state" = running ] && r=true || r=false
  echo "$r|$(cat "$d/config" 2>/dev/null)|<no value>" ;;
start) echo running > "$d/state" ;;
run)
  for a; do case "$a" in collie.config=*) echo "${a#collie.config=}" > "$d/config" ;; esac; done
  echo running > "$d/state" ;;
rm) echo missing > "$d/state" ;;
exec)
  if [ -e "$d/refuse-once" ]; then rm "$d/refuse-once"; state=exited; fi
  case "$state" in
  missing) echo 'Error: no container with name or ID "x" found: no such container' >&2; exit 125 ;;
  running) ;;
  *) echo 'Error: can only create exec sessions on running containers: container state improper' >&2; exit 255 ;;
  esac
  cat > /dev/null
  [ -e "$d/inside" ] && . "$d/inside"
  echo '{"changes":{"repo":true,"files":[]}}' ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(f.dir, "podman"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.set("state", "missing")
	return f
}

func (f podmanFake) set(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f podmanFake) get(name string) string {
	b, _ := os.ReadFile(filepath.Join(f.dir, name))
	return strings.TrimSpace(string(b))
}

// count is how many of the podman commands run since the log was last
// cleared were cmd.
func (f podmanFake) count(cmd string) int {
	n := 0
	for _, l := range strings.Split(f.get("log"), "\n") {
		if strings.HasPrefix(l, cmd+" ") {
			n++
		}
	}
	return n
}

// isolatedWithPodman is a manager with an isolated project of alice's
// whose containers podman fakes; with state other than "missing", alice's
// container was made earlier and is now in that state.
func isolatedWithPodman(t *testing.T, state string) (*Manager, model.Project, podmanFake) {
	t.Helper()
	f := newPodmanFake(t)
	m, _ := newTestManager(t)
	m.sb = sandbox.New(sandbox.Config{Podman: filepath.Join(f.dir, "podman"), DataDir: m.st.Dir()})
	if !m.sb.Available() {
		t.Skip("isolation is not supported here")
	}
	m.ctrActive, m.toolLn = map[string]time.Time{}, map[string]net.Listener{}
	p := model.Project{ID: "p1", Name: "app", Path: t.TempDir(), OwnerUser: "alice", Runtime: "container"}
	m.st.PutProject(p)
	t.Cleanup(func() { m.closeToolSockets(p.ID) })
	if state != "missing" {
		if err := m.ensureContainer(p, "alice", nil); err != nil {
			t.Fatal(err)
		}
		f.set("state", state)
	}
	f.set("log", "")
	return m, p, f
}

// A stopped container (as idle, or by a restart of Collie), one only
// created and, when git may make one, a missing one are started for git,
// which is then run again. Without create (removing a worktree) a missing
// container stays missing.
func TestGitStartsTheContainer(t *testing.T) {
	for _, c := range []struct {
		state  string
		create bool
		ok     bool
		starts string // what podman was told to do to bring the container up
	}{
		{"exited", true, true, "start"},
		{"created", true, true, "start"},
		{"missing", true, true, "run"},
		{"exited", false, true, "start"},
		{"created", false, true, "start"},
		{"missing", false, false, ""},
	} {
		t.Run(fmt.Sprintf("%s,create=%v", c.state, c.create), func(t *testing.T) {
			m, p, f := isolatedWithPodman(t, c.state)
			ch, err := m.git(p, "alice", c.create).Diff(p.Path, "")
			if c.ok && (err != nil || !ch.Repo) {
				t.Fatalf("diff: %+v, %v", ch, err)
			}
			if !c.ok && (err == nil || !strings.Contains(err.Error(), "no such container")) {
				t.Fatalf("diff without a container to run in: %v", err)
			}
			for _, cmd := range []string{"start", "run", "rm"} {
				want := 0
				if cmd == c.starts {
					want = 1
				}
				if n := f.count(cmd); n != want {
					t.Errorf("podman %s: %d times, want %d\n%s", cmd, n, want, f.get("log"))
				}
			}
			execs, state := 1, c.state
			if c.ok {
				execs, state = 2, "running"
			}
			if f.count("exec") != execs {
				t.Errorf("%d execs, want %d\n%s", f.count("exec"), execs, f.get("log"))
			}
			if f.get("state") != state {
				t.Errorf("container %s, want %s", f.get("state"), state)
			}
		})
	}
}

// What the helper exits with in a running container comes back as podman's
// exit code, also 125 or 255: it is the helper's failure, not a reason to
// start (or re-create) the container and ask again.
func TestGitTellsTheHelpersFailuresFromPodmans(t *testing.T) {
	for _, code := range []int{1, 2, 125, 126, 127, 255} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			m, p, f := isolatedWithPodman(t, "running")
			f.set("inside", fmt.Sprintf("echo 'collie git-helper: unexpected EOF' >&2; exit %d", code))
			_, err := m.git(p, "alice", true).Diff(p.Path, "")
			if err == nil || !strings.Contains(err.Error(), "collie git-helper: unexpected EOF") {
				t.Fatalf("diff: %v", err)
			}
			if f.count("exec") != 1 || f.count("start")+f.count("run")+f.count("rm") != 0 {
				t.Errorf("podman ran:\n%s", f.get("log"))
			}
		})
	}
}

// A container that was down when git tried it, and is up when Collie looks
// (started by an agent, or by git called at the same time), is just asked
// again.
func TestGitRetriesInAContainerStartedMeanwhile(t *testing.T) {
	m, p, f := isolatedWithPodman(t, "running")
	f.set("refuse-once", "")
	if _, err := m.git(p, "alice", true).Diff(p.Path, ""); err != nil {
		t.Fatal(err)
	}
	if f.count("exec") != 2 || f.count("start")+f.count("run")+f.count("rm") != 0 {
		t.Errorf("podman ran:\n%s", f.get("log"))
	}
}
