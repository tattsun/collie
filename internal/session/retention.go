package session

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tattsun/collie/internal/model"
)

// What a session's timeline shows — images users attached, images agents
// saw in tool results, media they showed, attached files — belongs to that
// session and is deleted with it, along with the copies its agent was
// given. A claim next to each upload or file names its session:
// uploads/<name>.session, files/<id>/.session.
//
// Agents' images and media are claimed when they are saved, what users
// attach when it is sent. Something already claimed can't be sent in
// another session: whoever saw it in a shared session could otherwise send
// it in a session of their own and delete it from there. What was uploaded
// but never sent is deleted after a day.

const (
	claimExt = ".session" // after an upload's name
	// claimName is a file's claim in its folder. Stored file names never
	// start with a dot (see cleanFileName).
	claimName = ".session"
	// unsentTTL is how long an upload or file nobody sent is kept.
	unsentTTL = 24 * time.Hour
	// claimedMarker, in the uploads, says the uploads of sessions from
	// before claims existed have been claimed (claimExisting).
	claimedMarker = ".claimed"
)

var (
	errClaimed    = errors.New("claimed by another session")
	errFileTaken  = errors.New("an attached file was already sent in another session; attach it again")
	errImageTaken = errors.New("an attached image was already sent in another session; attach it again")
)

// uploadClaim is where an upload's claim is; !ok for a name that is not
// an upload's.
func (m *Manager) uploadClaim(name string) (string, bool) {
	p, ok := m.UploadPath(name)
	if !ok {
		return "", false
	}
	return p + claimExt, true
}

// fileClaim is where a file's claim is; !ok for an id that is not a file's.
func (m *Manager) fileClaim(id string) (string, bool) {
	if !fileIDRe.MatchString(id) {
		return "", false
	}
	return filepath.Join(m.st.FilesDir(), id, claimName), true
}

// claimOf is the session a claim names, or "".
func claimOf(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// claim makes what a claim stands for the session's, unless another
// session has it (errClaimed). It is linked in place, so it appears whole
// or not at all.
func claim(path, sid string) error {
	if path == "" {
		return errors.New("nothing to claim")
	}
	tmp := path + "." + model.NewID()
	if err := os.WriteFile(tmp, []byte(sid), 0o600); err != nil {
		return err
	}
	err := os.Link(tmp, path)
	_ = os.Remove(tmp)
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, fs.ErrExist):
		return err
	case claimOf(path) != sid:
		return errClaimed
	}
	return nil
}

// claimSent makes the images and files of a message the session's.
func (m *Manager) claimSent(sid string, images []string, files []model.File) error {
	for _, name := range images {
		c, ok := m.uploadClaim(name)
		if !ok {
			return fmt.Errorf("bad image id %q", name)
		}
		if err := claim(c, sid); errors.Is(err, errClaimed) {
			return errImageTaken
		} else if err != nil {
			return err
		}
	}
	for _, f := range files {
		c, ok := m.fileClaim(f.ID)
		if !ok {
			return errFileMissing
		}
		if err := claim(c, sid); errors.Is(err, errClaimed) {
			return errFileTaken
		} else if err != nil {
			return err
		}
	}
	return nil
}

// saveSessionUpload stores an image or page an agent produced in a session.
func (m *Manager) saveSessionUpload(sid string, data []byte, ext string) (string, error) {
	name, err := m.saveUpload(data, ext)
	if err != nil {
		return "", err
	}
	c, _ := m.uploadClaim(name)
	if err := claim(c, sid); err != nil {
		m.removeUpload(name)
		return "", err
	}
	return name, nil
}

func (m *Manager) removeUpload(name string) {
	if p, ok := m.UploadPath(name); ok {
		_ = os.Remove(p)
		_ = os.Remove(p + claimExt)
	}
}

// refs is what a session's timeline shows from the uploads and files.
type refs struct {
	uploads []string
	files   []model.File
}

func (rf *refs) add(it *model.Item) {
	rf.uploads = append(rf.uploads, it.Images...)
	for _, md := range it.Media {
		rf.uploads = append(rf.uploads, md.ID)
	}
	rf.files = append(rf.files, it.Files...)
}

