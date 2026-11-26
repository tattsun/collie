package agent

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tattsun/collie/internal/model"
)

// Claude drives `claude -p` using the stream-json stdio protocol (the same
// protocol the Agent SDK speaks), including the control channel used for
// permission prompts, interrupts and mode changes.
type Claude struct {
	cfg  Config
	sink Sink
	p    *proc

	mu        sync.Mutex
	seq       int
	waiters   map[string]chan map[string]any
	pending   map[string]*claudeAsk
	order     []string
	blocks    map[string][]*streamBlock // message id -> blocks in stream order
	curMsg    map[string]string         // parent tool id -> streaming message id
	byIdx     map[string]*streamBlock   // parent:index -> block (for deltas)
	textSeq   int
	lastCost  float64
	busy      bool
	ctxWindow int64
}

type streamBlock struct {
	id      string
	typ     string
	matched bool
}

type claudeAsk struct {
	reqID       string
	kind        string // approval | question | plan
	tool        string
	input       map[string]any
	suggestions []any
	questions   []model.Question
}

func NewClaude(cfg Config, sink Sink) *Claude {
	return &Claude{
		cfg:     cfg,
		sink:    sink,
		waiters: map[string]chan map[string]any{},
		pending: map[string]*claudeAsk{},
		blocks:  map[string][]*streamBlock{},
		curMsg:  map[string]string{},
		byIdx:   map[string]*streamBlock{},
	}
}

func claudeCLIMode(mode string) string {
	if mode == "" || mode == "default" {
		return "manual"
	}
	return mode
}

func (c *Claude) Start() error {
	args := []string{
		"-p", "--output-format", "stream-json", "--input-format", "stream-json",
		"--verbose", "--include-partial-messages",
		"--permission-prompt-tool", "stdio",
		"--allow-dangerously-skip-permissions",
		"--permission-mode", claudeCLIMode(c.cfg.Mode),
	}
	if c.cfg.Model != "" && c.cfg.Model != "default" {
		args = append(args, "--model", c.cfg.Model)
	}
	if c.cfg.Effort != "" {
		args = append(args, "--effort", c.cfg.Effort)
	}
	if c.cfg.Resume {
		args = append(args, "--resume", c.cfg.Ref)
	} else {
		args = append(args, "--session-id", c.cfg.Ref)
	}
	args = append(args, c.cfg.ExtraArgs...)
	p, err := startProc(c.cfg, c.cfg.Bin, args, c.onLine)
	if err != nil {
		return err
	}
	c.p = p
	go func() {
		<-p.done
		c.mu.Lock()
		for _, ch := range c.waiters {
			close(ch)
		}
		c.waiters = map[string]chan map[string]any{}
		c.mu.Unlock()
		c.sink.Exited(p.exitError())
	}()
	resp, err := c.control(map[string]any{"subtype": "initialize", "hooks": nil}, 2*time.Minute)
	if err != nil {
		p.close()
		if e := p.exitError(); e != nil {
			return e
		}
		return err
	}
	c.sink.Caps(parseClaudeCaps(resp))
	return nil
}

func parseClaudeCaps(resp map[string]any) model.Caps {
	var caps model.Caps
	for _, x := range arr(resp, "commands") {
		if m, ok := x.(map[string]any); ok && str(m, "name") != "" {
			caps.Commands = append(caps.Commands, model.Command{Name: str(m, "name"), Description: str(m, "description"), Hint: str(m, "argumentHint")})
		}
	}
	for _, x := range arr(resp, "models") {
		if m, ok := x.(map[string]any); ok && str(m, "value") != "" {
			caps.Models = append(caps.Models, model.ModelOpt{ID: str(m, "value"), Label: str(m, "displayName"), Description: str(m, "description")})
		}
	}
	return caps
}

