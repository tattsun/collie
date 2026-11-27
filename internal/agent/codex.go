package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tattsun/collie/internal/model"
)

// Codex drives `codex app-server` (JSON-RPC over stdio, one JSON object per
// line). One app-server process is used per collie session.
type Codex struct {
	cfg  Config
	sink Sink
	p    *proc

	mu         sync.Mutex
	nextID     int64
	calls      map[int64]chan rpcReply
	threadID   string
	turnID     string
	pending    map[string]*codexAsk
	order      []string
	modeDirty  bool
	modelDirty bool
	diffs      map[string]string
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcReply struct {
	result json.RawMessage
	err    *rpcError
}

type rpcMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type codexAsk struct {
	rpcID     json.RawMessage
	kind      string // decision | permissions | question
	decisions []json.RawMessage
	options   []model.Option
	questions []model.Question
	perms     map[string]any
}

func NewCodex(cfg Config, sink Sink) *Codex {
	return &Codex{
		cfg:     cfg,
		sink:    sink,
		calls:   map[int64]chan rpcReply{},
		pending: map[string]*codexAsk{},
		diffs:   map[string]string{},
	}
}

func codexPolicy(mode, cwd string) (approval, sandbox string, policy map[string]any) {
	ww := map[string]any{"type": "workspaceWrite", "writableRoots": []string{cwd}, "networkAccess": false,
		"excludeTmpdirEnvVar": false, "excludeSlashTmp": false}
	switch mode {
	case "read-only":
		return "on-request", "read-only", map[string]any{"type": "readOnly", "networkAccess": false}
	case "untrusted":
		return "untrusted", "workspace-write", ww
	case "full":
		return "never", "danger-full-access", map[string]any{"type": "dangerFullAccess"}
	default:
		return "on-request", "workspace-write", ww
	}
}

func (c *Codex) Start() error {
	p, err := startProc(c.cfg, c.cfg.Bin, append([]string{"app-server"}, c.cfg.ExtraArgs...), c.onLine)
	if err != nil {
		return err
	}
	c.p = p
	go func() {
		<-p.done
		c.mu.Lock()
		for id, ch := range c.calls {
			ch <- rpcReply{err: &rpcError{Message: "agent process exited"}}
			delete(c.calls, id)
		}
		c.mu.Unlock()
		c.sink.Exited(p.exitError())
	}()
	fail := func(err error) error {
		p.close()
		if e := p.exitError(); e != nil {
			return e
		}
		return err
	}
	if _, err := c.call("initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "collie", "title": "Collie", "version": "0.1.0"},
		"capabilities": nil,
	}, time.Minute); err != nil {
		return fail(err)
	}
	if err := c.p.writeJSON(map[string]any{"method": "initialized"}); err != nil {
		return fail(err)
	}
	approval, sandbox, _ := codexPolicy(c.cfg.Mode, c.cfg.Cwd)
	params := map[string]any{"cwd": c.cfg.Cwd, "approvalPolicy": approval, "sandbox": sandbox}
	if c.cfg.Model != "" && c.cfg.Model != "default" {
		params["model"] = c.cfg.Model
	}
	if c.cfg.Instructions != "" {
		// Set only when there are some: they replace any
		// developer_instructions in the user's Codex config.
		params["developerInstructions"] = c.cfg.Instructions
	}
	var res json.RawMessage
	if c.cfg.Resume && c.cfg.Ref != "" {
		rp := cloneMap(params)
		rp["threadId"] = c.cfg.Ref
		rp["excludeTurns"] = true
		res, err = c.call("thread/resume", rp, 2*time.Minute)
		if err != nil {
			c.sink.Put(&model.Item{ID: model.NewID(), Kind: model.KindNotice, TS: model.NowMs(), Done: true,
				Text: "Could not resume the previous Codex thread (" + err.Error() + "); starting a new one."})
			res = nil
		}
	}
	if res == nil {
		res, err = c.call("thread/start", params, 2*time.Minute)
		if err != nil {
			return fail(err)
		}
	}
	var tr struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		Model string `json:"model"`
	}
	_ = json.Unmarshal(res, &tr)
	if tr.Thread.ID == "" {
		return fail(errors.New("codex did not return a thread id"))
	}
	c.mu.Lock()
	c.threadID = tr.Thread.ID
	c.mu.Unlock()
	c.sink.Meta(func(s *model.Session) {
		s.AgentRef = tr.Thread.ID
		s.Started = true
		if tr.Model != "" {
			s.Model = tr.Model
		}
	})
	go c.loadModels()
	go c.loadRateLimits()
	return nil
}

