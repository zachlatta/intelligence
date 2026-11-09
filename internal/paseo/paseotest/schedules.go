package paseotest

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/coder/websocket"
)

// Schedules, as Paseo 0.15 keeps and answers them: schedule/list (summaries, without runs),
// inspect, create, update, pause, resume, delete and run-once. The daemon pushes nothing when a
// schedule changes. A run-once is answered only when its run ends (FinishRun).

// Schedule is a new-agent schedule as the daemon stores it, with two runs (one succeeded, one
// failed) and a config field (MCP servers) a reader must never copy. status is active, paused or
// completed.
func Schedule(id, status string) map[string]any {
	s := map[string]any{"id": id, "name": "Schedule " + id, "prompt": "check the thing",
		"cadence": map[string]any{"type": "cron", "expression": "0 7 * * *", "timezone": "America/New_York"},
		"target": map[string]any{"type": "new-agent", "config": map[string]any{"provider": "claude", "cwd": "/w/app",
			"model": "claude-opus-5-5", "modeId": "bypassPermissions", "thinkingOptionId": "medium", "isolation": "worktree",
			"archiveOnFinish": true, "title": "Daily check",
			"mcpServers": map[string]any{"secret": map[string]any{"headers": map[string]any{"Authorization": "Bearer never copied"}}}}},
		"status": status, "createdAt": "2026-11-01T10:00:00.000Z", "updatedAt": "2026-11-08T12:05:00.000Z",
		"nextRunAt": "2026-11-09T12:00:00.000Z", "lastRunAt": "2026-11-08T12:05:00.000Z", "pausedAt": nil, "expiresAt": nil,
		"maxRuns": nil,
		"runs": []any{
			map[string]any{"id": "run-" + id + "-1", "scheduledFor": "2026-11-07T12:00:00.000Z", "startedAt": "2026-11-07T12:00:01.000Z",
				"endedAt": "2026-11-07T12:04:00.000Z", "status": "succeeded", "agentId": "agent-run-1", "workspaceId": "wks_run_1",
				"output": "all good", "error": nil},
			map[string]any{"id": "run-" + id + "-2", "scheduledFor": "2026-11-08T12:00:00.000Z", "startedAt": "2026-11-08T12:00:01.000Z",
				"endedAt": "2026-11-08T12:05:00.000Z", "status": "failed", "agentId": nil, "workspaceId": nil,
				"output": nil, "error": "Working directory /w/gone no longer exists"},
		}}
	if status == "paused" {
		s["pausedAt"], s["nextRunAt"] = "2026-11-08T13:00:00.000Z", nil
	}
	return s
}

// SetSchedules replaces the daemon's schedules.
func (f *Daemon) SetSchedules(s ...map[string]any) { f.mu.Lock(); f.schedules = s; f.mu.Unlock() }

// FailSchedules makes every schedule request fail as a daemon without schedules does.
func (f *Daemon) FailSchedules() { f.mu.Lock(); f.noSchedules = true; f.mu.Unlock() }

// ScheduleStatus is a schedule's status, "" for none.
func (f *Daemon) ScheduleStatus(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i := f.find(f.schedules, id); i >= 0 {
		return fmt.Sprint(f.schedules[i]["status"])
	}
	return ""
}

// FinishRun ends a running run (status succeeded or failed) and answers the run-once that started it.
func (f *Daemon) FinishRun(schedule, run, status, output string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.find(f.schedules, schedule)
	if i < 0 {
		return
	}
	s := f.schedules[i]
	for _, r := range s["runs"].([]any) {
		r := r.(map[string]any)
		if r["id"] == run {
			r["status"], r["output"], r["endedAt"], r["agentId"] = status, output, "2026-11-08T14:00:00.000Z", "agent-"+run
		}
	}
	s["lastRunAt"], s["updatedAt"] = "2026-11-08T14:00:00.000Z", "2026-11-08T14:00:00.000Z"
	if w, ok := f.runOnce[run]; ok {
		delete(f.runOnce, run)
		f.write(w.conn, session("schedule/run-once/response", map[string]any{"requestId": w.id, "schedule": copySchedule(s, true), "error": nil}))
	}
}

type waiter struct {
	conn *websocket.Conn
	id   string
}

// copySchedule is s as an answer carries it: with its runs (inspect) or without (a summary).
func copySchedule(s map[string]any, runs bool) map[string]any {
	out := maps.Clone(s)
	if !runs {
		delete(out, "runs")
	} else {
		out["runs"] = slices.Clone(s["runs"].([]any))
	}
	return out
}