// control sends a control_request and waits for its response payload.
func (c *Claude) control(req map[string]any, timeout time.Duration) (map[string]any, error) {
	c.mu.Lock()
	c.seq++
	id := fmt.Sprintf("collie-%d", c.seq)
	ch := make(chan map[string]any, 1)
	c.waiters[id] = ch
	c.mu.Unlock()
	if err := c.p.writeJSON(map[string]any{"type": "control_request", "request_id": id, "request": req}); err != nil {
		return nil, err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, errors.New("agent process exited")
		}
		if str(r, "subtype") == "error" {
			return nil, fmt.Errorf("claude: %s", str(r, "error"))
		}
		resp := obj(r, "response")
		if resp == nil {
			resp = map[string]any{}
		}
		return resp, nil
	case <-time.After(timeout):
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
		return nil, errors.New("timed out waiting for claude")
	}
}

func (c *Claude) Done() <-chan struct{} { return c.p.done }

func (c *Claude) Close() {
	if c.p != nil {
		c.p.close()
	}
}

func (c *Claude) SetMode(mode string) error {
	_, err := c.control(map[string]any{"subtype": "set_permission_mode", "mode": mode}, 15*time.Second)
	return err
}

func (c *Claude) SetModel(m string) error {
	var v any = m
	if m == "" || m == "default" {
		v = nil
	}
	_, err := c.control(map[string]any{"subtype": "set_model", "model": v}, 15*time.Second)
	return err
}

func (c *Claude) Interrupt() error {
	// Unblock anything waiting on the user first.
	c.mu.Lock()
	ids := append([]string(nil), c.order...)
	c.mu.Unlock()
	for _, id := range ids {
		_ = c.reply(id, Reply{Option: "deny", Text: "Interrupted by the user."}, true)
	}
	_, err := c.control(map[string]any{"subtype": "interrupt"}, 15*time.Second)
	return err
}

func (c *Claude) Send(in Input) error {
	c.mu.Lock()
	ids := append([]string(nil), c.order...)
	c.mu.Unlock()
	// A message typed while the agent is waiting on a decision rejects the
	// pending request and is delivered to Claude as the reason.
	if len(ids) > 0 {
		for i, id := range ids {
			msg := "The user rejected this request."
			if i == 0 && in.Text != "" {
				msg = "The user rejected this request and replied: " + in.Text
			}
			_ = c.reply(id, Reply{Option: "deny", Text: msg}, false)
		}
		if len(in.Images) == 0 {
			return nil
		}
	}
	var content []any
	for _, img := range in.Images {
		b, err := os.ReadFile(img.Path)
		if err != nil {
			return err
		}
		content = append(content, map[string]any{
			"type":   "image",
			"source": map[string]any{"type": "base64", "media_type": img.Mime, "data": base64.StdEncoding.EncodeToString(b)},
		})
	}
	if in.Text != "" {
		content = append(content, map[string]any{"type": "text", "text": in.Text})
	}
	if len(content) == 0 {
		return nil
	}
	err := c.p.writeJSON(map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": content},
		"parent_tool_use_id": nil,
		"session_id":         "",
	})
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.busy = true
	c.mu.Unlock()
	c.sink.Status(model.StatusRunning)
	return nil
}

func (c *Claude) Reply(askID string, r Reply) error { return c.reply(askID, r, false) }