// loadRateLimits shows the account's plan usage right away; later changes
// arrive as account/rateLimits/updated.
func (c *Codex) loadRateLimits() {
	res, err := c.call("account/rateLimits/read", map[string]any{"excludeResetCreditDetails": true}, 30*time.Second)
	if err != nil {
		return
	}
	var m map[string]any
	if json.Unmarshal(res, &m) != nil {
		return
	}
	if u, ok := codexUsage(m, model.NowMs()); ok {
		c.sink.Usage(u)
	}
}

func (c *Codex) loadModels() {
	res, err := c.call("model/list", map[string]any{}, 30*time.Second)
	if err != nil {
		return
	}
	var ml struct {
		Data []struct {
			ID          string `json:"id"`
			Model       string `json:"model"`
			DisplayName string `json:"displayName"`
			Description string `json:"description"`
			Hidden      bool   `json:"hidden"`
		} `json:"data"`
	}
	if json.Unmarshal(res, &ml) != nil {
		return
	}
	var caps model.Caps
	for _, m := range ml.Data {
		if m.Hidden {
			continue
		}
		id := m.Model
		if id == "" {
			id = m.ID
		}
		caps.Models = append(caps.Models, model.ModelOpt{ID: id, Label: m.DisplayName, Description: m.Description})
	}
	c.sink.Caps(caps)
}

func (c *Codex) call(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan rpcReply, 1)
	c.calls[id] = ch
	c.mu.Unlock()
	if err := c.p.writeJSON(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.calls, id)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("%s: %s", method, r.err.Message)
		}
		return r.result, nil
	case <-time.After(timeout):
		c.mu.Lock()
		delete(c.calls, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: timed out", method)
	}
}

func (c *Codex) respond(id json.RawMessage, result any) error {
	return c.p.writeJSON(map[string]any{"id": id, "result": result})
}

func (c *Codex) Done() <-chan struct{} { return c.p.done }

func (c *Codex) Close() {
	if c.p != nil {
		c.p.close()
	}
}

func (c *Codex) SetMode(mode string) error {
	c.mu.Lock()
	c.cfg.Mode = mode
	c.modeDirty = true
	c.mu.Unlock()
	return nil
}

func (c *Codex) SetModel(m string) error {
	c.mu.Lock()
	c.cfg.Model = m
	c.modelDirty = true
	c.mu.Unlock()
	return nil
}

func (c *Codex) Interrupt() error {
	c.mu.Lock()
	ids := append([]string(nil), c.order...)
	thread, turn := c.threadID, c.turnID
	c.mu.Unlock()
	for _, id := range ids {
		_ = c.reply(id, Reply{Option: "deny"}, true)
	}
	if turn == "" {
		return nil
	}
	_, err := c.call("turn/interrupt", map[string]any{"threadId": thread, "turnId": turn}, 15*time.Second)
	return err
}

