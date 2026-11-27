package session

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tattsun/collie/internal/model"
)

// A 1×1 PNG.
var tinyPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg==")

func TestSniffUpload(t *testing.T) {
	webm := append([]byte("\x1A\x45\xDF\xA3"), bytes.Repeat([]byte{0}, 64)...)
	html := []byte("<!doctype html><title>x</title><script>alert(1)</script>")
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"></svg>`)
	cases := []struct {
		name  string
		data  []byte
		media bool
		want  string
	}{
		{"a.png", tinyPNG, false, ".png"},
		{"a.html", tinyPNG, true, ".png"}, // content wins over the name
		{"a.webm", webm, false, ""},       // users attach images only
		{"a.webm", webm, true, ".webm"},
		{"a.html", html, false, ""},
		{"a.html", html, true, ".html"},
		{"a.png", html, true, ""}, // a page is only taken for what it says it is
		{"a.svg", svg, true, ".svg"},
		{"a.svg", []byte("not an image"), true, ""},
		{"a.svg", svg, false, ""},
		{"a.bin", []byte{0, 1, 2, 3}, true, ""},
		{"a.html", []byte{0xff, 0xfe, 0, 0}, true, ""},
	}
	for _, c := range cases {
		if got := sniffUpload(c.name, c.data, c.media); got != c.want {
			t.Errorf("sniffUpload(%s, media=%v) = %q, want %q", c.name, c.media, got, c.want)
		}
	}
	for _, n := range []string{"0123456789abcdef0123456789abcdef.html", "0123456789abcdef.svg"} {
		if !UploadActive(n) || !uploadRe.MatchString(n) {
			t.Errorf("%s must be a known, sandboxed upload", n)
		}
	}
	if UploadActive("0123456789abcdef.png") {
		t.Error("PNGs need no sandbox")
	}
	for _, n := range []string{"../x.html", "0123456789abcdef.htm", "0123456789abcdef.js", "0123456789ABCDEF.png"} {
		if uploadRe.MatchString(n) {
			t.Errorf("%s accepted as an upload name", n)
		}
	}
}

func TestParseShowMedia(t *testing.T) {
	cases := map[string][]string{
		`{"files":[{"path":"a.png","caption":"A"},{"path":"b.png"}]}`: {"a.png", "b.png"},
		`{"files":"[{\"path\":\"a.png\"}]"}`:                          {"a.png"},
		`{"files":["a.png","b.mp4"]}`:                                 {"a.png", "b.mp4"},
		`{"files":"a.png"}`:                                           {"a.png"},
		`{"path":"a.png"}`:                                            {"a.png"},
	}
	for in, want := range cases {
		_, files, err := parseShowMedia(json.RawMessage(in))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		var got []string
		for _, f := range files {
			got = append(got, f.Path)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: %v, want %v", in, got, want)
		}
	}
	for _, in := range []string{`{}`, `{"files":[]}`, `{"files":[{"caption":"x"}]}`, `{"files":42}`} {
		if _, _, err := parseShowMedia(json.RawMessage(in)); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
}

func TestShowMedia(t *testing.T) {
	m := newHomeTestManager(t)
	work := t.TempDir()
	m.st.PutProject(model.Project{ID: "p1", Name: "web", Path: work, OwnerUser: "root"}) // host-run: admins only
	m.st.PutProject(model.Project{ID: "p2", Name: "other", Path: t.TempDir(), OwnerUser: "root"})
	m.st.PutSession(model.Session{ID: "s1", ProjectID: "p1", Owner: "root", Cwd: work})
	m.st.PutSession(model.Session{ID: "s2", ProjectID: "p2", Owner: "root", Cwd: work})
	m.st.PutSession(model.Session{ID: "s3", ProjectID: "p1", Owner: "alice", Cwd: work})
	if err := os.WriteFile(filepath.Join(work, "a.png"), tinyPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(work, "out"), 0o700)
	if err := os.WriteFile(filepath.Join(work, "out", "report.html"), []byte("<!doctype html><h1>hi</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "notes.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	hello := func(tc *toolConn, sid string) {
		line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "collie/session", "params": map[string]string{"id": sid}})
		if m.handleRPC(tc, line) != nil {
			t.Fatal("answered the session notification")
		}
	}
	scope := toolScope{uid: "root", project: "p1"}
	args := map[string]any{"title": "Two takes", "files": []any{
		map[string]any{"path": "a.png", "caption": "A: light"},
		map[string]any{"path": filepath.Join(work, "out", "report.html"), "caption": "B: report"},
	}}

	// The bridge did not (or could not) name its session.
	tc := &toolConn{scope: scope}
	if text, isErr := callOn(t, m, tc, "show_media", args); !isErr {
		t.Fatalf("show_media without a session: %s", text)
	}
	// Sessions elsewhere are refused: another project, another user, Home.
	for _, sid := range []string{"s2", "s3", "nosuch"} {
		hello(tc, sid)
		if tc.session != "" {
			t.Fatalf("socket of p1/root accepted session %s", sid)
		}
	}
	tools := rpcOn(t, m, tc, "tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "show_media" {
		t.Errorf("a project's agents get only show_media: %v", tools)
	}
	if text, isErr := callOn(t, m, tc, "list_projects", nil); !isErr {
		t.Errorf("Home tools on a project socket: %s", text)
	}

	hello(tc, "s1")
	if tc.session != "s1" {
		t.Fatal("own session refused")
	}
	text, isErr := callOn(t, m, tc, "show_media", args)
	if isErr {
		t.Fatalf("show_media: %s", text)
	}
	items, _, err := m.Items(Actor{ID: "root", Admin: true}, "s1", 0, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("timeline: %v %v", items, err)
	}
	it := items[0]
	if it.Kind != model.KindMedia || it.Text != "Two takes" || len(it.Media) != 2 {
		t.Fatalf("media item: %+v", it)
	}
	if f := it.Media[0]; !strings.HasSuffix(f.ID, ".png") || f.Mime != "image/png" || f.Caption != "A: light" || f.Name != "a.png" {
		t.Errorf("image: %+v", f)
	}
	if f := it.Media[1]; !strings.HasSuffix(f.ID, ".html") || !strings.HasPrefix(f.Mime, "text/html") {
		t.Errorf("page: %+v", f)
	}
	for _, f := range it.Media {
		p, ok := m.UploadPath(f.ID)
		if b, err := os.ReadFile(p); !ok || err != nil || len(b) == 0 {
			t.Errorf("%s not stored: %v", f.ID, err)
		}
	}
	if !strings.Contains(formatItem(it), "a.png (A: light)") {
		t.Errorf("Home sees: %s", formatItem(it))
	}

	for _, bad := range []string{"notes.txt", "missing.png", "out"} {
		if text, isErr := callOn(t, m, tc, "show_media", map[string]any{"files": []any{bad}}); !isErr {
			t.Errorf("showed %s: %s", bad, text)
		}
	}
}

func TestToolSocketsPerProject(t *testing.T) {
	m := newHomeTestManager(t)
	a, err := m.toolSocket(toolScope{uid: "alice", project: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	home, _ := m.homeSocket("alice")
	other, _ := m.toolSocket(toolScope{uid: "bob", project: "p1"})
	if a == home || a == other || strings.HasPrefix(a, home+"/") {
		t.Fatalf("sockets share a directory: %s %s %s", a, home, other)
	}
	m.closeToolSockets("p1")
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Errorf("socket dir of a deleted project kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, toolSockName)); err != nil {
		t.Errorf("Home socket removed with a project: %v", err)
	}
}
