package agent

import (
	"time"

	"github.com/tattsun/collie/internal/model"
)

// Plan usage (rate-limit windows) as the agents report it.

// claudeRateLimit reads a `rate_limit_event`'s rate_limit_info. Claude Code
// emits one whenever a window's usage, reset time or status moves; the
// figures come from the API's response headers, so they arrive with any
// claude.ai login (including `claude setup-token` tokens).
func claudeRateLimit(info map[string]any, now int64) (model.Usage, bool) {
	if info == nil {
		return model.Usage{}, false
	}
	u := model.Usage{Status: str(info, "status"), UpdatedAt: now}
	seen := map[string]bool{}
	// Every window the account has (utilization is a fraction; resetsAt in
	// unix seconds).
	for id, x := range obj(info, "unifiedWindows") {
		w, ok := x.(map[string]any)
		if !ok {
			continue
		}
		f, ok := w["utilization"].(float64)
		if !ok {
			continue
		}
		u.Windows = append(u.Windows, model.UsageWindow{ID: id, Pct: f * 100, ResetsAt: secs(w, "resetsAt"), Status: str(w, "status")})
		seen[id] = true
	}
	// The window currently limiting the account (older CLIs only send this).
	if id := str(info, "rateLimitType"); id != "" && id != "overage" && !seen[id] {
		w := model.UsageWindow{ID: id, ResetsAt: secs(info, "resetsAt"), Status: u.Status}
		f, ok := info["utilization"].(float64)
		switch {
		case ok:
			w.Pct = f * 100
		case u.Status == "rejected":
			w.Pct = 100
		}
		if ok || u.Status == "rejected" {
			u.Windows = append(u.Windows, w)
		}
	}
	return u, len(u.Windows) > 0 || u.Status != ""
}

// claudePlanWindows are the windows of a get_usage answer worth showing.
var claudePlanWindows = []string{"five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet"}

// claudePlanUsage reads the answer to a `get_usage` control request (the
// data behind /usage). It only has plan limits for logins whose token may
// read the account's usage, which `claude setup-token` tokens may not.
func claudePlanUsage(resp map[string]any, now int64) (model.Usage, bool) {
	if ok, _ := resp["rate_limits_available"].(bool); !ok {
		return model.Usage{}, false
	}
	rl := obj(resp, "rate_limits")
	u := model.Usage{Plan: str(resp, "subscription_type"), UpdatedAt: now}
	for _, id := range claudePlanWindows {
		w := obj(rl, id)
		pct, ok := w["utilization"].(float64) // already 0-100 here
		if !ok {
			continue
		}
		var reset int64
		if t, err := time.Parse(time.RFC3339, str(w, "resets_at")); err == nil {
			reset = t.UnixMilli()
		}
		u.Windows = append(u.Windows, model.UsageWindow{ID: id, Pct: pct, ResetsAt: reset})
	}
	return u, len(u.Windows) > 0
}

// codexRateLimits reads a Codex RateLimitSnapshot (from
// account/rateLimits/read or account/rateLimits/updated). Updates are
// sparse: a window that is null is unknown, not cleared, so it is left out.
func codexRateLimits(snap map[string]any) []model.UsageWindow {
	if snap == nil {
		return nil
	}
	prefix, label := "", ""
	if id := str(snap, "limitId"); id != "" && id != "codex" {
		prefix, label = id+":", str(snap, "limitName")
		if label == "" {
			label = id
		}
	}
	var out []model.UsageWindow
	for _, k := range []string{"primary", "secondary"} {
		w := obj(snap, k)
		pct, ok := w["usedPercent"].(float64)
		if !ok {
			continue
		}
		out = append(out, model.UsageWindow{ID: prefix + k, Label: label, Pct: pct,
			ResetsAt: secs(w, "resetsAt"), Mins: int64(num(w, "windowDurationMins"))})
	}
	return out
}

// codexUsage reads the answer to account/rateLimits/read.
func codexUsage(res map[string]any, now int64) (model.Usage, bool) {
	u := model.Usage{UpdatedAt: now}
	snaps := []map[string]any{obj(res, "rateLimits")}
	if by := obj(res, "rateLimitsByLimitId"); len(by) > 0 {
		snaps = snaps[:0]
		for _, x := range by {
			if m, ok := x.(map[string]any); ok {
				snaps = append(snaps, m)
			}
		}
	}
	for _, s := range snaps {
		u.Windows = append(u.Windows, codexRateLimits(s)...)
		if p := str(s, "planType"); p != "" && u.Plan == "" {
			u.Plan = p
		}
	}
	return u, len(u.Windows) > 0
}

// secs reads a unix-seconds timestamp as unix ms (0 when absent).
func secs(m map[string]any, k string) int64 {
	if v, ok := m[k].(float64); ok && v > 0 {
		return int64(v) * 1000
	}
	return 0
}