func (c *Claude) reply(askID string, r Reply, interrupt bool) error {
	c.mu.Lock()
	a, ok := c.pending[askID]
	if ok {
		delete(c.pending, askID)
		c.order = removeStr(c.order, askID)
	}
	left := len(c.pending)
	busy := c.busy
	c.mu.Unlock()
	if !ok {
		return errors.New("this request is no longer pending")
	}

	allow := func(input map[string]any, perms []any) map[string]any {
		m := map[string]any{"behavior": "allow", "updatedInput": input}
		if len(perms) > 0 {
			m["updatedPermissions"] = perms
		}
		return m
	}
	deny := func(msg string) map[string]any {
		m := map[string]any{"behavior": "deny", "message": msg}
		if interrupt {
			m["interrupt"] = true
		}
		return m
	}
	var resp map[string]any
	var label string
	switch a.kind {
	case "question":
		if r.Option == "deny" {
			resp, label = deny(orDefault(r.Text, "The user declined to answer.")), "Declined"
			break
		}
		answers := map[string]any{}
		var parts []string
		for _, q := range a.questions {
			v := strings.Join(r.Answers[q.ID], ", ")
			answers[q.ID] = v
			parts = append(parts, v)
		}
		in := cloneMap(a.input)
		in["answers"] = answers
		resp, label = allow(in, nil), strings.Join(parts, " / ")
	case "plan":
		switch r.Option {
		case "approve_accept":
			resp = allow(a.input, []any{map[string]any{"type": "setMode", "mode": "acceptEdits", "destination": "session"}})
			label = "Approved · auto-accept edits"
			c.sink.Meta(func(s *model.Session) { s.Mode = "acceptEdits" })
		case "approve":
			resp = allow(a.input, []any{map[string]any{"type": "setMode", "mode": "default", "destination": "session"}})
			label = "Approved"
			c.sink.Meta(func(s *model.Session) { s.Mode = "default" })
		default:
			resp, label = deny(orDefault(r.Text, "The user wants to keep planning. Ask what they would like to change.")), "Keep planning"
		}
	default:
		switch r.Option {
		case "allow":
			resp, label = allow(a.input, nil), "Allowed"
		case "always":
			var perms []any
			for _, s := range a.suggestions {
				if m, ok := s.(map[string]any); ok {
					m = cloneMap(m)
					m["destination"] = "session"
					perms = append(perms, m)
				}
			}
			resp, label = allow(a.input, perms), "Always allowed"
		default:
			resp, label = deny(orDefault(r.Text, "The user denied this action.")), "Denied"
		}
	}
	err := c.p.writeJSON(map[string]any{
		"type":     "control_response",
		"response": map[string]any{"subtype": "success", "request_id": a.reqID, "response": resp},
	})
	c.sink.Update(askID, func(it *model.Item) {
		it.Ask.State = "answered"
		it.Ask.Answer = label
		it.Done = true
	})
	if left == 0 && busy {
		c.sink.Status(model.StatusRunning)
	}
	return err
}

// ---- stdout handling ----

func (c *Claude) onLine(line []byte) {
	var o map[string]any
	if json.Unmarshal(line, &o) != nil {
		return
	}
	parent := str(o, "parent_tool_use_id")
	switch str(o, "type") {
	case "system":
		c.onSystem(o)
	case "stream_event":
		c.onStream(obj(o, "event"), parent)
	case "assistant":
		c.onAssistant(obj(o, "message"), parent)
	case "user":
		c.onUser(obj(o, "message"), parent)
	case "result":
		c.onResult(o)
	case "control_request":
		c.onControlRequest(o)
	case "control_response":
		r := obj(o, "response")
		id := str(r, "request_id")
		c.mu.Lock()
		ch := c.waiters[id]
		delete(c.waiters, id)
		c.mu.Unlock()
		if ch != nil {
			ch <- r
		}
	case "control_cancel_request":
		askID := "ask-" + str(o, "request_id")
		c.mu.Lock()
		_, ok := c.pending[askID]
		delete(c.pending, askID)
		c.order = removeStr(c.order, askID)
		c.mu.Unlock()
		if ok {
			c.sink.Update(askID, func(it *model.Item) { it.Ask.State = "cancelled"; it.Done = true })
		}
	}
}

func (c *Claude) onSystem(o map[string]any) {
	switch str(o, "subtype") {
	case "init":
		m := str(o, "model")
		c.sink.Meta(func(s *model.Session) {
			s.Started = true
			if m != "" {
				s.Model = m
			}
		})
	case "compact_boundary":
		c.sink.Put(&model.Item{ID: model.NewID(), Kind: model.KindNotice, TS: model.NowMs(), Text: "Context compacted", Done: true})
	case "api_retry", "api_error":
		msg := str(o, "error")
		if msg == "" {
			msg = str(o, "message")
		}
		if msg != "" {
			c.sink.Put(&model.Item{ID: model.NewID(), Kind: model.KindNotice, TS: model.NowMs(), Text: "API retry: " + msg, Done: true})
		}
	}
}