func (c *Codex) Send(in Input) error {
	c.mu.Lock()
	ids := append([]string(nil), c.order...)
	c.mu.Unlock()
	for i, id := range ids {
		r := Reply{Option: "deny"}
		if i == 0 {
			r.Text = in.Text
		}
		_ = c.reply(id, r, false)
	}
	var input []any
	if in.Text != "" {
		input = append(input, map[string]any{"type": "text", "text": in.Text, "text_elements": []any{}})
	}
	for _, img := range in.Images {
		path := img.Path
		if img.AgentPath != "" {
			path = img.AgentPath
		}
		input = append(input, map[string]any{"type": "localImage", "path": path})
	}
	if len(input) == 0 {
		return nil
	}
	c.mu.Lock()
	thread, turn := c.threadID, c.turnID
	dirty := c.modeDirty
	mode := c.cfg.Mode
	modelDirty, mdl := c.modelDirty, c.cfg.Model
	c.mu.Unlock()
	if turn != "" {
		_, err := c.call("turn/steer", map[string]any{"threadId": thread, "input": input,
			"expectedTurnId": turn, "clientUserMessageId": in.ItemID}, 30*time.Second)
		if err == nil {
			return nil
		}
		// The turn most likely just finished: start a new one instead.
	}
	params := map[string]any{"threadId": thread, "input": input, "clientUserMessageId": in.ItemID}
	if dirty {
		approval, _, policy := codexPolicy(mode, c.cfg.Cwd)
		params["approvalPolicy"] = approval
		params["sandboxPolicy"] = policy
	}
	if modelDirty && mdl != "" && mdl != "default" {
		params["model"] = mdl
	}
	if c.cfg.Effort != "" {
		params["effort"] = c.cfg.Effort
	}
	res, err := c.call("turn/start", params, time.Minute)
	if err != nil {
		return err
	}
	var tr struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(res, &tr)
	c.mu.Lock()
	if dirty {
		c.modeDirty = false
	}
	if modelDirty {
		c.modelDirty = false
	}
	if tr.Turn.ID != "" {
		c.turnID = tr.Turn.ID
	}
	c.mu.Unlock()
	c.sink.Status(model.StatusRunning)
	return nil
}

func (c *Codex) Reply(askID string, r Reply) error { return c.reply(askID, r, false) }

func (c *Codex) reply(askID string, r Reply, cancel bool) error {
	c.mu.Lock()
	a, ok := c.pending[askID]
	if ok {
		delete(c.pending, askID)
		c.order = removeStr(c.order, askID)
	}
	left := len(c.pending)
	running := c.turnID != ""
	c.mu.Unlock()
	if !ok {
		return errors.New("this request is no longer pending")
	}
	var result any
	label := ""
	switch a.kind {
	case "decision":
		idx := -1
		if r.Option == "deny" {
			want := "decline"
			if cancel {
				want = "cancel"
			}
			for i, d := range a.decisions {
				if string(d) == strconv.Quote(want) {
					idx = i
				}
			}
			if idx < 0 {
				for i, d := range a.decisions {
					if string(d) == `"decline"` || string(d) == `"cancel"` {
						idx = i
					}
				}
			}
		} else if n, err := strconv.Atoi(r.Option); err == nil && n >= 0 && n < len(a.decisions) {
			idx = n
		}
		if idx < 0 {
			result, label = map[string]any{"decision": "decline"}, "Denied"
		} else {
			result, label = map[string]any{"decision": a.decisions[idx]}, a.options[idx].Label
		}
	case "permissions":
		granted := map[string]any{}
		scope := "turn"
		switch r.Option {
		case "turn", "session":
			for _, k := range []string{"network", "fileSystem"} {
				if v, ok := a.perms[k]; ok && v != nil {
					granted[k] = v
				}
			}
			scope = r.Option
			label = "Allowed for this " + r.Option
		default:
			label = "Denied"
		}
		result = map[string]any{"permissions": granted, "scope": scope}
	case "question":
		answers := map[string]any{}
		var parts []string
		for i, q := range a.questions {
			v := r.Answers[q.ID]
			if r.Option == "deny" {
				v = nil
				if i == 0 && r.Text != "" {
					v = []string{r.Text}
				}
			}
			if v == nil {
				v = []string{}
			}
			answers[q.ID] = map[string]any{"answers": v}
			parts = append(parts, strings.Join(v, ", "))
		}
		result = map[string]any{"answers": answers}
		label = strings.Join(parts, " / ")
		if r.Option == "deny" && r.Text == "" {
			label = "Declined"
		}
	}
	err := c.respond(a.rpcID, result)
	c.sink.Update(askID, func(it *model.Item) {
		it.Ask.State = "answered"
		it.Ask.Answer = label
		it.Done = true
	})
	if left == 0 && running {
		c.sink.Status(model.StatusRunning)
	}
	return err
}

