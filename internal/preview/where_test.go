package preview

import (
	"bufio"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAncestry(t *testing.T) {
	parents := map[int]int{40: 30, 30: 20, 20: 10, 10: 1}
	cwds := map[int]string{40: "/tmp/x", 30: "/tmp/x", 20: "/w/s1", 10: "/home/collie"}
	got := ancestry(40, 0, func(p int) int { return parents[p] }, func(p int) string { return cwds[p] })
	if want := []string{"/tmp/x", "/w/s1", "/home/collie"}; !slices.Equal(got, want) {
		t.Errorf("ancestry %v, want %v", got, want)
	}
	// Not past Collie itself.
	got = ancestry(40, 10, func(p int) int { return parents[p] }, func(p int) string { return cwds[p] })
	if want := []string{"/tmp/x", "/w/s1"}; !slices.Equal(got, want) {
		t.Errorf("stopping at Collie: %v, want %v", got, want)
	}
	// A loop in made-up data ends.
	loop := ancestry(5, 0, func(p int) int { return 5 }, func(int) string { return "/a" })
	if !slices.Equal(loop, []string{"/a"}) {
		t.Errorf("loop: %v", loop)
	}
	if p := parseStatPPid("123 (a b) c) S 45 123 0"); p != 45 {
		t.Errorf("ppid %d", p)
	}
	if d := cleanCwd("/w/s1 (deleted)"); d != "" {
		t.Errorf("deleted cwd %q", d)
	}
	if m := parsePs(" 1 0\n 50 1\nx y\n"); m[50] != 1 || len(m) != 2 {
		t.Errorf("ps: %v", m)
	}
	if m := parseLsofCwd("p50\nfcwd\nn/Users/me/proj\np51\nfcwd\nnrelative\n"); m[50] != "/Users/me/proj" || m[51] != "" {
		t.Errorf("lsof cwd: %v", m)
	}
}

func TestWhere(t *testing.T) {
	fs := []Folder{
		{Path: "/p", Sessions: []string{"s2", "s3"}}, // the project's folder, shared
		{Path: "/data/worktrees/p/s1", Sessions: []string{"s1"}},
		{Path: "/p/sub", Sessions: []string{"s4"}}, // a session working in a subfolder
		{Path: "/q"}, // a project's folder no session uses
	}
	cases := []struct {
		dirs []string
		want string
	}{
		{[]string{"/data/worktrees/p/s1/web"}, "/data/worktrees/p/s1"},
		{[]string{"/tmp/x", "/data/worktrees/p/s1"}, "/data/worktrees/p/s1"}, // started there, then moved
		{[]string{"/p/src", "/data/worktrees/p/s1"}, "/p"},                   // nearest wins
		{[]string{"/p/sub/x"}, "/p/sub"},                                     // most specific
		{[]string{"/q/app"}, "/q"},
		{[]string{"/pp"}, ""}, // not inside /p
		{nil, ""},
	}
	for _, c := range cases {
		f, ok := Where(c.dirs, fs)
		if (c.want == "") == ok || f.Path != c.want {
			t.Errorf("Where(%v) = %q, %v; want %q", c.dirs, f.Path, ok, c.want)
		}
	}
}

// TestHelperListen is a server for the tests below, run as a process of
// its own: it listens, says on which port, and stays until stdin closes.
func TestHelperListen(t *testing.T) {
	if os.Getenv("COLLIE_TEST_LISTEN") != "1" {
		t.Skip("helper process")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(1)
	}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("helper")) }))
	os.Stdout.WriteString(strconv.Itoa(ln.Addr().(*net.TCPAddr).Port) + "\n")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	os.Exit(0)
}

// startServer runs a helper server whose shell works in dir and which
// itself works in sub (empty: the same), like `cd sub && npm run dev`
// from an agent's shell in dir.
func startServer(t *testing.T, dir, sub string) int {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	if sub == "" {
		sub = dir
	}
	cmd := exec.Command("sh", "-c", `exec 3<&0; (cd "$1" && exec "$0" -test.run='^TestHelperListen$') <&3 3<&- & wait`, os.Args[0], sub)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "COLLIE_TEST_LISTEN=1")
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		in.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	port, _ := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || port == 0 {
		t.Fatalf("helper: %q %v", line, err)
	}
	return port
}

func TestListingKnowsWhereServersWork(t *testing.T) {
	base := t.TempDir()
	shell, moved := filepath.Join(base, "worktree"), filepath.Join(base, "elsewhere")
	for _, d := range []string{shell, moved} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	port := startServer(t, shell, moved)
	ports, err := Listening(Filter{UID: -1})
	if err != nil {
		t.Fatal(err)
	}
	var p Port
	for _, x := range ports {
		if x.Port == port {
			p = x
		}
	}
	// The server works in "elsewhere"; the shell that started it in the
	// worktree.
	if len(p.Dirs) < 2 || p.Dirs[0] != moved || p.Dirs[1] != shell {
		t.Fatalf("port %d: dirs %v, want [%s %s …]", port, p.Dirs, moved, shell)
	}
	if slices.Contains(p.Dirs, mustCwd(t)) {
		t.Error("the walk went past Collie itself (this test process)")
	}
	f, ok := Where(p.Dirs, []Folder{{Path: shell, Sessions: []string{"s1"}}})
	if !ok || f.Sessions[0] != "s1" {
		t.Errorf("not told to be s1's: %v", f)
	}

	// A filter that keeps only another folder's servers: neither listed
	// nor reachable.
	other := Filter{UID: -1, Allow: func(dirs []string) bool { return slices.Contains(dirs, "/nowhere") }}
	if ps, _ := Listening(other); slices.ContainsFunc(ps, func(x Port) bool { return x.Port == port }) {
		t.Error("listed for another folder")
	}
	if _, err := DialAddr(port, other); err == nil {
		t.Error("reachable for another folder")
	}
	u := HostUpstream(other)
	if u.Check(port) == nil {
		t.Error("Check let another folder's server through")
	}
}

func mustCwd(t *testing.T) string {
	d, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return d
}