func (c *Claude) onStream(ev map[string]any, parent string) {
	if ev == nil {
		return
	}
	switch str(ev, "type") {
	case "message_start":
		msg := obj(ev, "message")
		c.mu.Lock()
		c.curMsg[parent] = str(msg, "id")
		c.mu.Unlock()
	case "content_block_start":
		cb := obj(ev, "content_block")
		idx := int(num(ev, "index"))
		c.mu.Lock()
		msgID := c.curMsg[parent]
		if msgID == "" {
			c.mu.Unlock()
			return
		}
		typ := str(cb, "type")
		var b *streamBlock
		switch typ {
		case "text", "thinking":
			b = &streamBlock{id: fmt.Sprintf("%s:%d", msgID, idx), typ: typ}
		case "tool_use", "server_tool_use":
			b = &streamBlock{id: str(cb, "id"), typ: "tool_use"}
		}
		if b != nil {
			c.blocks[msgID] = append(c.blocks[msgID], b)
			c.byIdx[fmt.Sprintf("%s:%d", parent, idx)] = b
		}
		c.mu.Unlock()
		if b == nil {
			return
		}
		now := model.NowMs()
		switch typ {
		case "text":
			c.sink.Put(&model.Item{ID: b.id, Kind: model.KindText, TS: now, Parent: parent})
		case "thinking":
			c.sink.Put(&model.Item{ID: b.id, Kind: model.KindThinking, TS: now, Parent: parent})
		default:
			c.sink.Put(&model.Item{ID: b.id, Kind: model.KindTool, TS: now, Parent: parent,
				Tool: &model.Tool{Name: str(cb, "name"), Status: "running"}})
		}
	case "content_block_delta":
		idx := int(num(ev, "index"))
		c.mu.Lock()
		b := c.byIdx[fmt.Sprintf("%s:%d", parent, idx)]
		c.mu.Unlock()
		if b == nil {
			return
		}
		d := obj(ev, "delta")
		switch str(d, "type") {
		case "text_delta":
			c.sink.Append(b.id, "text", str(d, "text"))
		case "thinking_delta":
			if t := str(d, "thinking"); t != "" {
				c.sink.Append(b.id, "text", t)
			}
		}
	}
}

func (c *Claude) onAssistant(msg map[string]any, parent string) {
	if msg == nil {
		return
	}
	msgID := str(msg, "id")
	if u := obj(msg, "usage"); u != nil && parent == "" {
		used := int64(num(u, "input_tokens") + num(u, "cache_read_input_tokens") + num(u, "cache_creation_input_tokens") + num(u, "output_tokens"))
		c.mu.Lock()
		win := c.ctxWindow
		c.mu.Unlock()
		c.sink.Meta(func(s *model.Session) {
			s.CtxUsed = used
			if win > 0 {
				s.CtxMax = win
			}
		})
	}
	now := model.NowMs()
	for _, x := range arr(msg, "content") {
		b, ok := x.(map[string]any)
		if !ok {
			continue
		}
		typ := str(b, "type")
		switch typ {
		case "text", "thinking":
			text := str(b, typ)
			id := c.matchBlock(msgID, typ)
			kind := model.KindText
			if typ == "thinking" {
				kind = model.KindThinking
			}
			if !c.sink.Update(id, func(it *model.Item) {
				if text != "" || it.Text == "" {
					it.Text = text
				}
				it.Done = true
			}) {
				c.sink.Put(&model.Item{ID: id, Kind: kind, TS: now, Text: text, Done: true, Parent: parent})
			}
			if typ == "text" && parent == "" && text != "" {
				preview := truncate(text, 140)
				c.sink.Meta(func(s *model.Session) { s.Preview = preview })
			}
		case "tool_use", "server_tool_use", "mcp_tool_use":
			id := str(b, "id")
			name := str(b, "name")
			input := mustJSON(b["input"])
			c.matchBlock(msgID, "tool_use")
			if !c.sink.Update(id, func(it *model.Item) {
				it.Tool.Name = name
				it.Tool.Input = input
			}) {
				c.sink.Put(&model.Item{ID: id, Kind: model.KindTool, TS: now, Parent: parent,
					Tool: &model.Tool{Name: name, Input: input, Status: "running"}})
			}
		default:
			if strings.HasSuffix(typ, "_tool_result") {
				id := str(b, "tool_use_id")
				out := clip(stringifyContent(b["content"]))
				c.sink.Update(id, func(it *model.Item) {
					it.Tool.Output = out
					it.Tool.Status = "ok"
					it.Done = true
				})
			}
		}
	}
}

