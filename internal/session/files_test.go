package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tattsun/collie/internal/agent"
	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
)

func TestCleanFileName(t *testing.T) {
	cases := map[string]string{
		"依頼書.docx":                      "依頼書.docx",
		"Q3 budget (final).xlsx":        "Q3 budget (final).xlsx",
		"../../etc/passwd":              "passwd",
		`C:\Users\me\見積.xlsx`:           "見積.xlsx",
		".env":                          "env",
		"-rf.txt":                       "rf.txt",
		" .. -x.pdf":                    "x.pdf",
		"invoice\u202Efdp.exe":          "invoicefdp.exe", // right-to-left override
		"a\x00b\nc\x7f.txt":             "abc.txt",
		"notes. ":                       "notes",
		"":                              "file",
		"...":                           "file",
		"bad\xffutf8.csv":               "badutf8.csv",
		strings.Repeat("x", 30) + ".md": strings.Repeat("x", 30) + ".md",
	}
	for in, want := range cases {
		if got := cleanFileName(in); got != want {
			t.Errorf("cleanFileName(%q) = %q, want %q", in, got, want)
		}
	}
	long := cleanFileName(strings.Repeat("あ", 100) + ".docx")
	if len(long) > maxFileNameLen || !utf8.ValidString(long) || !strings.HasSuffix(long, "あ.docx") {
		t.Errorf("long name cut to %q (%d bytes)", long, len(long))
	}
}

