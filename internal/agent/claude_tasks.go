package agent

import (
	"cmp"
	"slices"
	"time"

	"github.com/tattsun/collie/internal/model"
)

// Background tasks. Claude Code runs some work as tasks: background
// commands, sub-agents, monitors, workflows. Its turn can end while they
// run on; when one ends it tells the agent, which starts a turn of its own
// to deal with it. The CLI reports them with system events:
//
//	task_started       task_id, task_type, description, tool_use_id, is_backgrounded
//	task_progress      task_id, description, summary
//	task_updated       task_id, patch{status, description, is_backgrounded, ...}
//	task_notification  task_id, status, summary (the task is over)
//
// Foreground commands and sub-agents are tasks too (is_backgrounded false)
// so that they can be sent to the background later; the session lists
// only the backgrounded ones, which are the ones nothing else shows.

type claudeTask struct {
	model.Task
	bg bool
}

// taskOver are the statuses after which a task does nothing more.
var taskOver = map[string]bool{
	"completed": true, "failed": true, "killed": true, "stopped": true,
	"cancelled": true, "canceled": true, "error": true,
}

func (c *Claude) onTask(o map[string]any) {
	id := str(o, "task_id")
	if id == "" {
		return
	}
	var notice string
	c.mu.Lock()
	t := c.tasks[id]
	switch str(o, "subtype") {
	case "task_started":
		bg, known := o["is_backgrounded"].(bool)
		t = &claudeTask{bg: bg || !known, Task: model.Task{
			ID: id, Kind: str(o, "task_type"), Title: str(o, "description"),
			ToolID: str(o, "tool_use_id"), Started: model.NowMs(),
		}}
		c.tasks[id] = t
	case "task_progress":
		if t != nil {
			if d := str(o, "description"); d != "" {
				t.Title = d
			}
			if s := str(o, "summary"); s != "" {
				t.Summary = truncate(s, 200)
			}
		}
	case "task_updated":
		p := obj(o, "patch")
		if t != nil && p != nil {
			if d := str(p, "description"); d != "" {
				t.Title = d
			}
			if bg, ok := p["is_backgrounded"].(bool); ok {
				t.bg = bg
			}
			if st := str(p, "status"); taskOver[st] {
				notice = taskEndNotice(t, st)
				delete(c.tasks, id)
			}
		}
	case "task_notification":
		if t != nil {
			notice = taskEndNotice(t, str(o, "status"))
			delete(c.tasks, id)
		}
	}
	list := c.taskList()
	changed := !slices.Equal(list, c.shownTasks)
	if changed {
		c.shownTasks = list
	}
	c.mu.Unlock()
	if changed {
		c.sink.Meta(func(s *model.Session) { s.Tasks = list })
	}
	if notice != "" {
		c.sink.Put(&model.Item{ID: model.NewID(), Kind: model.KindNotice, TS: model.NowMs(), Done: true, Text: notice})
	}
}

// taskList returns the backgrounded tasks, oldest first. c.mu must be held.
func (c *Claude) taskList() []model.Task {
	var list []model.Task
	for _, t := range c.tasks {
		if t.bg {
			list = append(list, t.Task)
		}
	}
	slices.SortFunc(list, func(a, b model.Task) int {
		return cmp.Or(cmp.Compare(a.Started, b.Started), cmp.Compare(a.ID, b.ID))
	})
	return list
}

// taskEndNotice is the timeline note for a background task that ended (the
// agent goes on to report on it), or "" for a foreground one: its tool
// call already shows how it went.
func taskEndNotice(t *claudeTask, status string) string {
	if !t.bg {
		return ""
	}
	title := t.Title
	if title == "" {
		title = t.Kind
	}
	switch status {
	case "failed", "error":
		return "Background task failed: " + title
	case "killed", "stopped", "cancelled", "canceled":
		return "Background task stopped: " + title
	}
	return "Background task finished: " + title
}

// StopTask asks Claude Code to stop one of its tasks.
func (c *Claude) StopTask(id string) error {
	_, err := c.control(map[string]any{"subtype": "stop_task", "task_id": id}, 15*time.Second)
	return err
}
