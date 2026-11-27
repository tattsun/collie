package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tattsun/collie/internal/model"
)

func TestTaskOutputPath(t *testing.T) {
	const p = "/tmp/claude-1002/-home-collie--collie-worktrees-52460f4e/b60acd9f-58b0/tasks/bby1oprzi.output"
	cases := []struct {
		result, id, want string
	}{
		{"Command running in background with ID: bby1oprzi. Output is being written to: " + p + ". You will be notified when it completes.", "bby1oprzi", p},
		{"Command did not complete within its 120s timeout and was moved to the background (ID: bby1oprzi). Output is being written to: " + p + ".", "bby1oprzi", p},
		{"Command was manually backgrounded by user with ID: bby1oprzi. Output is being written to: " + p + ".", "bby1oprzi", p},
		{"Output is being written to " + p + "; use Read on that path for interim output.", "bby1oprzi", p},
		// another task's file, or none at all
		{"Output is being written to: " + p + ".", "other", ""},
		{"Command running in background with ID: bby1oprzi.", "bby1oprzi", ""},
		// only a plain path to the task's own file
		{"Output is being written to: /tmp/../etc/bby1oprzi.output.", "bby1oprzi", ""},
		{"Output is being written to: tasks/bby1oprzi.output.", "bby1oprzi", ""},
		{"Output is being written to: /tmp/a b/bby1oprzi.output.", "bby1oprzi", ""},
		{"Output is being written to: " + p + ".", "../bby1oprzi", ""},
		// the first that fits wins
		{"Output is being written to: /x/y.output. Output is being written to: " + p + ".", "bby1oprzi", p},
	}
	for _, c := range cases {
		if got := taskOutputPath(c.result, c.id); got != c.want {
			t.Errorf("taskOutputPath(%q, %q) = %q, want %q", c.result, c.id, got, c.want)
		}
	}
}

func TestTerminalText(t *testing.T) {
	cases := []struct {
		in   string
		cut  bool
		want string
	}{
		{"plain\nlines\n", false, "plain\nlines\n"},
		{"\x1b[1;32mok\x1b[0m done\r\n", false, "ok done\n"},
		{"\x1b]0;title\x07after", false, "after"},
		{"10%\r20%\r30%\nnext", false, "30%\nnext"},
		{"50%\r", false, "50%"},
		{"tab\tbell\x07bs\x08\x7f", false, "tab\tbellbs"},
		{"rtial line\nwhole line\n", true, "whole line\n"},
		{"\xa8\xe5\xad\x97", true, "字"}, // a cut UTF-8 sequence at the start
		{"bad \xff byte", false, "bad � byte"},
	}
	for _, c := range cases {
		if got := terminalText([]byte(c.in), c.cut); got != c.want {
			t.Errorf("terminalText(%q, %v) = %q, want %q", c.in, c.cut, got, c.want)
		}
	}
}

func TestTaskOutput(t *testing.T) {
	m, _ := newTestManager(t)
	work := t.TempDir()
	out := filepath.Join(work, "tasks", "b1.output")
	_ = os.MkdirAll(filepath.Dir(out), 0o700)
	if err := os.WriteFile(out, []byte("\x1b[32mbuilding\x1b[0m\r\nlistening on :7931\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.st.PutProject(model.Project{ID: "p1", Name: "web", Path: work, OwnerUser: "root"}) // host-run: admins only
	m.st.PutSession(model.Session{ID: "s1", ProjectID: "p1", Owner: "root", Cwd: work, Tasks: []model.Task{
		{ID: "b1", Kind: "local_bash", ToolID: "toolu_1"},
		{ID: "b2", Kind: "local_bash", ToolID: "toolu_2"},
		{ID: "a3", Kind: "local_agent", ToolID: "toolu_3"},
	}})
	for id, res := range map[string]string{
		"toolu_1": fmt.Sprintf("Command running in background with ID: b1. Output is being written to: %s. You will be notified when it completes.", out),
		"toolu_2": "Command running in background with ID: b2.",
		"toolu_3": "Async agent launched successfully.",
	} {
		if err := m.st.AppendItem("s1", &model.Item{ID: id, Kind: model.KindTool, Done: true, Tool: &model.Tool{Name: "Bash", Output: res, Status: "ok"}}); err != nil {
			t.Fatal(err)
		}
	}
	root := Actor{ID: "root", Admin: true}

	got, err := m.TaskOutput(root, "s1", "b1")
	if err != nil || got.Text != "building\nlistening on :7931\n" || got.More {
		t.Fatalf("TaskOutput = %+v, %v", got, err)
	}
	// Tasks without a known output file, or not running in the session.
	for _, id := range []string{"b2", "a3", "nosuch"} {
		if got, err := m.TaskOutput(root, "s1", id); err == nil {
			t.Errorf("output of %s: %+v", id, got)
		}
	}
	// A host-run agent's files are for admins only.
	if got, err := m.TaskOutput(Actor{ID: "alice"}, "s1", "b1"); err == nil {
		t.Errorf("non-admin read a host file: %+v", got)
	}

	// Long output: only its end, from a whole line.
	long := strings.Repeat("0123456789abcdef\n", taskOutputMax/17+100) + "last\n"
	if err := os.WriteFile(out, []byte(long), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = m.TaskOutput(root, "s1", "b1")
	if err != nil || !got.More || !strings.HasSuffix(got.Text, "\nlast\n") || !strings.HasPrefix(got.Text, "0123456789abcdef\n") || len(got.Text) > taskOutputMax {
		t.Fatalf("long output: more=%v len=%d %v", got.More, len(got.Text), err)
	}
}