// ---- stdout handling ----

func (c *Codex) onLine(line []byte) {
	var m rpcMsg
	if json.Unmarshal(line, &m) != nil {
		return
	}
	switch {
	case m.Method != "" && len(m.ID) > 0:
		var params map[string]any
		_ = json.Unmarshal(m.Params, &params)
		c.onServerRequest(m.ID, m.Method, params)
	case m.Method != "":
		var params map[string]any
		_ = json.Unmarshal(m.Params, &params)
		c.onNotify(m.Method, params)
	case len(m.ID) > 0:
		id, err := strconv.ParseInt(string(m.ID), 10, 64)
		if err != nil {
			return
		}
		c.mu.Lock()
		ch := c.calls[id]
		delete(c.calls, id)
		c.mu.Unlock()
		if ch != nil {
			ch <- rpcReply{result: m.Result, err: m.Error}
		}
	}
}

func (c *Codex) onNotify(method string, p map[string]any) {
	switch method {
	case "turn/started":
		t := obj(p, "turn")
		c.mu.Lock()
		c.turnID = str(t, "id")
		c.mu.Unlock()
		c.sink.Status(model.StatusRunning)
	case "turn/completed":
		t := obj(p, "turn")
		c.mu.Lock()
		if c.turnID == str(t, "id") {
			c.turnID = ""
		}
		waiting := len(c.pending) > 0
		c.mu.Unlock()
		status := str(t, "status")
		now := model.NowMs()
		if status == "failed" {
			e := obj(t, "error")
			msg := str(e, "message")
			if d := str(e, "additionalDetails"); d != "" {
				msg += "\n" + d
			}
			c.sink.Put(&model.Item{ID: model.NewID(), Kind: model.KindError, TS: now, Done: true, Text: orDefault(msg, "Turn failed")})
		}
		c.sink.Put(&model.Item{ID: "res-" + model.NewID(), Kind: model.KindResult, TS: now, Done: true,
			Result: &model.Result{DurationMs: int64(num(t, "durationMs")), Status: status, IsError: status == "failed"}})
		if !waiting {
			c.sink.Status(model.StatusIdle)
		}
	case "item/started":
		c.onItem(obj(p, "item"), false)
	case "item/completed":
		c.onItem(obj(p, "item"), true)
	case "item/agentMessage/delta", "item/reasoning/summaryTextDelta", "item/plan/delta":
		c.sink.Append(str(p, "itemId"), "text", str(p, "delta"))
	case "item/reasoning/summaryPartAdded":
		c.sink.Append(str(p, "itemId"), "text", "\n\n")
	case "item/commandExecution/outputDelta":
		c.sink.Append(str(p, "itemId"), "output", str(p, "delta"))
	case "turn/plan/updated":
		var steps []model.PlanStep
		for _, x := range arr(p, "plan") {
			if s, ok := x.(map[string]any); ok {
				steps = append(steps, model.PlanStep{Step: str(s, "step"), Status: str(s, "status")})
			}
		}
		c.sink.Put(&model.Item{ID: "plan-" + str(p, "turnId"), Kind: model.KindPlan, TS: model.NowMs(), Done: true,
			Text: str(p, "explanation"), Plan: steps})
	case "error":
		e := obj(p, "error")
		msg := str(e, "message")
		if retry, _ := p["willRetry"].(bool); retry {
			c.sink.Put(&model.Item{ID: model.NewID(), Kind: model.KindNotice, TS: model.NowMs(), Done: true, Text: "Retrying: " + msg})
		} else {
			c.sink.Put(&model.Item{ID: model.NewID(), Kind: model.KindError, TS: model.NowMs(), Done: true, Text: msg})
		}
	case "warning":
		if msg := str(p, "message"); msg != "" {
			c.sink.Put(&model.Item{ID: model.NewID(), Kind: model.KindNotice, TS: model.NowMs(), Done: true, Text: msg})
		}
	case "account/rateLimits/updated":
		rl := obj(p, "rateLimits")
		if ws := codexRateLimits(rl); len(ws) > 0 {
			c.sink.Usage(model.Usage{Windows: ws, Plan: str(rl, "planType"), UpdatedAt: model.NowMs()})
		}
	case "thread/tokenUsage/updated":
		tu := obj(p, "tokenUsage")
		last := obj(tu, "last")
		used := int64(num(last, "totalTokens"))
		win := int64(num(tu, "modelContextWindow"))
		c.sink.Meta(func(s *model.Session) {
			s.CtxUsed = used
			if win > 0 {
				s.CtxMax = win
			}
		})
	case "serverRequest/resolved":
		rid := fmt.Sprint(p["requestId"])
		if f, ok := p["requestId"].(float64); ok {
			rid = strconv.FormatInt(int64(f), 10)
		}
		askID := "ask-" + rid
		c.mu.Lock()
		_, ok := c.pending[askID]
		delete(c.pending, askID)
		c.order = removeStr(c.order, askID)
		left := len(c.pending)
		running := c.turnID != ""
		c.mu.Unlock()
		if ok {
			c.sink.Update(askID, func(it *model.Item) {
				if it.Ask.State == "pending" {
					it.Ask.State = "cancelled"
				}
				it.Done = true
			})
			if left == 0 && running {
				c.sink.Status(model.StatusRunning)
			}
		}
	}
}

