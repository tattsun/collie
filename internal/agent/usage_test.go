package agent

import (
	"encoding/json"
	"testing"

	"github.com/tattsun/collie/internal/model"
)

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func byID(ws []model.UsageWindow) map[string]model.UsageWindow {
	out := map[string]model.UsageWindow{}
	for _, w := range ws {
		out[w.ID] = w
	}
	return out
}

func TestClaudeRateLimitWindows(t *testing.T) {
	info := decode(t, `{"status":"allowed","rateLimitType":"five_hour","utilization":0.42,"resetsAt":1700000000,
		"unifiedWindows":{
			"five_hour":{"utilization":0.42,"resetsAt":1700000000,"status":"allowed"},
			"seven_day":{"utilization":0.181,"resetsAt":1700500000}}}`)
	u, ok := claudeRateLimit(info, 1)
	if !ok || u.Status != "allowed" || len(u.Windows) != 2 {
		t.Fatalf("got %+v", u)
	}
	w := byID(u.Windows)
	if w["five_hour"].Pct != 42 || w["five_hour"].ResetsAt != 1700000000000 || w["five_hour"].Status != "allowed" {
		t.Errorf("five_hour = %+v", w["five_hour"])
	}
	if p := w["seven_day"].Pct; p < 18.09 || p > 18.11 {
		t.Errorf("seven_day pct = %v", p)
	}
}

func TestClaudeRateLimitTopLevelOnly(t *testing.T) {
	// Older CLIs: only the window that limits the account.
	u, ok := claudeRateLimit(decode(t, `{"status":"allowed_warning","rateLimitType":"seven_day","utilization":0.9,"resetsAt":1700500000}`), 1)
	if !ok || len(u.Windows) != 1 || u.Windows[0].ID != "seven_day" || u.Windows[0].Pct != 90 {
		t.Fatalf("got %+v", u)
	}
	// Refused without a figure: the window is full.
	u, _ = claudeRateLimit(decode(t, `{"status":"rejected","rateLimitType":"five_hour","resetsAt":1700000000}`), 1)
	if len(u.Windows) != 1 || u.Windows[0].Pct != 100 {
		t.Fatalf("rejected: %+v", u)
	}
	// Nothing to show for overage, but the status still counts.
	u, ok = claudeRateLimit(decode(t, `{"status":"allowed","rateLimitType":"overage","utilization":0.1}`), 1)
	if !ok || len(u.Windows) != 0 || u.Status != "allowed" {
		t.Fatalf("overage: %+v ok=%v", u, ok)
	}
	if _, ok := claudeRateLimit(nil, 1); ok {
		t.Fatal("nil info reported")
	}
}

func TestClaudePlanUsage(t *testing.T) {
	resp := decode(t, `{"subscription_type":"max","rate_limits_available":true,"rate_limits":{
		"five_hour":{"utilization":12,"resets_at":"2026-11-27T15:00:00Z"},
		"seven_day":{"utilization":null,"resets_at":null},
		"seven_day_opus":null}}`)
	u, ok := claudePlanUsage(resp, 1)
	if !ok || u.Plan != "max" || len(u.Windows) != 1 {
		t.Fatalf("got %+v", u)
	}
	if w := u.Windows[0]; w.ID != "five_hour" || w.Pct != 12 || w.ResetsAt != 1795791600000 {
		t.Errorf("five_hour = %+v", w)
	}
	if _, ok := claudePlanUsage(decode(t, `{"rate_limits_available":false,"rate_limits":null}`), 1); ok {
		t.Fatal("unavailable limits reported")
	}
}

func TestCodexUsage(t *testing.T) {
	res := decode(t, `{"rateLimits":{"limitId":"codex","planType":"plus",
			"primary":{"usedPercent":7,"windowDurationMins":300,"resetsAt":1700000000},
			"secondary":{"usedPercent":31,"windowDurationMins":10080,"resetsAt":1700500000}},
		"rateLimitsByLimitId":{
			"codex":{"limitId":"codex","planType":"plus",
				"primary":{"usedPercent":7,"windowDurationMins":300,"resetsAt":1700000000},
				"secondary":{"usedPercent":31,"windowDurationMins":10080,"resetsAt":1700500000}},
			"codex_other":{"limitId":"codex_other","limitName":"GPT-5 mini","primary":{"usedPercent":2},"secondary":null}}}`)
	u, ok := codexUsage(res, 1)
	if !ok || u.Plan != "plus" || len(u.Windows) != 3 {
		t.Fatalf("got %+v", u)
	}
	w := byID(u.Windows)
	if w["primary"].Pct != 7 || w["primary"].Mins != 300 || w["primary"].ResetsAt != 1700000000000 || w["primary"].Label != "" {
		t.Errorf("primary = %+v", w["primary"])
	}
	if w["secondary"].Mins != 10080 || w["secondary"].Pct != 31 {
		t.Errorf("secondary = %+v", w["secondary"])
	}
	if o := w["codex_other:primary"]; o.Pct != 2 || o.Label != "GPT-5 mini" {
		t.Errorf("other bucket = %+v", o)
	}
	// A sparse update leaves unknown windows out instead of zeroing them.
	ws := codexRateLimits(decode(t, `{"primary":{"usedPercent":9},"secondary":null}`))
	if len(ws) != 1 || ws[0].ID != "primary" || ws[0].Pct != 9 {
		t.Fatalf("sparse = %+v", ws)
	}
}

func TestMergeUsage(t *testing.T) {
	old := model.Usage{Plan: "max", Status: "allowed", UpdatedAt: 1, Windows: []model.UsageWindow{
		{ID: "seven_day", Pct: 20}, {ID: "five_hour", Pct: 10},
	}}
	got := model.MergeUsage(old, model.Usage{UpdatedAt: 2, Windows: []model.UsageWindow{{ID: "five_hour", Pct: 15}}})
	if got.Plan != "max" || got.Status != "allowed" || got.UpdatedAt != 2 || len(got.Windows) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got.Windows[0].ID != "five_hour" || got.Windows[0].Pct != 15 || got.Windows[1].ID != "seven_day" || got.Windows[1].Pct != 20 {
		t.Errorf("windows = %+v", got.Windows)
	}
	got = model.MergeUsage(got, model.Usage{UpdatedAt: 3, Status: "rejected"})
	if got.Status != "rejected" || len(got.Windows) != 2 {
		t.Errorf("status update = %+v", got)
	}
}
