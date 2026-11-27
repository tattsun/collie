package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/preview"
)

// Which session a server belongs to, and what host-run sessions may open.

// TestHelperListen is a dev server run as a process of its own (see
// startHelper).
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

// startHelper runs a dev server working in dir, as an agent's shell would
// start it there.
func startHelper(t *testing.T, dir string) int {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	cmd := exec.Command("sh", "-c", `exec 3<&0; "$0" -test.run='^TestHelperListen$' <&3 3<&- & wait`, os.Args[0])
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

type listed struct {
	Ports []portInfo
	Slots int
}

func (f *previewFixture) ports(t *testing.T, sess, sid string) (int, listed) {
	t.Helper()
	w := f.do("GET", "/api/sessions/"+sid+"/ports", "", cookie(sessionCookie, sess))
	var out listed
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func has(ps []portInfo, port int) (portInfo, bool) {
	for _, p := range ps {
		if p.Port == port {
			return p, true
		}
	}
	return portInfo{}, false
}

// On the host, every host-run session's agent runs as the same user: a
// session sees and opens only the servers working in its own folder.
func TestHostRunSessionsSeeOnlyTheirOwnServers(t *testing.T) {
	s := newTestServer(t, Options{PreviewURL: previewOrigin, PreviewPublic: []int{8443}})
	if err := s.users.AddPasskey(s.owner, webauthn.Credential{ID: []byte("c1")}, ""); err != nil {
		t.Fatal(err)
	}
	bob, _ := s.users.CreateUser("bob", "Bob", true)
	f := &previewFixture{testServer: s}
	f.ownerSess, _ = s.users.NewSession(s.owner, "test", "")
	f.bobSess, _ = s.users.NewSession(bob.ID, "test", "")

	base := t.TempDir()
	dir := func(name string) string {
		d := filepath.Join(base, name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		return d
	}
	p1, w1, p2, other := dir("p1"), dir("worktrees/p1/s1"), dir("p2"), dir("elsewhere")
	st := s.m.Store()
	st.PutProject(model.Project{ID: "p1", Name: "web", Path: p1, OwnerUser: s.owner, Runtime: "host"})
	st.PutProject(model.Project{ID: "p2", Name: "bobs", Path: p2, OwnerUser: bob.ID, Runtime: "host"})
	st.PutSession(model.Session{ID: "s1", ProjectID: "p1", Owner: s.owner, Cwd: w1, Worktree: true, Title: "worktree"})
	st.PutSession(model.Session{ID: "s2", ProjectID: "p1", Owner: s.owner, Cwd: p1, Title: "in the folder"})
	st.PutSession(model.Session{ID: "s3", ProjectID: "p1", Owner: s.owner, Cwd: p1, Title: "also in the folder"})
	st.PutSession(model.Session{ID: "s4", ProjectID: "p2", Owner: bob.ID, Cwd: p2, Title: "bob's"})

	inW1, inP1, inP2, nowhere := startHelper(t, w1), startHelper(t, filepath.Join(p1, ".")), startHelper(t, p2), startHelper(t, other)
	all := []int{inW1, inP1, inP2, nowhere}

	cases := []struct {
		sess, sid string
		see       int
		sessions  []string
		folder    string
	}{
		{f.ownerSess, "s1", inW1, []string{"s1"}, "worktree"},
		{f.ownerSess, "s2", inP1, []string{"s2", "s3"}, "project"}, // the folder they share
		{f.ownerSess, "s3", inP1, []string{"s2", "s3"}, "project"},
		{f.bobSess, "s4", inP2, []string{"s4"}, "project"},
	}
	for _, c := range cases {
		code, l := f.ports(t, c.sess, c.sid)
		if code != http.StatusOK {
			t.Fatalf("%s: %d", c.sid, code)
		}
		for _, port := range all {
			p, ok := has(l.Ports, port)
			if ok != (port == c.see) {
				t.Errorf("%s: port %d listed %v", c.sid, port, ok)
			}
			if ok {
				slices.Sort(p.Sessions)
				if !slices.Equal(p.Sessions, c.sessions) || p.Folder != c.folder {
					t.Errorf("%s: port %d told as %v %q, want %v %q", c.sid, port, p.Sessions, p.Folder, c.sessions, c.folder)
				}
			}
		}
		// Opening: only its own, typed in or not.
		for _, port := range all {
			w := f.do("POST", "/api/sessions/"+c.sid+"/preview", fmt.Sprintf(`{"port":%d}`, port), cookie(sessionCookie, c.sess),
				header("Origin", "https://collie.example.com"))
			if (w.Code == http.StatusOK) != (port == c.see) {
				t.Errorf("%s: opening port %d: %d", c.sid, port, w.Code)
			}
		}
	}
}

func TestDescribePorts(t *testing.T) {
	fs := []preview.Folder{{Path: "/p", Sessions: []string{"s2", "s3"}}, {Path: "/w/s1", Sessions: []string{"s1"}}, {Path: "/q"}}
	project := map[string]bool{"/p": true, "/q": true}
	ports := []preview.Port{
		{Port: 1, Dirs: []string{"/w/s1/web"}},
		{Port: 2, Dirs: []string{"/p"}},
		{Port: 3, Dirs: []string{"/tmp"}},
		{Port: 4, Dirs: []string{"/q/x"}},
		{Port: 5}, // an old bridge, or a process we may not look at
	}
	got := describePorts(ports, fs, project, func(id string) bool { return id != "s3" })
	want := []portInfo{
		{Port: 1, Sessions: []string{"s1"}, Folder: "worktree"},
		{Port: 2, Sessions: []string{"s2"}, Folder: "project"}, // (s3: not shown to this user)
		{Port: 3, Sessions: []string{}},
		{Port: 4, Sessions: []string{}, Folder: "project"},
		{Port: 5, Sessions: []string{}},
	}
	for i := range want {
		if got[i].Port != want[i].Port || !slices.Equal(got[i].Sessions, want[i].Sessions) || got[i].Folder != want[i].Folder {
			t.Errorf("port %d: %+v, want %+v", want[i].Port, got[i], want[i])
		}
	}
}

// Previews on Collie's name get a few ports, each its own origin: links
// for different servers go to different ports, the same server to its own.
func TestPreviewSlots(t *testing.T) {
	f := newPreviewFixture(t)
	f.opt.PreviewURL, f.opt.PreviewPorts, f.opt.PreviewPublic = ":", []int{7879, 7880, 7881}, []int{8443, 8444, 8445}
	f.opt.PublicURL = "https://collie.example.com"
	other := newDevServer(t)
	at := func(sess string, port int) (string, string) {
		w := f.do("POST", "/api/sessions/"+f.sessionID+"/preview", fmt.Sprintf(`{"port":%d}`, port), cookie(sessionCookie, sess),
			header("Origin", "https://collie.example.com"))
		var out struct{ URL string }
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		u, err := url.Parse(out.URL)
		if err != nil || u.Host == "" {
			t.Fatalf("link: %d %s", w.Code, w.Body.String())
		}
		return u.Port(), u.Query().Get("t")
	}
	p1, t1 := at(f.ownerSess, f.dev.port)
	p2, _ := at(f.ownerSess, other.port)
	if p1 != "8443" || p2 != "8444" {
		t.Errorf("two servers at once: ports %s and %s", p1, p2)
	}
	// Spent on its own port (slot 0) it opens; the same server goes back there.
	w := f.onPreview("GET", preview.OpenPath+"?t="+t1, cookie(sessionCookie, f.ownerSess))
	c := previewCookie(t, w)
	if c == "" {
		t.Fatalf("open: %d", w.Code)
	}
	w = f.do("POST", "/api/sessions/"+f.sessionID+"/preview", fmt.Sprintf(`{"port":%d}`, f.dev.port), cookie(sessionCookie, f.ownerSess),
		cookie(preview.CookieName(0), c), header("Origin", "https://collie.example.com"))
	if !strings.Contains(w.Body.String(), ":8443/") {
		t.Errorf("the same server again: %s", w.Body.String())
	}
	if _, l := f.ports(t, f.ownerSess, f.sessionID); l.Slots != 3 {
		t.Errorf("slots %d", l.Slots)
	}
}