func (r *Runtime) refs() refs {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureLoaded()
	var rf refs
	for _, it := range r.items {
		rf.add(it)
	}
	return rf
}

// deleteAttachments deletes what a deleted session's timeline showed, if
// it was the session's, and the copies its agent was given.
func (m *Manager) deleteAttachments(s model.Session, rf refs) {
	p, _ := m.st.Project(s.ProjectID)
	staged := func(name string) {
		if p.Isolated() && m.sb != nil {
			if err := m.sb.RemoveStaged(p.ID, s.Owner, stageDir+"/"+name); err != nil {
				log.Printf("session %s: delete the agent's copy %s: %v", s.ID, name, err)
			}
		}
	}
	for _, name := range rf.uploads {
		if c, ok := m.uploadClaim(name); !ok || claimOf(c) != s.ID {
			continue
		}
		staged(name) // Codex takes images by path
		m.removeUpload(name)
	}
	for _, f := range rf.files {
		if c, ok := m.fileClaim(f.ID); !ok || claimOf(c) != s.ID {
			continue
		}
		staged(f.ID)
		if err := os.RemoveAll(filepath.Join(m.st.FilesDir(), f.ID)); err != nil {
			log.Printf("session %s: delete %s: %v", s.ID, f.Name, err)
		}
	}
	_ = os.RemoveAll(m.hostStageDir(s.ID))
}

// sweepAttachments deletes uploads and files no session holds: ones never
// sent (the composer that uploaded them is long gone), and ones whose
// session was deleted while they were being saved or sent.
func (m *Manager) sweepAttachments() {
	gone := func(claim string, fi fs.FileInfo) bool {
		if sid := claimOf(claim); sid != "" {
			_, ok := m.st.Session(sid)
			return !ok
		}
		return fi != nil && time.Since(fi.ModTime()) > unsentTTL
	}
	if ents, err := os.ReadDir(m.st.UploadsDir()); err == nil {
		for _, e := range ents {
			name := e.Name()
			if _, ok := m.UploadPath(name); !ok {
				if base, isClaim := strings.CutSuffix(name, claimExt); isClaim {
					if _, err := os.Stat(filepath.Join(m.st.UploadsDir(), base)); errors.Is(err, fs.ErrNotExist) {
						_ = os.Remove(filepath.Join(m.st.UploadsDir(), name))
					}
				}
				continue
			}
			c, _ := m.uploadClaim(name)
			fi, _ := e.Info()
			if gone(c, fi) {
				m.removeUpload(name)
			}
		}
	}
	if ents, err := os.ReadDir(m.st.FilesDir()); err == nil {
		for _, e := range ents {
			if !e.IsDir() || !fileIDRe.MatchString(e.Name()) {
				continue
			}
			c, _ := m.fileClaim(e.Name())
			fi, _ := e.Info()
			if gone(c, fi) {
				_ = os.RemoveAll(filepath.Join(m.st.FilesDir(), e.Name()))
			}
		}
	}
}

// claimExisting claims, once, what the timelines of sessions from before
// claims existed show, so that it is deleted with them and not taken for
// something never sent. It runs before Collie serves anyone.
func (m *Manager) claimExisting() {
	marker := filepath.Join(m.st.UploadsDir(), claimedMarker)
	if _, err := os.Stat(marker); err == nil {
		return
	}
	start, n := time.Now(), 0
	for _, s := range m.st.Sessions() {
		items, _, err := m.st.LoadItems(s.ID)
		if err != nil {
			continue
		}
		var rf refs
		for _, it := range items {
			rf.add(it)
		}
		for _, name := range rf.uploads {
			if p, ok := m.UploadPath(name); !ok {
				continue
			} else if _, err := os.Stat(p); err == nil && claim(p+claimExt, s.ID) == nil {
				n++
			}
		}
		for _, f := range rf.files {
			if c, ok := m.fileClaim(f.ID); ok && claim(c, s.ID) == nil {
				n++
			}
		}
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		log.Printf("claim existing uploads: %v", err)
		return
	}
	if n > 0 {
		log.Printf("claimed %d images and files of existing sessions in %s", n, time.Since(start).Round(time.Millisecond))
	}
}