func (c *Codex) upsert(id, kind string, fn func(*model.Item)) {
	if c.sink.Update(id, fn) {
		return
	}
	it := &model.Item{ID: id, Kind: kind, TS: model.NowMs()}
	if kind == model.KindTool {
		it.Tool = &model.Tool{Status: "running"}
	}
	fn(it)
	c.sink.Put(it)
}

func (c *Codex) onItem(item map[string]any, done bool) {
	if item == nil {
		return
	}
	id := str(item, "id")
	typ := str(item, "type")
	switch typ {
	case "userMessage", "hookPrompt":
		return
	case "agentMessage", "plan":
		text := str(item, "text")
		c.upsert(id, model.KindText, func(it *model.Item) {
			if text != "" {
				it.Text = text
			}
			it.Done = done
		})
		if done && text != "" {
			preview := truncate(text, 140)
			c.sink.Meta(func(s *model.Session) { s.Preview = preview })
		}
	case "reasoning":
		var parts []string
		for _, x := range arr(item, "summary") {
			if s, ok := x.(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
		text := strings.Join(parts, "\n\n")
		c.upsert(id, model.KindThinking, func(it *model.Item) {
			if text != "" {
				it.Text = text
			}
			it.Done = done
		})
	case "commandExecution":
		cmd := cleanCommand(str(item, "command"))
		input := mustJSON(map[string]any{"command": cmd, "cwd": str(item, "cwd")})
		status := toolStatus(str(item, "status"), item["exitCode"])
		out := str(item, "aggregatedOutput")
		c.upsert(id, model.KindTool, func(it *model.Item) {
			it.Tool.Name = "Bash"
			it.Tool.Input = input
			it.Tool.Status = status
			if out != "" {
				it.Tool.Output = clip(out)
			}
			if done {
				if ec, ok := item["exitCode"].(float64); ok && ec != 0 && !strings.Contains(it.Tool.Output, "exit code") {
					it.Tool.Output = strings.TrimRight(it.Tool.Output, "\n") + fmt.Sprintf("\n[exit code %d]", int(ec))
				}
				it.Tool.Output = clip(it.Tool.Output)
			}
			it.Done = done
		})
	case "fileChange":
		var files []string
		diff := codexDiff(arr(item, "changes"), &files)
		c.mu.Lock()
		c.diffs[id] = diff
		c.mu.Unlock()
		status := toolStatus(str(item, "status"), nil)
		c.upsert(id, model.KindTool, func(it *model.Item) {
			it.Tool.Name = "Patch"
			it.Tool.Input = mustJSON(map[string]any{"files": files})
			it.Tool.Diff = diff
			it.Tool.Status = status
			it.Done = done
		})
	case "mcpToolCall":
		name := "mcp__" + str(item, "server") + "__" + str(item, "tool")
		input := mustJSON(item["arguments"])
		status := toolStatus(str(item, "status"), nil)
		var out string
		var imgs []string
		if r := obj(item, "result"); r != nil {
			out = stringifyContent(r["content"])
			if done {
				imgs = toolImages(c.sink, r["content"])
			}
		}
		if e := obj(item, "error"); e != nil {
			out = str(e, "message")
			status = "error"
		}
		c.upsert(id, model.KindTool, func(it *model.Item) {
			it.Tool.Name, it.Tool.Input, it.Tool.Status = name, input, status
			if out != "" {
				it.Tool.Output = clip(out)
			}
			if len(imgs) > 0 {
				it.Images = imgs
			}
			it.Done = done
		})
	case "webSearch":
		input := mustJSON(map[string]any{"query": str(item, "query")})
		c.upsert(id, model.KindTool, func(it *model.Item) {
			it.Tool.Name, it.Tool.Input = "WebSearch", input
			it.Tool.Status = "running"
			if done {
				it.Tool.Status = "ok"
			}
			it.Done = done
		})
	case "dynamicToolCall":
		name := str(item, "tool")
		input := mustJSON(item["arguments"])
		status := toolStatus(str(item, "status"), nil)
		out := stringifyContent(item["contentItems"])
		c.upsert(id, model.KindTool, func(it *model.Item) {
			it.Tool.Name, it.Tool.Input, it.Tool.Status = name, input, status
			if out != "" {
				it.Tool.Output = clip(out)
			}
			it.Done = done
		})
	case "imageView":
		input := mustJSON(map[string]any{"path": str(item, "path")})
		c.upsert(id, model.KindTool, func(it *model.Item) {
			it.Tool.Name, it.Tool.Input, it.Tool.Status = "ViewImage", input, "ok"
			it.Done = done
		})
	case "contextCompaction":
		if done {
			c.sink.Put(&model.Item{ID: id, Kind: model.KindNotice, TS: model.NowMs(), Done: true, Text: "Context compacted"})
		}
	case "enteredReviewMode":
		c.sink.Put(&model.Item{ID: id, Kind: model.KindNotice, TS: model.NowMs(), Done: true, Text: "Review started"})
	case "exitedReviewMode":
		if done {
			c.sink.Put(&model.Item{ID: id, Kind: model.KindText, TS: model.NowMs(), Done: true, Text: str(item, "review")})
		}
	default:
		raw := cloneMap(item)
		delete(raw, "id")
		delete(raw, "type")
		input := mustJSON(raw)
		name := typ
		if typ == "collabAgentToolCall" || typ == "subAgentActivity" {
			name = "Agent"
		}
		c.upsert(id, model.KindTool, func(it *model.Item) {
			it.Tool.Name, it.Tool.Input = name, input
			it.Tool.Status = "running"
			if done {
				it.Tool.Status = "ok"
			}
			it.Done = done
		})
	}
}

func (c *Codex) onServerRequest(id json.RawMessage, method string, p map[string]any) {
	askID := "ask-" + strings.Trim(string(id), `"`)
	a := &codexAsk{rpcID: id}
	ask := &model.Ask{State: "pending"}
	it := &model.Item{ID: askID, Kind: model.KindApproval, TS: model.NowMs(), Ask: ask}
	decisionOptions := func(ds []any) {
		if len(ds) == 0 {
			ds = []any{"accept", "acceptForSession", "decline", "cancel"}
		}
		for i, d := range ds {
			op := model.Option{ID: strconv.Itoa(i)}
			switch v := d.(type) {
			case string:
				switch v {
				case "accept":
					op.Label, op.Style = "Allow", "primary"
				case "acceptForSession":
					op.Label = "Allow for this session"
				case "decline":
					op.Label, op.Style = "Deny", "danger"
				case "cancel":
					op.Label, op.Style = "Deny & stop", "danger"
				default:
					op.Label = v
				}
			case map[string]any:
				if am := obj(v, "acceptWithExecpolicyAmendment"); am != nil {
					var parts []string
					for _, x := range arr(am, "execpolicy_amendment") {
						parts = append(parts, fmt.Sprint(x))
					}
					op.Label = "Always allow `" + strings.Join(parts, " ") + "`"
				} else if nm := obj(v, "applyNetworkPolicyAmendment"); nm != nil {
					op.Label = "Apply network rule"
					if amend := obj(nm, "network_policy_amendment"); amend != nil {
						if h := str(amend, "host"); h != "" {
							op.Label = "Allow network access to " + h
						}
					}
				} else {
					for k := range v {
						op.Label = k
					}
				}
			}
			a.decisions = append(a.decisions, mustJSON(d))
			a.options = append(a.options, op)
		}
		ask.Options = a.options
	}
	switch method {
	case "item/commandExecution/requestApproval":
		a.kind = "decision"
		ask.Tool = "Bash"
		ask.Input = mustJSON(map[string]any{"command": cleanCommand(str(p, "command")), "cwd": str(p, "cwd")})
		ask.Title = str(p, "reason")
		decisionOptions(arr(p, "availableDecisions"))
	case "item/fileChange/requestApproval":
		a.kind = "decision"
		c.mu.Lock()
		diff := c.diffs[str(p, "itemId")]
		c.mu.Unlock()
		ask.Tool = "Patch"
		ask.Title = orDefault(str(p, "reason"), "Apply these changes?")
		ask.Body, ask.BodyKind = diff, "diff"
		decisionOptions(arr(p, "availableDecisions"))
	case "item/permissions/requestApproval":
		a.kind = "permissions"
		a.perms = obj(p, "permissions")
		ask.Tool = "Permissions"
		ask.Title = orDefault(str(p, "reason"), "Additional permissions requested")
		b, _ := json.MarshalIndent(a.perms, "", "  ")
		ask.Body, ask.BodyKind = string(b), "code"
		ask.Options = []model.Option{
			{ID: "turn", Label: "Allow for this turn", Style: "primary"},
			{ID: "session", Label: "Allow for this session"},
			{ID: "deny", Label: "Deny", Style: "danger"},
		}
	case "item/tool/requestUserInput":
		a.kind = "question"
		it.Kind = model.KindQuestion
		for _, x := range arr(p, "questions") {
			q, ok := x.(map[string]any)
			if !ok {
				continue
			}
			mq := model.Question{ID: str(q, "id"), Header: str(q, "header"), Question: str(q, "question")}
			mq.Other, _ = q["isOther"].(bool)
			mq.Secret, _ = q["isSecret"].(bool)
			for _, y := range arr(q, "options") {
				if op, ok := y.(map[string]any); ok {
					mq.Options = append(mq.Options, model.QOption{Label: str(op, "label"), Description: str(op, "description")})
				}
			}
			if len(mq.Options) == 0 {
				mq.Other = true
			}
			a.questions = append(a.questions, mq)
		}
		ask.Questions = a.questions
	case "mcpServer/elicitation/request":
		_ = c.respond(id, map[string]any{"action": "decline", "content": nil, "_meta": nil})
		c.sink.Put(&model.Item{ID: model.NewID(), Kind: model.KindNotice, TS: model.NowMs(), Done: true,
			Text: "An MCP server asked for input; collie declined it."})
		return
	case "applyPatchApproval", "execCommandApproval":
		_ = c.respond(id, map[string]any{"decision": "denied"})
		return
	default:
		_ = c.p.writeJSON(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "not supported by collie"}})
		return
	}
	c.mu.Lock()
	c.pending[askID] = a
	c.order = append(c.order, askID)
	c.mu.Unlock()
	c.sink.Put(it)
	c.sink.Status(model.StatusWaiting)
}

