package session

import (
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/tattsun/collie/internal/model"
)

// Output of background commands. Claude Code writes what a background
// command prints to a file and names it in the result of the tool call
// that started the command ("Command running in background with ID: <id>.
// Output is being written to: /tmp/claude-1000/…/tasks/<id>.output."), so
// the user can look at its end while it runs.

// TaskOutput is the end of what a background command has printed so far.
type TaskOutput struct {
	Text string `json:"text"`
	More bool   `json:"more,omitempty"` // it printed more before Text
}

const taskOutputMax = 64 << 10

var errNoTaskOutput = errors.New("this task's output cannot be shown")

var (
	taskOutputRe = regexp.MustCompile(`Output is being written to:? (/\S+)`)
	taskIDRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	pathPartRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

// taskOutputPath finds where a task's output is written in the result of
// the tool call that started it, or returns "". Only a plain absolute path
// to the task's own output file is taken (as Claude Code itself checks).
func taskOutputPath(result, taskID string) string {
	if !taskIDRe.MatchString(taskID) {
		return ""
	}
	for _, m := range taskOutputRe.FindAllStringSubmatch(result, -1) {
		p := strings.TrimRight(m[1], ".;,")
		if !strings.HasSuffix(p, "/"+taskID+".output") {
			continue
		}
		ok := true
		for _, part := range strings.Split(p[1:], "/") {
			if part == "." || part == ".." || !pathPartRe.MatchString(part) {
				ok = false
				break
			}
		}
		if ok {
			return p
		}
	}
	return ""
}

// TaskOutput returns the end of what one of the agent's background
// commands has printed so far.
func (m *Manager) TaskOutput(a Actor, sid, taskID string) (TaskOutput, error) {
	s, err := m.visibleSession(a.ID, sid)
	if err != nil {
		return TaskOutput{}, err
	}
	if err := m.hostCheck(a, s); err != nil {
		return TaskOutput{}, err
	}
	toolID := ""
	for _, t := range s.Tasks {
		if t.ID == taskID {
			toolID = t.ToolID
		}
	}
	if toolID == "" {
		return TaskOutput{}, errNoTaskOutput
	}
	r, err := m.runtime(sid)
	if err != nil {
		return TaskOutput{}, err
	}
	result := ""
	r.mu.Lock()
	r.ensureLoaded()
	if i, ok := r.idx[toolID]; ok && r.items[i].Tool != nil {
		result = r.items[i].Tool.Output
	}
	r.mu.Unlock()
	path := taskOutputPath(result, taskID)
	if path == "" {
		return TaskOutput{}, errNoTaskOutput
	}
	p, ok := m.st.Project(s.ProjectID)
	if !ok {
		return TaskOutput{}, ErrNotFound
	}
	data, more, err := m.tailAgentFile(p, s, path, taskOutputMax)
	if err != nil {
		return TaskOutput{}, err
	}
	return TaskOutput{Text: terminalText(data, more), More: more}, nil
}

// tailAgentFile reads the last limit bytes of a file as the session's
// agent sees it; more reports that the file holds more before them.
func (m *Manager) tailAgentFile(p model.Project, s model.Session, path string, limit int64) ([]byte, bool, error) {
	if p.Isolated() {
		if m.sb == nil || !m.sb.Available() {
			return nil, false, errors.New("podman is not available")
		}
		return m.sb.TailFile(p.ID, s.Owner, path, limit)
	}
	if !m.isAdmin(s.Owner) {
		return nil, false, errHostOnly
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !fi.Mode().IsRegular() {
		return nil, false, errors.New("not a regular file")
	}
	more := fi.Size() > limit
	if more {
		if _, err := f.Seek(fi.Size()-limit, io.SeekStart); err != nil {
			return nil, false, err
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	return data, more, err
}

var ansiRe = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)

// terminalText turns what a command printed into plain text: without
// terminal escapes and other control characters, each line as it was last
// redrawn (progress bars rewrite theirs after a carriage return). When the
// output was cut, the partial first line goes.
func terminalText(b []byte, cut bool) string {
	s := string(b)
	if cut {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		} else {
			for len(s) > 0 && !utf8.RuneStart(s[0]) {
				s = s[1:]
			}
		}
	}
	s = strings.ToValidUTF8(ansiRe.ReplaceAllString(s, ""), "�")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		l = strings.TrimRight(l, "\r")
		if j := strings.LastIndexByte(l, '\r'); j >= 0 {
			l = l[j+1:]
		}
		lines[i] = strings.Map(func(r rune) rune {
			if r < 0x20 && r != '\t' || r == 0x7f {
				return -1
			}
			return r
		}, l)
	}
	return strings.Join(lines, "\n")
}
