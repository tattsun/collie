package agent

import (
	"slices"
	"testing"

	"github.com/tattsun/collie/internal/model"
)

type taskSink struct {
	capsSink
	sess     model.Session
	notices  []string
	statuses []string
}

func (s *taskSink) Meta(fn func(*model.Session)) { fn(&s.sess) }
func (s *taskSink) Status(st string)             { s.statuses = append(s.statuses, st) }
func (s *taskSink) Put(it *model.Item) {
	if it.Kind == model.KindNotice {
		s.notices = append(s.notices, it.Text)
	}
}

func taskTitles(s *taskSink) []string {
	var out []string
	for _, t := range s.sess.Tasks {
		out = append(out, t.Title)
	}
	return out
}

func TestClaudeBackgroundTasks(t *testing.T) {
	s := &taskSink{}
	c := NewClaude(Config{}, s)
	line := func(j string) { c.onLine([]byte(j)) }

	// A foreground command is a task too, but not one to list.
	line(`{"type":"system","subtype":"task_started","task_id":"b1","task_type":"local_bash","description":"Run tests","tool_use_id":"toolu_1","is_backgrounded":false}`)
	if len(s.sess.Tasks) != 0 {
		t.Fatalf("foreground task listed: %v", taskTitles(s))
	}
	// ...until it is sent to the background.
	line(`{"type":"system","subtype":"task_updated","task_id":"b1","patch":{"is_backgrounded":true}}`)
	if got := taskTitles(s); !slices.Equal(got, []string{"Run tests"}) {
		t.Fatalf("after backgrounding: %v", got)
	}
	if s.sess.Tasks[0].ToolID != "toolu_1" || s.sess.Tasks[0].Kind != "local_bash" || s.sess.Tasks[0].Started == 0 {
		t.Fatalf("task = %+v", s.sess.Tasks[0])
	}

	// One started in the background, with no flag at all (task types that
	// only run in the background).
	line(`{"type":"system","subtype":"task_started","task_id":"a2","task_type":"local_agent","description":"Review the diff","is_backgrounded":true}`)
	line(`{"type":"system","subtype":"task_started","task_id":"m3","task_type":"monitor","description":"Watch the log"}`)
	if got := taskTitles(s); len(got) != 3 {
		t.Fatalf("tasks = %v", got)
	}
	line(`{"type":"system","subtype":"task_progress","task_id":"a2","summary":"Reading internal/agent"}`)
	if i := slices.IndexFunc(s.sess.Tasks, func(x model.Task) bool { return x.ID == "a2" }); i < 0 || s.sess.Tasks[i].Summary != "Reading internal/agent" {
		t.Fatalf("progress not kept: %+v", s.sess.Tasks)
	}

	// Ending: once by task_updated and then task_notification for the same
	// task (one notice), once by task_notification alone.
	line(`{"type":"system","subtype":"task_updated","task_id":"a2","patch":{"status":"failed"}}`)
	line(`{"type":"system","subtype":"task_notification","task_id":"a2","status":"failed"}`)
	line(`{"type":"system","subtype":"task_notification","task_id":"b1","status":"completed"}`)
	if got := taskTitles(s); !slices.Equal(got, []string{"Watch the log"}) {
		t.Fatalf("after ending: %v", got)
	}
	want := []string{"Background task failed: Review the diff", "Background task finished: Run tests"}
	if !slices.Equal(s.notices, want) {
		t.Fatalf("notices = %q, want %q", s.notices, want)
	}

	// A foreground task ending says nothing: its tool call shows it.
	line(`{"type":"system","subtype":"task_started","task_id":"b4","task_type":"local_bash","description":"ls","is_backgrounded":false}`)
	line(`{"type":"system","subtype":"task_notification","task_id":"b4","status":"completed"}`)
	if len(s.notices) != 2 {
		t.Fatalf("notices = %q", s.notices)
	}
	line(`{"type":"system","subtype":"task_notification","task_id":"m3","status":"killed"}`)
	if len(s.sess.Tasks) != 0 || s.notices[2] != "Background task stopped: Watch the log" {
		t.Fatalf("tasks = %v, notices = %q", taskTitles(s), s.notices)
	}
}

func TestClaudeSelfStartedTurn(t *testing.T) {
	s := &taskSink{}
	c := NewClaude(Config{}, s)
	// A sub-agent's message is not the agent waking up.
	c.onLine([]byte(`{"type":"stream_event","parent_tool_use_id":"toolu_9","event":{"type":"message_start","message":{"id":"m0"}}}`))
	if len(s.statuses) != 0 {
		t.Fatalf("statuses = %v", s.statuses)
	}
	// With no turn going, a reply means the agent started one itself.
	c.onLine([]byte(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m1"}}}`))
	c.onLine([]byte(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m2"}}}`))
	if !slices.Equal(s.statuses, []string{model.StatusRunning}) {
		t.Fatalf("statuses = %v", s.statuses)
	}
}