// ---- helpers ----

var shellWrap = regexp.MustCompile(`^(?:\S*/)?(?:bash|zsh|sh) -l?c (.*)$`)

func cleanCommand(cmd string) string {
	m := shellWrap.FindStringSubmatch(cmd)
	if m == nil {
		return cmd
	}
	s := m[1]
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], `'\''`, `'`)
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
	}
	return s
}

func toolStatus(s string, exit any) string {
	switch s {
	case "inProgress", "":
		return "running"
	case "failed":
		return "error"
	case "declined":
		return "denied"
	}
	if ec, ok := exit.(float64); ok && ec != 0 {
		return "error"
	}
	return "ok"
}

func codexDiff(changes []any, files *[]string) string {
	var b strings.Builder
	for _, x := range changes {
		ch, ok := x.(map[string]any)
		if !ok {
			continue
		}
		path := str(ch, "path")
		*files = append(*files, path)
		kind := str(obj(ch, "kind"), "type")
		body := str(ch, "diff")
		from, to := "a/"+path, "b/"+path
		switch kind {
		case "add":
			from = "/dev/null"
		case "delete":
			to = "/dev/null"
		}
		if mp := str(obj(ch, "kind"), "move_path"); mp != "" {
			to = "b/" + mp
		}
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- %s\n+++ %s\n", path, path, from, to)
		if strings.HasPrefix(body, "@@") || strings.Contains(body, "\n@@") {
			b.WriteString(body)
		} else {
			prefix := "+"
			if kind == "delete" {
				prefix = "-"
			}
			for _, l := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
				b.WriteString(prefix + l + "\n")
			}
		}
		if !strings.HasSuffix(b.String(), "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// ProbeCodex starts a throwaway app-server just to list the available models.
func ProbeCodex(bin, cwd string) (model.Caps, error) {
	var caps model.Caps
	s := &capsSink{}
	c := NewCodex(Config{Bin: bin, Cwd: cwd}, s)
	p, err := startProc(Config{Cwd: cwd}, bin, []string{"app-server"}, c.onLine)
	if err != nil {
		return caps, err
	}
	c.p = p
	defer p.close()
	if _, err := c.call("initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "collie", "title": "Collie", "version": "0.1.0"},
		"capabilities": nil,
	}, 30*time.Second); err != nil {
		return caps, err
	}
	_ = p.writeJSON(map[string]any{"method": "initialized"})
	c.loadModels()
	return s.caps, nil
}

// capsSink only records capabilities.
type capsSink struct{ caps model.Caps }

func (s *capsSink) Put(*model.Item)                       {}
func (s *capsSink) Update(string, func(*model.Item)) bool { return true }
func (s *capsSink) Append(string, string, string)         {}
func (s *capsSink) Status(string)                         {}
func (s *capsSink) Meta(func(*model.Session))             {}
func (s *capsSink) Caps(c model.Caps)                     { s.caps = c }
func (s *capsSink) Usage(model.Usage)                     {}
func (s *capsSink) Exited(error)                          {}
func (s *capsSink) Blob([]byte) string                    { return "" }