// schedule answers the schedule requests; false for any other. Called with f.mu held.
func (f *Daemon) schedule(c *websocket.Conn, id string, msg map[string]any) bool {
	typ := fmt.Sprint(msg["type"])
	if !strings.HasPrefix(typ, "schedule/") {
		return false
	}
	fail := func(err string) {
		f.write(c, session("rpc_error", map[string]any{"requestId": id, "requestType": typ, "error": err, "code": "schedule_request_failed"}))
	}
	if f.noSchedules {
		f.write(c, session("rpc_error", map[string]any{"requestId": id, "error": "Unknown request, try upgrading the daemon"}))
		return true
	}
	answer := func(s map[string]any, runs bool) {
		f.write(c, session(typ+"/response", map[string]any{"requestId": id, "schedule": copySchedule(s, runs), "error": nil}))
	}
	if typ == "schedule/list" {
		list := []any{}
		for _, s := range f.schedules {
			list = append(list, copySchedule(s, false))
		}
		f.write(c, session("schedule/list/response", map[string]any{"requestId": id, "schedules": list, "error": nil}))
		return true
	}
	if typ == "schedule/create" {
		if err := checkCreate(msg); err != "" {
			fail(err)
			return true
		}
		f.nextID++
		target := msg["target"].(map[string]any)
		s := map[string]any{"id": fmt.Sprintf("%08x", f.nextID), "name": msg["name"], "prompt": msg["prompt"], "cadence": msg["cadence"],
			"target": target, "status": "active", "createdAt": "2026-11-08T12:00:00.000Z", "updatedAt": "2026-11-08T12:00:00.000Z",
			"nextRunAt": "2026-11-09T12:00:00.000Z", "lastRunAt": nil, "pausedAt": nil, "expiresAt": msg["expiresAt"],
			"maxRuns": msg["maxRuns"], "runs": []any{}}
		f.schedules = append(f.schedules, s)
		answer(s, false)
		return true
	}
	sid, _ := msg["scheduleId"].(string)
	i := f.find(f.schedules, sid)
	if i < 0 {
		fail("Schedule not found: " + sid)
		return true
	}
	s := f.schedules[i]
	switch typ {
	case "schedule/inspect":
		answer(s, true)
	case "schedule/pause", "schedule/resume":
		if s["status"] == "completed" {
			fail("Schedule " + sid + " is already completed")
			return true
		}
		if typ == "schedule/pause" && s["status"] == "active" {
			s["status"], s["pausedAt"], s["nextRunAt"], s["updatedAt"] = "paused", "2026-11-08T13:00:00.000Z", nil, "2026-11-08T13:00:00.000Z"
		}
		if typ == "schedule/resume" && s["status"] == "paused" {
			s["status"], s["pausedAt"], s["nextRunAt"], s["updatedAt"] = "active", nil, "2026-11-09T12:00:00.000Z", "2026-11-08T13:30:00.000Z"
		}
		answer(s, false)
	case "schedule/delete":
		f.schedules = slices.Delete(f.schedules, i, i+1)
		f.write(c, session("schedule/delete/response", map[string]any{"requestId": id, "scheduleId": sid, "error": nil}))
	case "schedule/update":
		if p, ok := msg["prompt"].(string); ok && strings.TrimSpace(p) == "" {
			fail("Invalid message")
			return true
		}
		if cad, ok := msg["cadence"].(map[string]any); ok {
			if err := checkCadence(cad); err != "" {
				fail(err)
				return true
			}
		}
		for _, k := range []string{"name", "prompt", "cadence", "maxRuns", "expiresAt"} {
			if v, ok := msg[k]; ok {
				s[k] = v
			}
		}
		if patch, ok := msg["newAgentConfig"].(map[string]any); ok {
			target := s["target"].(map[string]any)
			if target["type"] != "new-agent" {
				fail("new-agent config updates are only valid for new-agent target schedules")
				return true
			}
			cfg := maps.Clone(target["config"].(map[string]any))
			maps.Copy(cfg, patch)
			s["target"] = map[string]any{"type": "new-agent", "config": cfg}
		}
		s["updatedAt"] = "2026-11-08T13:45:00.000Z"
		answer(s, true)
	case "schedule/run-once":
		if s["status"] == "completed" {
			fail("Schedule " + sid + " is already completed")
			return true
		}
		for _, r := range s["runs"].([]any) {
			if r.(map[string]any)["status"] == "running" {
				fail("Schedule " + sid + " is already running")
				return true
			}
		}
		f.nextID++
		run := fmt.Sprintf("run-%d", f.nextID)
		s["runs"] = append(s["runs"].([]any), map[string]any{"id": run, "scheduledFor": "2026-11-08T13:50:00.000Z",
			"startedAt": "2026-11-08T13:50:00.000Z", "endedAt": nil, "status": "running", "agentId": nil, "workspaceId": nil,
			"output": nil, "error": nil})
		s["updatedAt"] = "2026-11-08T13:50:00.000Z"
		if f.runOnce == nil {
			f.runOnce = map[string]waiter{}
		}
		f.runOnce[run] = waiter{c, id} // answered when the run ends
	default:
		fail("Invalid message")
	}
	return true
}

// checkCreate validates a schedule/create as the daemon's schema and service do: "" if it is good.
func checkCreate(msg map[string]any) string {
	if p, _ := msg["prompt"].(string); strings.TrimSpace(p) == "" {
		return "Invalid message"
	}
	cad, _ := msg["cadence"].(map[string]any)
	if err := checkCadence(cad); err != "" {
		return err
	}
	target, _ := msg["target"].(map[string]any)
	cfg, _ := target["config"].(map[string]any)
	if target["type"] != "new-agent" || cfg == nil {
		return "Invalid message"
	}
	if p, _ := cfg["provider"].(string); p == "" {
		return "Invalid message"
	}
	if d, _ := cfg["cwd"].(string); d == "" {
		return "Invalid message"
	}
	if iso, ok := cfg["isolation"]; ok && iso != "local" && iso != "worktree" {
		return "Invalid message"
	}
	if m, ok := msg["maxRuns"].(float64); ok && (m < 1 || m != float64(int(m))) {
		return "Invalid message"
	}
	return ""
}

func checkCadence(cad map[string]any) string {
	switch cad["type"] {
	case "every":
		if ms, _ := cad["everyMs"].(float64); ms < 1 || ms != float64(int64(ms)) {
			return "Invalid message"
		}
	case "cron":
		expr, _ := cad["expression"].(string)
		if len(strings.Fields(expr)) != 5 {
			return "Cron expressions must have 5 fields"
		}
	default:
		return "Invalid message"
	}
	return ""
}