// matchBlock pairs a finished content block with the item created for it
// while streaming, so the final text replaces the streamed text in place.
func (c *Claude) matchBlock(msgID, typ string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range c.blocks[msgID] {
		if !b.matched && b.typ == typ {
			b.matched = true
			return b.id
		}
	}
	c.textSeq++
	return fmt.Sprintf("%s:x%d", msgID, c.textSeq)
}

func (c *Claude) onUser(msg map[string]any, parent string) {
	content, ok := msg["content"].([]any)
	if !ok {
		return
	}
	for _, x := range content {
		b, ok := x.(map[string]any)
		if !ok || str(b, "type") != "tool_result" {
			continue
		}
		id := str(b, "tool_use_id")
		out := clip(stringifyContent(b["content"]))
		isErr, _ := b["is_error"].(bool)
		status := "ok"
		if isErr {
			status = "error"
			if strings.Contains(out, "rejected") || strings.Contains(out, "denied") || strings.Contains(out, "doesn't want to proceed") {
				status = "denied"
			}
		}
		if !c.sink.Update(id, func(it *model.Item) {
			it.Tool.Output = out
			it.Tool.Status = status
			it.Done = true
		}) {
			c.sink.Put(&model.Item{ID: id, Kind: model.KindTool, TS: model.NowMs(), Parent: parent, Done: true,
				Tool: &model.Tool{Name: "Tool", Output: out, Status: status}})
		}
	}
}

func (c *Claude) onResult(o map[string]any) {
	total := num(o, "total_cost_usd")
	c.mu.Lock()
	delta := total - c.lastCost
	if delta < 0 {
		delta = total
	}
	c.lastCost = total
	c.busy = false
	clear(c.blocks)
	clear(c.curMsg)
	clear(c.byIdx)
	if mu := obj(o, "modelUsage"); mu != nil {
		for _, v := range mu {
			if m, ok := v.(map[string]any); ok {
				if w := int64(num(m, "contextWindow")); w > c.ctxWindow {
					c.ctxWindow = w
				}
			}
		}
	}
	win := c.ctxWindow
	c.mu.Unlock()

	isErr, _ := o["is_error"].(bool)
	sub := str(o, "subtype")
	res := &model.Result{
		DurationMs: int64(num(o, "duration_ms")),
		CostUSD:    delta,
		Turns:      int(num(o, "num_turns")),
		IsError:    isErr,
		Status:     sub,
	}
	it := &model.Item{ID: "res-" + model.NewID(), Kind: model.KindResult, TS: model.NowMs(), Done: true, Result: res}
	if isErr || (sub != "" && sub != "success") {
		text := str(o, "result")
		if errs := arr(o, "errors"); len(errs) > 0 && text == "" {
			var parts []string
			for _, e := range errs {
				parts = append(parts, fmt.Sprint(e))
			}
			text = strings.Join(parts, "\n")
		}
		it.Text = text
	}
	c.sink.Put(it)
	if isErr {
		if note := authNotice(str(o, "result")); note != "" {
			c.sink.Put(&model.Item{ID: model.NewID(), Kind: model.KindNotice, TS: model.NowMs(), Done: true, Text: note})
		}
	}
	c.sink.Meta(func(s *model.Session) {
		s.CostUSD += delta
		if win > 0 {
			s.CtxMax = win
		}
	})
	c.mu.Lock()
	waiting := len(c.pending) > 0
	c.mu.Unlock()
	if !waiting {
		c.sink.Status(model.StatusIdle)
	}
}

