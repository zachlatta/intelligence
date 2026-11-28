package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/probe"
	"github.com/zachlatta/intelligence/internal/resources"
)

func TestGrimmHostResourcesViewAndAssets(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	stream := grimmRig(t, h)
	d := h.(*testServer).d

	rec := call(t, h, "POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/app","maxWorkers":8}`, sameOriginFetch)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", rec.Code, rec.Body)
	}
	var created struct{ ID, URL string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	succeed(t, stream, stream.command(), `{"workspaceId":"w-app"}`)

	// Assets and header slot are served.
	if js := get(t, h, "/grimm_host.js", nil); js.Code != http.StatusOK || !strings.Contains(js.Body.String(), "grimm_host_resources") {
		t.Fatalf("grimm_host.js: %d", js.Code)
	}
	if css := get(t, h, "/grimm_host.css", nil); css.Code != http.StatusOK || !strings.Contains(css.Body.String(), ".g-host") {
		t.Fatalf("grimm_host.css: %d", css.Code)
	}
	page := get(t, h, created.URL, nil).Body.String()
	for _, want := range []string{`/grimm_host.css`, `/grimm_host.js`, `id="g-host"`} {
		if !strings.Contains(page, want) {
			t.Errorf("session page missing %s", want)
		}
	}

	// Before any host resource reading, the view still returns a row with has_reading = false.
	q := `SELECT to_jsonb(r) FROM grimm_host_resources r WHERE session_id = '` + created.ID + `'`
	before := sqlQuery(t, h, q)
	if len(before) != 1 {
		t.Fatalf("expected 1 row before reading, got %d", len(before))
	}
	var row map[string]any
	_ = json.Unmarshal(before[0][0], &row)
	if row["host"] != "host-1" || row["has_reading"] != false {
		t.Fatalf("before reading = %+v", row)
	}

	// Add a manager and a worker so session_agents has data to join against agent_minutes.
	for _, stmt := range []string{
		`INSERT INTO grimm_managers (session_id, account_id, epoch, host, agent_id, provider, model, status)
		 SELECT id, account_id, 1, host, 'mgr-1', 'antigravity', 'gemini-4-neon-medium', 'live' FROM grimm_sessions WHERE id = $1`,
		`INSERT INTO grimm_workers (session_id, account_id, name, role, provider, model, host, agent_id, status)
		 SELECT id, account_id, 'nova', 'builder', 'claude', 'claude-opus-5-5', host, 'wk-1', 'running' FROM grimm_sessions WHERE id = $1`,
	} {
		if _, err := d.Owner.Exec(t.Context(), stmt, created.ID); err != nil {
			t.Fatalf("seed agents: %v", err)
		}
	}

	// Open the SSE stream before sending the host resource frame to verify live push.
	srv := serve(t, h)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/grimm/sessions/"+created.ID+"/stream", nil)
	req.AddCookie(h.(*testServer).cookie)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := make(chan [2]string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 4<<20)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				ev = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				events <- [2]string{ev, v}
			}
		}
		close(events)
	}()

	// Send a host resource report frame over the host stream.
	now := time.Now().UTC().Truncate(time.Minute)
	f64 := func(v float64) *float64 { return &v }
	u64 := func(v uint64) *uint64 { return &v }
	hostRep := resources.Report{
		Minutes: []resources.Minute{{
			Minute:           now,
			Samples:          12,
			CPUs:             16,
			CPUAvgPct:        f64(38.4),
			CPUMaxPct:        f64(71.2),
			Load1:            6.4,
			Load5:            5.2,
			Load15:           4.1,
			MemTotal:         64 << 30,
			MemAvailableMin:  24 << 30,
			SwapTotal:        8 << 30,
			SwapUsedMax:      1 << 30,
			DiskTotal:        500 << 30,
			DiskAvailableMin: 120 << 30,
			PSICPUSome:       f64(2.5),
			PSIMemSome:       f64(1.2),
			PSIMemFull:       f64(0.3),
			PSIIOSome:        f64(4.8),
			PSIIOFull:        f64(0.9),
			OOMKills:         u64(1),
			PaseoMemMax:      u64(12 << 30),
			PaseoMemPeak:     u64(14 << 30),
			PaseoOOMKills:    u64(0),
			DiskReadBps:      f64(45 << 20),
			DiskWriteBps:     f64(22 << 20),
			DiskIOPS:         f64(340),
			DiskBusyAvgPct:   f64(18.5),
			DiskBusyMaxPct:   f64(44.0),
			Filesystems: []resources.Filesystem{
				{Mount: "/", Device: "/dev/nvme0n1p2", Type: "ext4", Total: 500 << 30, Available: 120 << 30},
				{Mount: "/data", Device: "/dev/nvme1n1", Type: "xfs", Total: 1000 << 30, Available: 400 << 30},
			},
		}},
		Agents: []resources.AgentMinute{
			{AgentID: "wk-1", Minute: now, CPUSeconds: f64(48.2), CPUSource: "ebpf", MemMax: 4 << 30, ProcessesMax: 12, Execs: u64(42)},
			{AgentID: "mgr-1", Minute: now, CPUSeconds: f64(9.1), CPUSource: "ebpf", MemMax: 1 << 30, ProcessesMax: 4, Execs: u64(7)},
		},
		OOMs: []probe.OOM{
			{At: now, PID: 4242, Comm: "go-test", AgentID: "wk-1", AnonRSS: 3 << 30},
		},
		Probe: &resources.ProbeStatus{Running: true, Version: "abc1234", Tracked: 42},
	}
	if ack := stream.call(hostclient.Frame{Type: hostclient.HostFrame, Host: &hostRep}); ack.Error != "" {
		t.Fatalf("host frame: %s", ack.Error)
	}

	// Verify SSE pushes a host_resources event promptly.
	gotPush := false
	for !gotPush {
		select {
		case e, open := <-events:
			if !open {
				t.Fatal("stream closed before host_resources event")
			}
			if e[0] == "host_resources" && strings.Contains(e[1], `"host":"host-1"`) {
				gotPush = true
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for host_resources SSE event")
		}
	}

	// Re-query grimm_host_resources via /api/sql and verify all fields.
	after := sqlQuery(t, h, q)
	if len(after) != 1 {
		t.Fatalf("expected 1 row after reading, got %d", len(after))
	}
	var full struct {
		Host         string  `json:"host"`
		HasReading   bool    `json:"has_reading"`
		Fresh        bool    `json:"fresh"`
		CPUs         int     `json:"cpus"`
		CPUAvgPct    float64 `json:"cpu_avg_pct"`
		CPUMaxPct    float64 `json:"cpu_max_pct"`
		Load1        float64 `json:"load1"`
		Load5        float64 `json:"load5"`
		Load15       float64 `json:"load15"`
		Load1PerCPU  float64 `json:"load1_per_cpu"`
		MemTotal     int64   `json:"mem_total"`
		SwapTotal    int64   `json:"swap_total"`
		PaseoMemMax  int64   `json:"paseo_mem_max"`
		DiskBusyAvg  float64 `json:"disk_busy_avg_pct"`
		PSIMemSome   float64 `json:"psi_mem_some"`
		ProbeRunning bool    `json:"probe_running"`
		Filesystems  []struct {
			Mount    string  `json:"mount"`
			UsedFrac float64 `json:"used_frac"`
		} `json:"filesystems"`
		RecentOOMs []struct {
			Comm string `json:"comm"`
		} `json:"recent_ooms"`
		SessionAgents []struct {
			Name       string  `json:"name"`
			Role       string  `json:"role"`
			MemMax     int64   `json:"mem_max"`
			CPUSeconds float64 `json:"cpu_seconds"`
		} `json:"session_agents"`
	}
	if err := json.Unmarshal(after[0][0], &full); err != nil {
		t.Fatalf("unmarshal view row: %v\n%s", err, after[0][0])
	}
	if !full.HasReading || !full.Fresh || full.CPUs != 16 || full.CPUAvgPct < 38 || full.PaseoMemMax != 12<<30 || !full.ProbeRunning {
		t.Fatalf("unexpected view fields: %+v", full)
	}
	if len(full.Filesystems) != 2 {
		t.Fatalf("filesystems = %+v, want 2", full.Filesystems)
	}
	if len(full.RecentOOMs) != 1 || full.RecentOOMs[0].Comm != "go-test" {
		t.Fatalf("recent_ooms = %+v", full.RecentOOMs)
	}
	if len(full.SessionAgents) != 2 || full.SessionAgents[0].Name != "nova" {
		t.Fatalf("session_agents = %+v, want nova first", full.SessionAgents)
	}
}

func sqlQuery(t *testing.T, h http.Handler, query string) [][]json.RawMessage {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"query": query})
	rec := call(t, h, "POST", "/api/sql", string(b), sameOriginFetch)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/sql: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Rows [][]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode sql: %v", err)
	}
	return out.Rows
}

const grimmHostHarness = `
const fs = require("fs");
const [file, rowJSON] = process.argv.slice(1);

function makeEl(tag) {
  const listeners = {};
  const e = {
    tag, id: "", className: "", hidden: false, innerHTML: "", textContent: "", dataset: {}, attrs: {}, children: [],
    style: {},
    classList: {
      _set: new Set(),
      add(...xs) { xs.forEach(x => this._set.add(x)); e.className = [...this._set].join(" "); },
      remove(...xs) { xs.forEach(x => this._set.delete(x)); e.className = [...this._set].join(" "); },
      toggle(x, f) { const on = f === undefined ? !this._set.has(x) : !!f; on ? this._set.add(x) : this._set.delete(x); e.className = [...this._set].join(" "); return on; },
      contains(x) { return this._set.has(x); },
    },
    setAttribute(k, v) { this.attrs[k] = String(v); },
    getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
    addEventListener(ev, fn) { (listeners[ev] ||= []).push(fn); },
    dispatch(ev, obj = {}) { (listeners[ev] || []).forEach(fn => fn(obj)); },
    querySelector(sel) {
      if (sel === ".g-host-btn") return this._btn || null;
      if (sel === ".g-host-card") return this._card || null;
      return null;
    },
    contains(other) { return other === this; },
  };
  return e;
}

const docListeners = {};
const root = makeEl("div");
root.id = "grimm";
root.dataset.session = "00000000-0000-4000-8000-000000000001";
const slot = makeEl("div");
slot.id = "g-host";
slot.hidden = true;

global.document = {
  readyState: "complete",
  querySelector(sel) {
    if (sel === "#grimm") return root;
    if (sel === "#g-host") return slot;
    return null;
  },
  addEventListener(ev, fn) { (docListeners[ev] ||= []).push(fn); },
  dispatchEvent(ev) { (docListeners[ev.type] || []).forEach(fn => fn(ev)); },
};
global.window = {
  matchMedia: () => ({ matches: false }),
  addEventListener() {},
};
global.Date.now = () => Date.parse("2026-11-27T23:15:00Z");

let fetchCount = 0;
let lastQuery = "";
global.fetch = async (url, opts) => {
  fetchCount++;
  const body = JSON.parse(opts.body);
  lastQuery = body.query;
  return { ok: true, status: 200, json: async () => ({ rows: [[JSON.parse(rowJSON)]] }) };
};

eval(fs.readFileSync(file, "utf8"));
setTimeout(() => {
  // Trigger a live SSE refresh event and check that it re-queries /api/sql.
  document.dispatchEvent({ type: "grimm:host-resources", detail: { host: "mew-dev" } });
  setTimeout(() => {
    console.log(JSON.stringify({
      hidden: slot.hidden,
      cls: slot.className,
      fetchCount,
      hasView: lastQuery.includes("grimm_host_resources"),
      html: slot.innerHTML,
    }));
    process.exit(0);
  }, 40);
}, 40);
`

func TestGrimmHostWidgetRendersMetersDetailAndStaleState(t *testing.T) {
	t.Parallel()
	node := requireNode(t)
	run := func(row string) string {
		t.Helper()
		out, err := exec.Command(node, "-e", grimmHostHarness, "grimm_host.js", row).CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	freshRow := `{
		"session_id": "00000000-0000-4000-8000-000000000001",
		"host": "demo-box",
		"host_online": true,
		"has_reading": true,
		"minute": "2026-11-27T23:14:30Z",
		"fresh": true,
		"cpus": 16,
		"cpu_avg_pct": 84.5,
		"cpu_max_pct": 96.0,
		"load1": 12.8,
		"load5": 10.4,
		"load15": 8.0,
		"load1_per_cpu": 0.8,
		"mem_total": 68719476736,
		"mem_available_min": 6871947673,
		"mem_used_frac": 0.90,
		"swap_total": 8589934592,
		"swap_used_max": 2147483648,
		"disk_total": 536870912000,
		"disk_available_min": 107374182400,
		"disk_used_frac": 0.80,
		"disk_read_bps": 52428800,
		"disk_write_bps": 26214400,
		"disk_iops": 412,
		"disk_busy_avg_pct": 62.0,
		"disk_busy_max_pct": 88.0,
		"psi_cpu_some": 5.4,
		"psi_mem_some": 3.2,
		"psi_mem_full": 0.8,
		"psi_io_some": 7.1,
		"psi_io_full": 1.5,
		"oom_kills": 2,
		"paseo_mem_max": 17179869184,
		"paseo_mem_peak": 19327352832,
		"paseo_oom_kills": 1,
		"probe_running": true,
		"probe_error": "",
		"filesystems": [
			{"mount": "/", "fs_type": "ext4", "total": 536870912000, "available": 107374182400, "used_frac": 0.80},
			{"mount": "/data", "fs_type": "xfs", "total": 1073741824000, "available": 536870912000, "used_frac": 0.50}
		],
		"recent_ooms": [
			{"at": "2026-11-27T23:10:00Z", "pid": 991, "comm": "node", "agent_id": "wk-1", "anon_rss": 4294967296}
		],
		"session_agents": [
			{"name": "nova", "role": "worker", "agent_id": "wk-1", "mem_max": 6442450944, "cpu_seconds": 124.5, "processes_max": 14}
		]
	}`
	got := run(freshRow)
	for _, want := range []string{
		`"hidden":false`,
		`"fetchCount":2`,
		`"hasView":true`,
		`demo-box`,
		`85%`,       // CPU avg
		`96%`,       // CPU peak
		`90%`,       // Mem used
		`80%`,       // Disk used
		`12.8`,      // Load1
		`0.80/cpu`,  // Load per CPU
		`6.0 GB`,    // Swap avail (or 2.0/8.0 GB)
		`/data`,     // Second filesystem
		`50.0 MB/s`, // Disk read
		`62%`,       // Disk busy
		`16.0 GB`,   // Paseo cgroup mem
		`nova`,      // Busiest session agent
		`node`,      // Recent OOM kill
		`/resources#demo-box`,
		`lv-crit`, // 90% mem triggers critical threshold
	} {
		if !strings.Contains(got, want) {
			t.Errorf("fresh render missing %q in:\n%s", want, got)
		}
	}

	staleRow := `{
		"session_id": "00000000-0000-4000-8000-000000000001",
		"host": "old-box",
		"host_online": false,
		"has_reading": true,
		"minute": "2026-11-27T23:00:00Z",
		"fresh": false,
		"cpus": 4,
		"cpu_avg_pct": 12.0,
		"cpu_max_pct": 20.0,
		"load1": 0.4,
		"load5": 0.3,
		"load15": 0.2,
		"load1_per_cpu": 0.1,
		"mem_total": 17179869184,
		"mem_available_min": 12884901888,
		"mem_used_frac": 0.25,
		"swap_total": 0,
		"swap_used_max": 0,
		"disk_total": 107374182400,
		"disk_available_min": 85899345920,
		"disk_used_frac": 0.20,
		"filesystems": [],
		"recent_ooms": [],
		"session_agents": []
	}`
	stale := run(staleRow)
	for _, want := range []string{`old-box`, `stale`, `15m ago`} {
		if !strings.Contains(stale, want) {
			t.Errorf("stale render missing %q in:\n%s", want, stale)
		}
	}

	// An OOM kill from hours ago with green meters should tint the pill amber (lv-warn), not red (lv-crit).
	oldOomRow := `{
		"session_id": "00000000-0000-4000-8000-000000000001",
		"host": "calm-box",
		"host_online": true,
		"has_reading": true,
		"minute": "2026-11-27T23:14:40Z",
		"fresh": true,
		"cpus": 8,
		"cpu_avg_pct": 15.0,
		"cpu_max_pct": 25.0,
		"load1": 1.0,
		"load5": 1.0,
		"load15": 1.0,
		"load1_per_cpu": 0.125,
		"mem_total": 34359738368,
		"mem_available_min": 25769803776,
		"mem_used_frac": 0.25,
		"disk_total": 107374182400,
		"disk_available_min": 85899345920,
		"disk_used_frac": 0.20,
		"oom_kills": 0,
		"filesystems": [],
		"recent_ooms": [{"at": "2026-11-27T18:00:00Z", "pid": 400, "comm": "python3", "agent_id": "", "anon_rss": 2147483648}],
		"session_agents": []
	}`
	oldOom := run(oldOomRow)
	if !strings.Contains(oldOom, `g-host-btn lv-warn`) || strings.Contains(oldOom, `g-host-btn lv-crit`) {
		t.Errorf("old OOM pill should be lv-warn, not lv-crit:\n%s", oldOom)
	}

	// macOS hosts (Unsupported reports delete their host_probes row -> probe_running: null) must not show "eBPF probe off",
	// whereas a Linux host with a failing probe (probe_running: false, probe_error: "dial ...") still shows it.
	noProbeRow := `{
		"session_id": "00000000-0000-4000-8000-000000000001",
		"host": "mac-box",
		"host_online": true,
		"has_reading": true,
		"minute": "2026-11-27T23:14:40Z",
		"fresh": true,
		"cpus": 10,
		"cpu_avg_pct": 15.0,
		"cpu_max_pct": 25.0,
		"load1": 1.0,
		"mem_total": 34359738368,
		"mem_available_min": 25769803776,
		"mem_used_frac": 0.25,
		"disk_total": 107374182400,
		"disk_available_min": 85899345920,
		"disk_used_frac": 0.20,
		"probe_running": null,
		"probe_error": "",
		"filesystems": [],
		"recent_ooms": [],
		"session_agents": []
	}`
	if out := run(noProbeRow); strings.Contains(out, "eBPF probe off") {
		t.Errorf("null probe_running should not show eBPF probe off:\n%s", out)
	}

	brokenProbeRow := strings.Replace(noProbeRow, `"probe_running": null,`, `"probe_running": false,`, 1)
	brokenProbeRow = strings.Replace(brokenProbeRow, `"probe_error": ""`, `"probe_error": "dial unix: no such file"`, 1)
	if out := run(brokenProbeRow); !strings.Contains(out, "eBPF probe off") {
		t.Errorf("broken Linux probe should show eBPF probe off:\n%s", out)
	}
}
