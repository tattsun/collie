package session

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tattsun/collie/internal/agent"
	"github.com/tattsun/collie/internal/model"
)

// Attached files: besides images, which reach the agent inline, users
// attach any other file to a message (a Word brief, a spreadsheet, a PDF).
// Collie keeps it as files/<id>/<name>. When the message is delivered, the
// agent gets a copy where it can read it, and the message lists the paths:
// in its container's home for isolated projects, in a folder of the session
// for host-run ones. Claude Code is given that folder as a working
// directory, so reading there needs no approval.

const (
	MaxFile        = 25 << 20 // bytes per attached file
	maxMsgFiles    = 20       // files per message
	maxFileNameLen = 200      // bytes, under every file system's limit
	// stageDir holds the copies in a container's home.
	stageDir = ".collie-uploads"
)

var (
	fileIDRe       = regexp.MustCompile(`^[0-9a-f]{32}$`)
	ErrFileTooBig  = fmt.Errorf("files can be up to %d MB", MaxFile>>20)
	errManyFiles   = fmt.Errorf("at most %d files per message", maxMsgFiles)
	errFileMissing = errors.New("an attached file is no longer on the server; attach it again")
)

// cleanFileName makes a name the user's browser sent safe to keep and to
// hand to an agent: no directories, control or invisible formatting
// characters (which could disguise an extension), not hidden, not taken
// for a command-line option, and short enough for any file system.
func cleanFileName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(r)
	}
	name = strings.TrimLeft(strings.TrimSpace(b.String()), ".- ")
	name = strings.TrimRight(name, ". ")
	if len(name) > maxFileNameLen {
		ext := filepath.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		stem := strings.TrimSuffix(name, ext)
		cut := maxFileNameLen - len(ext)
		for cut > 0 && !utf8.RuneStart(stem[cut]) {
			cut--
		}
		name = strings.TrimRight(stem[:cut], ". ") + ext
	}
	if name == "" {
		name = "file"
	}
	return name
}

// SaveFile stores a file the user attached (up to MaxFile bytes).
func (m *Manager) SaveFile(name string, r io.Reader) (model.File, error) {
	f := model.File{ID: model.NewID() + model.NewID(), Name: cleanFileName(name)}
	dir := filepath.Join(m.st.FilesDir(), f.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return f, err
	}
	out, err := os.OpenFile(filepath.Join(dir, f.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		f.Size, err = io.Copy(out, io.LimitReader(r, MaxFile+1))
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err == nil && f.Size > MaxFile {
			err = ErrFileTooBig
		}
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return f, err
	}
	return f, nil
}

// AttachedFile finds a stored file and where it is on the server.
func (m *Manager) AttachedFile(id string) (string, model.File, bool) {
	if !fileIDRe.MatchString(id) {
		return "", model.File{}, false
	}
	dir := filepath.Join(m.st.FilesDir(), id)
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 1 || !ents[0].Type().IsRegular() {
		return "", model.File{}, false
	}
	fi, err := ents[0].Info()
	if err != nil {
		return "", model.File{}, false
	}
	return filepath.Join(dir, ents[0].Name()), model.File{ID: id, Name: ents[0].Name(), Size: fi.Size()}, true
}

// attachedFiles looks up the files attached to a message.
func (m *Manager) attachedFiles(ids []string) ([]model.File, error) {
	if len(ids) > maxMsgFiles {
		return nil, errManyFiles
	}
	var out []model.File
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		_, f, ok := m.AttachedFile(id)
		if !ok {
			return nil, errFileMissing
		}
		out = append(out, f)
	}
	return out, nil
}

// hostStageDir holds the copies for a host-run session's agent.
func (m *Manager) hostStageDir(sid string) string {
	return filepath.Join(m.st.Dir(), "attach", sid)
}

// stageRoot creates the folder the session's agent finds attached files
// in and returns its path as the agent sees it.
func (m *Manager) stageRoot(p model.Project, s model.Session) (string, error) {
	if p.Isolated() {
		if m.sb == nil {
			return "", errors.New("podman is not available")
		}
		return m.sb.StageDir(p.ID, s.Owner, stageDir)
	}
	dir := m.hostStageDir(s.ID)
	return dir, os.MkdirAll(dir, 0o700)
}

// stageInput prepares a message for delivery once the agent is up (and
// with it the container's home): copies of the attached files where the
// agent can read them, listed after the text, and for Codex in a container
// the images, which it takes by path.
func (m *Manager) stageInput(sid string, q queued) (agent.Input, error) {
	in := q.Input
	if len(q.files) == 0 && len(in.Images) == 0 {
		return in, nil
	}
	s, ok := m.st.Session(sid)
	if !ok {
		return in, errSessionGone
	}
	p, ok := m.st.Project(s.ProjectID)
	if !ok {
		return in, errSessionGone
	}
	if s.Agent == "codex" && p.Isolated() && m.sb != nil {
		in.Images = append([]agent.Image(nil), in.Images...)
		for i := range in.Images {
			ap, err := m.sb.StageUpload(p.ID, s.Owner, in.Images[i].Path, stageDir+"/"+filepath.Base(in.Images[i].Path))
			if err != nil {
				return in, err
			}
			in.Images[i].AgentPath = ap
		}
	}
	if len(q.files) == 0 {
		return in, nil
	}
	root, err := m.stageRoot(p, s)
	if err != nil {
		return in, err
	}
	var lines []string
	for _, f := range q.files {
		src, _, ok := m.AttachedFile(f.ID)
		if !ok {
			return in, errFileMissing
		}
		var path string
		if p.Isolated() {
			path, err = m.sb.StageUpload(p.ID, s.Owner, src, stageDir+"/"+f.ID+"/"+f.Name)
		} else {
			path = filepath.Join(root, f.ID, f.Name)
			err = copyFile(src, path)
		}
		if err != nil {
			return in, fmt.Errorf("copy %s for the agent: %w", f.Name, err)
		}
		lines = append(lines, fmt.Sprintf("- %s (%s)", path, sizeLabel(f.Size)))
	}
	note := "The user attached these files (Collie saved copies for you to read):\n" + strings.Join(lines, "\n")
	if in.Text == "" {
		in.Text = note
	} else {
		in.Text += "\n\n" + note
	}
	return in, nil
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".collie-tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func sizeLabel(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d bytes", n)
	case n < 1<<20:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

// claudeStageArgs lets Claude Code read the attached files unasked (and in
// modes that never ask): their folder becomes one of its working
// directories.
func (m *Manager) claudeStageArgs(p model.Project, s model.Session) []string {
	dir, err := m.stageRoot(p, s)
	if err != nil {
		return nil
	}
	return []string{"--add-dir", dir}
}