func (c *Claude) onControlRequest(o map[string]any) {
	reqID := str(o, "request_id")
	req := obj(o, "request")
	if str(req, "subtype") != "can_use_tool" {
		_ = c.p.writeJSON(map[string]any{
			"type":     "control_response",
			"response": map[string]any{"subtype": "error", "request_id": reqID, "error": "not supported by collie"},
		})
		return
	}
	tool := str(req, "tool_name")
	input := obj(req, "input")
	if input == nil {
		input = map[string]any{}
	}
	askID := "ask-" + reqID
	a := &claudeAsk{reqID: reqID, tool: tool, input: input, suggestions: arr(req, "permission_suggestions")}
	ask := &model.Ask{Tool: tool, Input: mustJSON(input), State: "pending"}
	it := &model.Item{ID: askID, TS: model.NowMs(), Ask: ask, Kind: model.KindApproval}
	switch tool {
	case "AskUserQuestion":
		a.kind = "question"
		it.Kind = model.KindQuestion
		for _, x := range arr(input, "questions") {
			q, ok := x.(map[string]any)
			if !ok {
				continue
			}
			mq := model.Question{ID: str(q, "question"), Header: str(q, "header"), Question: str(q, "question"), Other: true}
			mq.Multi, _ = q["multiSelect"].(bool)
			for _, y := range arr(q, "options") {
				if op, ok := y.(map[string]any); ok {
					mq.Options = append(mq.Options, model.QOption{Label: str(op, "label"), Description: str(op, "description")})
				}
			}
			a.questions = append(a.questions, mq)
		}
		ask.Questions = a.questions
	case "ExitPlanMode":
		a.kind = "plan"
		ask.Title = "Plan ready — start implementing?"
		ask.Body = str(input, "plan")
		ask.BodyKind = "md"
		ask.Options = []model.Option{
			{ID: "approve_accept", Label: "Yes, auto-accept edits", Style: "primary"},
			{ID: "approve", Label: "Yes, ask before edits"},
			{ID: "deny", Label: "No, keep planning", Style: "danger"},
		}
	default:
		a.kind = "approval"
		ask.Title = str(req, "description")
		if r := str(req, "decision_reason"); r != "" && ask.Title == "" {
			ask.Title = r
		}
		ask.Options = []model.Option{{ID: "allow", Label: "Allow", Style: "primary"}}
		if len(a.suggestions) > 0 {
			ask.Options = append(ask.Options, model.Option{ID: "always", Label: "Allow for this session"})
		}
		ask.Options = append(ask.Options, model.Option{ID: "deny", Label: "Deny", Style: "danger"})
	}
	c.mu.Lock()
	c.pending[askID] = a
	c.order = append(c.order, askID)
	c.mu.Unlock()
	c.sink.Put(it)
	c.sink.Status(model.StatusWaiting)
}

// ---- helpers ----

func stringifyContent(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, e := range x {
			if m, ok := e.(map[string]any); ok {
				switch str(m, "type") {
				case "text":
					parts = append(parts, str(m, "text"))
				case "image":
					parts = append(parts, "[image]")
				default:
					b, _ := json.Marshal(m)
					parts = append(parts, string(b))
				}
			}
		}
		return strings.Join(parts, "\n")
	case nil:
		return ""
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func cloneMap(m map[string]any) map[string]any {
	c := make(map[string]any, len(m)+1)
	for k, v := range m {
		c[k] = v
	}
	return c
}

func removeStr(s []string, v string) []string {
	out := s[:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

// authNotice turns a failed result caused by the user's Claude credentials
// into a hint on how to fix it ("" when the failure is something else).
func authNotice(result string) string {
	switch {
	case strings.Contains(result, "Not logged in"):
		return "Claude Code is not logged in for you. Run `claude setup-token` in a project terminal and paste the token under Account → Agent accounts."
	case strings.Contains(result, "OAuth access token is invalid"):
		return "Your Claude token was rejected — it may have been copied incompletely or revoked. Run `claude setup-token` in a project terminal again and save the new token (in the terminal: Select → Save as my Claude token)."
	}
	return ""
}
