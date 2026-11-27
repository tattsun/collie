package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/agent"
	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/sandbox"
)

func (m *Manager) testHas(t *testing.T, ref string) bool {
	t.Helper()
	if p, ok := m.UploadPath(ref); ok {
		_, err := os.Stat(p)
		return err == nil
	}
	_, _, ok := m.AttachedFile(ref)
	return ok
}

func (m *Manager) testClaim(t *testing.T, ref string) string {
	t.Helper()
	if c, ok := m.uploadClaim(ref); ok {
		return c
	}
	c, ok := m.fileClaim(ref)
	if !ok {
		t.Fatalf("%q is neither an upload nor a file", ref)
	}
	return c
}

func putItems(t *testing.T, m *Manager, sid string, items ...*model.Item) {
	t.Helper()
	r, err := m.runtime(sid)
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.ensureLoaded()
	for _, it := range items {
		r.put(it)
	}
	r.mu.Unlock()
}

// Everything a session's timeline shows is deleted with it: what users
// attached, the agent's screenshots and the media it showed. Someone who
// saw an attachment in a shared session can't send it in a session of
// their own (and delete it from there).
func TestAttachmentsGoWithTheirSession(t *testing.T) {
	m := newHomeTestManager(t)
	m.SetAccess(allAdmins{fakeAccess{"team": {"alice": "owner", "bob": "member"}}})
	m.cfg.ClaudeBin = fakeAgent(t, "claude", filepath.Join(t.TempDir(), "started"))
	work := t.TempDir()
	m.st.PutProject(model.Project{ID: "pt", Name: "team", Path: work, OwnerGroup: "team"})
	m.st.PutSession(model.Session{ID: "a1", ProjectID: "pt", Owner: "alice", Cwd: work, Agent: "claude", Shared: true})
	m.st.PutSession(model.Session{ID: "b1", ProjectID: "pt", Owner: "bob", Cwd: work, Agent: "claude"})
	alice, bob := Actor{ID: "alice", Admin: true}, Actor{ID: "bob", Admin: true}
	file := func(name string) string {
		t.Helper()
		f, err := m.SaveFile(name, strings.NewReader("content of "+name))
		if err != nil {
			t.Fatal(err)
		}
		return f.ID
	}
	image := func() string {
		t.Helper()
		name, err := m.SaveUpload(tinyPNG)
		if err != nil {
			t.Fatal(err)
		}
		return name
	}
	// The fake agent fails to start in the background; wait for that.
	send := func(a Actor, sid string, images, files []string) error {
		t.Helper()
		m.updateSession(sid, func(s *model.Session) { s.Status = model.StatusStopped })
		if err := m.Send(a, sid, "", images, files); err != nil {
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

	photo, brief := image(), file("brief.docx")
	if err := send(alice, "a1", []string{photo}, []string{brief}); err != nil {
		t.Fatal(err)
	}
	if err := m.Send(bob, "b1", "", []string{photo}, nil); !errors.Is(err, errImageTaken) {
		t.Fatalf("bob sent alice's image in his own session: %v", err)
	}
	if err := m.Send(bob, "b1", "", nil, []string{brief}); !errors.Is(err, errFileTaken) {
		t.Fatalf("bob sent alice's file in his own session: %v", err)
	}
	shot := m.blob("a1", tinyPNG)
	page, err := m.saveSessionUpload("a1", []byte("<!doctype html><h1>report</h1>"), ".html")
	if shot == "" || err != nil {
		t.Fatalf("agent's images: %q %v", shot, err)
	}
	putItems(t, m, "a1",
		&model.Item{ID: "t-1", Kind: model.KindTool, Images: []string{shot}, Done: true},
		&model.Item{ID: "m-1", Kind: model.KindMedia, Media: []model.Media{{ID: page}}, Done: true})
	if _, err := m.stageInput("a1", queued{agent.Input{}, []model.File{{ID: brief, Name: "brief.docx"}}}); err != nil {
		t.Fatal(err)
	}
	bobsPhoto, notes := image(), file("notes.txt")
	if err := send(bob, "b1", []string{bobsPhoto}, []string{notes}); err != nil {
		t.Fatal(err)
	}

	if err := m.deleteSession("a1", false); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{photo, brief, shot, page} {
		if m.testHas(t, ref) {
			t.Errorf("%s outlived its session", ref)
		}
	}
	if ents, _ := os.ReadDir(m.st.UploadsDir()); len(ents) != 2 { // bob's photo and its claim
		t.Errorf("left in the uploads: %v", ents)
	}
	if _, err := os.Stat(m.hostStageDir("a1")); !os.IsNotExist(err) {
		t.Errorf("the agent's copies outlived the session: %v", err)
	}
	if !m.testHas(t, bobsPhoto) || !m.testHas(t, notes) {
		t.Error("another session's attachments were deleted")
	}

	// What was never sent goes after a day; what a session that is gone
	// held (deleted while it was being sent) at the next sweep.
	draftImg, draftFile, oldImg, oldFile := image(), file("draft.txt"), image(), file("old.txt")
	day := time.Now().Add(-25 * time.Hour)
	for _, p := range []string{filepath.Join(m.st.UploadsDir(), oldImg), filepath.Join(m.st.FilesDir(), oldFile)} {
		if err := os.Chtimes(p, day, day); err != nil {
			t.Fatal(err)
		}
	}
	m.sweepAttachments()
	for ref, want := range map[string]bool{draftImg: true, draftFile: true, oldImg: false, oldFile: false, bobsPhoto: true, notes: true} {
		if m.testHas(t, ref) != want {
			t.Errorf("after the sweep, %s kept = %v", ref, !want)
		}
	}
	m.st.DeleteSession("b1")
	m.sweepAttachments()
	if m.testHas(t, bobsPhoto) || m.testHas(t, notes) {
		t.Error("what a session that is gone held survived the sweep")
	}
	if _, err := os.Stat(m.testClaim(t, bobsPhoto)); !os.IsNotExist(err) {
		t.Errorf("claim left behind: %v", err)
	}
}

// In a container, the agent's copies in the user's home go too (files, and
// images Codex took by path).
func TestContainerCopiesGoWithTheirSession(t *testing.T) {
	m := newHomeTestManager(t)
	m.sb = sandbox.New(sandbox.Config{Podman: filepath.Join(t.TempDir(), "no-podman"), DataDir: t.TempDir()})
	m.st.PutProject(model.Project{ID: "pc", Path: t.TempDir(), OwnerUser: "alice", Runtime: "container"})
	m.st.PutSession(model.Session{ID: "c1", ProjectID: "pc", Owner: "alice", Agent: "codex"})
	home := m.sb.HomeDir("pc", "alice")
	if err := os.MkdirAll(home, 0o700); err != nil {
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
	// As send does, without starting an agent.
	if err := m.claimSent("c1", []string{img}, []model.File{f}); err != nil {
		t.Fatal(err)
	}
	putItems(t, m, "c1", &model.Item{ID: "u-1", Kind: model.KindUser, Images: []string{img}, Files: []model.File{f}, Done: true})
	if _, err := m.stageInput("c1", queued{agent.Input{Images: []agent.Image{{Path: imgPath}}}, []model.File{f}}); err != nil {
		t.Fatal(err)
	}
	copies := []string{filepath.Join(home, ".collie-uploads", f.ID), filepath.Join(home, ".collie-uploads", img)}
	for _, p := range copies {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}

	if err := m.deleteSession("c1", false); err != nil {
		t.Fatal(err)
	}
	for _, p := range copies {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("the agent's copy %s outlived the session: %v", p, err)
		}
	}
	if m.testHas(t, f.ID) || m.testHas(t, img) {
		t.Error("the attachments outlived their session")
	}
}

// Sessions from before claims existed have theirs claimed once, at start,
// so the sweep doesn't take them for uploads never sent.
func TestClaimExisting(t *testing.T) {
	m, _ := newTestManager(t)
	m.st.PutSession(model.Session{ID: "s1", ProjectID: "p1", Owner: "alice"})
	img, _ := m.SaveUpload(tinyPNG)
	shot, _ := m.SaveUpload(tinyPNG)
	f, _ := m.SaveFile("brief.docx", strings.NewReader("x"))
	for _, it := range []*model.Item{
		{ID: "u-1", Kind: model.KindUser, Images: []string{img, "0123456789abcdef.png"}, Files: []model.File{f}, Done: true},
		{ID: "t-1", Kind: model.KindTool, Images: []string{shot}, Done: true},
	} {
		if err := m.st.AppendItem("s1", it); err != nil {
			t.Fatal(err)
		}
	}
	m.claimExisting()
	for _, c := range []string{m.testClaim(t, img), m.testClaim(t, shot), m.testClaim(t, f.ID)} {
		if claimOf(c) != "s1" {
			t.Errorf("%s not claimed", c)
		}
	}
	if _, err := os.Stat(m.testClaim(t, "0123456789abcdef.png")); !os.IsNotExist(err) {
		t.Errorf("claimed a missing upload: %v", err)
	}

	// Only once.
	later, _ := m.SaveUpload(tinyPNG)
	_ = m.st.AppendItem("s1", &model.Item{ID: "u-2", Kind: model.KindUser, Images: []string{later}, Done: true})
	m.claimExisting()
	if claimOf(m.testClaim(t, later)) != "" {
		t.Error("claimed again")
	}
}