func TestSaveFile(t *testing.T) {
	m, _ := newTestManager(t)
	doc := []byte("PK\x03\x04 a Word document")
	f, err := m.SaveFile("依頼書.docx", bytes.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	p, got, ok := m.AttachedFile(f.ID)
	if !ok || got != f || got.Name != "依頼書.docx" || got.Size != int64(len(doc)) {
		t.Fatalf("stored %+v, found %+v %v", f, got, ok)
	}
	if b, err := os.ReadFile(p); err != nil || !bytes.Equal(b, doc) {
		t.Fatalf("stored bytes: %q %v", b, err)
	}
	for _, id := range []string{"", "..", f.ID[:16], strings.Repeat("g", 32), f.ID + "/x", strings.ToUpper(f.ID)} {
		if _, _, ok := m.AttachedFile(id); ok {
			t.Errorf("found a file for id %q", id)
		}
	}

	if _, err := m.SaveFile("big.zip", bytes.NewReader(make([]byte, MaxFile+1))); !errors.Is(err, ErrFileTooBig) {
		t.Fatalf("a file over the limit: %v", err)
	}
	if ents, _ := os.ReadDir(m.st.FilesDir()); len(ents) != 1 {
		t.Errorf("the refused file was left behind: %v", ents)
	}
}

// A host-run session's agent gets a copy in a folder of the session, which
// Claude Code may read unasked; the message lists the copies.
func TestAttachedFilesOnTheHost(t *testing.T) {
	m := newHomeTestManager(t)
	m.SetAccess(allAdmins{})
	log := filepath.Join(t.TempDir(), "started")
	m.cfg.ClaudeBin = fakeAgent(t, "claude", log)
	work := t.TempDir()
	m.st.PutProject(model.Project{ID: "p1", Name: "web", Path: work, OwnerUser: "root"})
	m.st.PutSession(model.Session{ID: "s1", ProjectID: "p1", Owner: "root", Cwd: work, Agent: "claude"})
	m.st.PutSession(model.Session{ID: "s2", ProjectID: "p1", Owner: "root", Cwd: work, Agent: "claude"})
	root := Actor{ID: "root", Admin: true}

	doc := []byte("PK\x03\x04 brief")
	f, err := m.SaveFile("依頼書.docx", bytes.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}

	// Claude Code is started with the folder as a working directory.
	r, err := m.runtime("s2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.startDriver(false); err == nil {
		t.Fatal("the fake agent started")
	}
	b, _ := os.ReadFile(log)
	args := strings.Split(strings.TrimSpace(string(b)), "\n")
	if i := slices.Index(args, "--add-dir"); i < 0 || i+1 >= len(args) || args[i+1] != m.hostStageDir("s2") {
		t.Errorf("claude args lack --add-dir %s: %q", m.hostStageDir("s2"), args)
	}
	if fi, err := os.Stat(m.hostStageDir("s2")); err != nil || !fi.IsDir() {
		t.Errorf("the folder was not made before Claude started: %v", err)
	}

	// The timeline shows the text and the file; the agent gets the paths.
	if err := m.Send(root, "s1", "要約して", nil, []string{f.ID}); err != nil {
		t.Fatal(err)
	}
	// The fake agent fails to start in the background; wait for that.
	var items []*model.Item
	for range 500 {
		items, _, _ = m.Items(root, "s1", 0, 50)
		if s, _ := m.st.Session("s1"); s.Status == model.StatusError {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(items) == 0 || items[0].Kind != model.KindUser || items[0].Text != "要約して" || len(items[0].Files) != 1 || items[0].Files[0] != f {
		t.Fatalf("user item: %+v", items)
	}
	if !strings.Contains(formatItem(items[0]), "[attached: 依頼書.docx]") {
		t.Errorf("Home sees: %s", formatItem(items[0]))
	}
	in, err := m.stageInput("s1", queued{agent.Input{Text: "要約して"}, []model.File{f}})
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(m.hostStageDir("s1"), f.ID, "依頼書.docx")
	if !strings.HasPrefix(in.Text, "要約して\n\n") || !strings.Contains(in.Text, "- "+copyPath+" (") {
		t.Errorf("agent's message: %q", in.Text)
	}
	if b, err := os.ReadFile(copyPath); err != nil || !bytes.Equal(b, doc) {
		t.Errorf("copy for the agent: %q %v", b, err)
	}
	// Only files: the message is the list.
	if in, err := m.stageInput("s1", queued{agent.Input{}, []model.File{f}}); err != nil || !strings.HasPrefix(in.Text, "The user attached") {
		t.Errorf("files only: %q %v", in.Text, err)
	}

	if err := m.Send(root, "s1", "x", nil, []string{strings.Repeat("0", 32)}); !errors.Is(err, errFileMissing) {
		t.Errorf("unknown file: %v", err)
	}
	if err := m.Send(root, "s1", "x", nil, slices.Repeat([]string{f.ID}, maxMsgFiles+1)); !errors.Is(err, errManyFiles) {
		t.Errorf("too many files: %v", err)
	}

	// Deleting a session removes the agent's copies (s3 has no agent
	// starting in the background, which would make the folder again).
	m.st.PutSession(model.Session{ID: "s3", ProjectID: "p1", Owner: "root", Cwd: work, Agent: "claude"})
	if _, err := m.stageInput("s3", queued{agent.Input{}, []model.File{f}}); err != nil {
		t.Fatal(err)
	}
	if err := m.deleteSession("s3", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.hostStageDir("s3")); !os.IsNotExist(err) {
		t.Errorf("copies left after deleting the session: %v", err)
	}
}

// In a container, the copies go to the user's home there; Codex also
// takes the images by path from there.
func TestAttachedFilesInAContainer(t *testing.T) {
	m := newHomeTestManager(t)
	m.sb = sandbox.New(sandbox.Config{Podman: filepath.Join(t.TempDir(), "no-podman"), DataDir: t.TempDir()})
	p := model.Project{ID: "pc", Path: t.TempDir(), OwnerUser: "alice", Runtime: "container"}
	s := model.Session{ID: "c1", ProjectID: "pc", Owner: "alice", Agent: "codex"}
	m.st.PutProject(p)
	m.st.PutSession(s)
	home := m.sb.HomeDir("pc", "alice")
	if err := os.MkdirAll(home, 0o700); err != nil { // made when the container starts
		t.Fatal(err)
	}
	f, err := m.SaveFile("brief.pdf", strings.NewReader("%PDF-1.7"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := m.SaveUpload(tinyPNG)
	if err != nil {
		t.Fatal(err)
	}
	imgPath, _ := m.UploadPath(img)

	in, err := m.stageInput("c1", queued{agent.Input{Text: "look", Images: []agent.Image{{Path: imgPath}}}, []model.File{f}})
	if err != nil {
		t.Fatal(err)
	}
	want := sandbox.Home + "/.collie-uploads/" + f.ID + "/brief.pdf"
	if !strings.Contains(in.Text, "- "+want+" (8 bytes)") {
		t.Errorf("agent's message: %q", in.Text)
	}
	if b, err := os.ReadFile(filepath.Join(home, ".collie-uploads", f.ID, "brief.pdf")); err != nil || string(b) != "%PDF-1.7" {
		t.Errorf("copy in the container home: %q %v", b, err)
	}
	if got := in.Images[0].AgentPath; got != sandbox.Home+"/.collie-uploads/"+img {
		t.Errorf("Codex image path: %q", got)
	}
	if got := m.claudeStageArgs(p, s); !slices.Equal(got, []string{"--add-dir", sandbox.Home + "/.collie-uploads"}) {
		t.Errorf("claude args: %q", got)
	}
}

// A file belongs to the session it was sent in and is deleted with it,
// along with the agent's copy. Someone who saw it in a shared session
// can't send it in a session of their own (and delete it from there).
func TestFilesGoWithTheirSession(t *testing.T) {
	m := newHomeTestManager(t)
	m.SetAccess(allAdmins{fakeAccess{"team": {"alice": "owner", "bob": "member"}}})
	m.cfg.ClaudeBin = fakeAgent(t, "claude", filepath.Join(t.TempDir(), "started"))
	work := t.TempDir()
	m.st.PutProject(model.Project{ID: "pt", Name: "team", Path: work, OwnerGroup: "team"})
	m.st.PutSession(model.Session{ID: "a1", ProjectID: "pt", Owner: "alice", Cwd: work, Agent: "claude", Shared: true})
	m.st.PutSession(model.Session{ID: "b1", ProjectID: "pt", Owner: "bob", Cwd: work, Agent: "claude"})
	alice, bob := Actor{ID: "alice", Admin: true}, Actor{ID: "bob", Admin: true}
	save := func(name string) model.File {
		t.Helper()
		f, err := m.SaveFile(name, strings.NewReader("content of "+name))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	exists := func(f model.File) bool {
		_, _, ok := m.AttachedFile(f.ID)
		return ok
	}
	// The fake agent fails to start in the background; wait for that.
	send := func(a Actor, sid string, f model.File) error {
		t.Helper()
		m.updateSession(sid, func(s *model.Session) { s.Status = model.StatusStopped })
		if err := m.Send(a, sid, "", nil, []string{f.ID}); err != nil {
			return err
		}
		for range 500 {
			if s, _ := m.st.Session(sid); s.Status == model.StatusError {
				return nil
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%s: the agent never failed", sid)
		return nil
	}

	brief, notes := save("brief.docx"), save("notes.txt")
	if err := send(alice, "a1", brief); err != nil {
		t.Fatal(err)
	}
	if err := m.Send(bob, "b1", "", nil, []string{brief.ID}); !errors.Is(err, errFileTaken) {
		t.Fatalf("bob sent alice's file in his own session: %v", err)
	}
	if err := send(bob, "b1", notes); err != nil {
		t.Fatal(err)
	}
	if _, err := m.stageInput("a1", queued{agent.Input{}, []model.File{brief}}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := m.AttachedFile(brief.ID); !ok {
		t.Fatal("the claim hides the file")
	}

	if err := m.deleteSession("a1", false); err != nil {
		t.Fatal(err)
	}
	if exists(brief) {
		t.Error("the file outlived its session")
	}
	if _, err := os.Stat(m.hostStageDir("a1")); !os.IsNotExist(err) {
		t.Errorf("the agent's copy outlived the session: %v", err)
	}
	if !exists(notes) {
		t.Error("another session's file was deleted")
	}

	// Files never sent go after a day; files of sessions that are gone
	// (deleted while the file was being sent) at the next sweep.
	draft, old := save("draft.txt"), save("old.txt")
	day := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(filepath.Join(m.st.FilesDir(), old.ID), day, day); err != nil {
		t.Fatal(err)
	}
	m.sweepFiles()
	if !exists(draft) || exists(old) || !exists(notes) {
		t.Errorf("after the sweep: draft %v, old %v, notes %v", exists(draft), exists(old), exists(notes))
	}
	m.st.DeleteSession("b1")
	m.sweepFiles()
	if exists(notes) {
		t.Error("a file of a session that is gone survived the sweep")
	}
}

// In a container, the agent's copy in the user's home goes too.
func TestContainerCopyGoesWithItsSession(t *testing.T) {
	m := newHomeTestManager(t)
	m.sb = sandbox.New(sandbox.Config{Podman: filepath.Join(t.TempDir(), "no-podman"), DataDir: t.TempDir()})
	m.st.PutProject(model.Project{ID: "pc", Path: t.TempDir(), OwnerUser: "alice", Runtime: "container"})
	m.st.PutSession(model.Session{ID: "c1", ProjectID: "pc", Owner: "alice", Agent: "claude"})
	home := m.sb.HomeDir("pc", "alice")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := m.SaveFile("brief.pdf", strings.NewReader("%PDF-1.7"))
	if err != nil {
		t.Fatal(err)
	}
	// As send does, without starting an agent.
	if err := m.claimFiles("c1", []model.File{f}); err != nil {
		t.Fatal(err)
	}
	r, _ := m.runtime("c1")
	r.mu.Lock()
	r.ensureLoaded()
	r.put(&model.Item{ID: "u-1", Kind: model.KindUser, Files: []model.File{f}, Done: true})
	r.mu.Unlock()
	if _, err := m.stageInput("c1", queued{agent.Input{}, []model.File{f}}); err != nil {
		t.Fatal(err)
	}
	copyDir := filepath.Join(home, ".collie-uploads", f.ID)
	if _, err := os.Stat(filepath.Join(copyDir, "brief.pdf")); err != nil {
		t.Fatal(err)
	}

	if err := m.deleteSession("c1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(copyDir); !os.IsNotExist(err) {
		t.Errorf("the agent's copy outlived the session: %v", err)
	}
	if _, _, ok := m.AttachedFile(f.ID); ok {
		t.Error("the file outlived its session")
	}
}
