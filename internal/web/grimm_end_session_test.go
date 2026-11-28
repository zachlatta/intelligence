package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
)

func TestGrimmEndSessionAPIAndUIAndReadOnlySQL(t *testing.T) {
	t.Parallel()
	h, d := dbServer(t)
	id1, stream := readySession(t, h)

	// Start a manager in session 1 and store a transcript entry for it.
	if rec := call(t, h, "POST", "/api/grimm/sessions/"+id1+"/messages", `{"text":"Check the parser"}`, sameOriginFetch); rec.Code != 202 {
		t.Fatalf("message: %d %s", rec.Code, rec.Body)
	}
	mgrCmd := stream.command()
	succeed(t, stream, mgrCmd, `{"agentId":"mgr-1"}`)
	rep := wfReport()
	rep.Paseo.Providers = []hostclient.Provider{{Name: "antigravity", Available: true, Models: []hostclient.Model{{ID: "gemini-4-neon-medium"}}}}
	rep.Paseo.Agents = append(rep.Paseo.Agents,
		hostclient.Agent{ID: "mgr-1", WorkspaceID: "w-app", Provider: "antigravity", Status: "running", CreatedAt: time.Now()},
		hostclient.Agent{ID: "w-1", WorkspaceID: "w-app", Provider: "antigravity", Status: "running", CreatedAt: time.Now()},
	)
	if ack := stream.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	batch := hostclient.TranscriptBatch{Host: "host-1", AgentID: "mgr-1", Epoch: "e1", Replace: true, Entries: []hostclient.TranscriptEntry{
		{SeqStart: 1, SeqEnd: 1, At: time.Now(), Item: json.RawMessage(`{"type":"assistant_message","text":"Parser looks solid."}`)},
	}}
	if ack := stream.call(hostclient.Frame{Type: hostclient.TranscriptFrame, Transcript: &batch}); ack.Error != "" {
		t.Fatal(ack.Error)
	}

	// Wait until supervisor records mgr-1 as live.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var status string
		_ = d.Owner.QueryRow(t.Context(), `SELECT status FROM grimm_managers WHERE session_id = $1 ORDER BY epoch DESC LIMIT 1`, id1).Scan(&status)
		if status == "live" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("manager never became live (status=%q)", status)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Create a second active session so ending session 1 navigates to session 2.
	rec := call(t, h, "POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/other","title":"Other Repo"}`, sameOriginFetch)
	if rec.Code != 201 {
		t.Fatalf("create second session: %d %s", rec.Code, rec.Body)
	}
	var created2 struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created2)
	id2 := created2.ID
	succeed(t, stream, stream.command(), `{"workspaceId":"w-other"}`)

	// Active session page has the End session button and confirm dialog.
	page1 := get(t, h, "/grimm/"+id1, nil).Body.String()
	for _, want := range []string{`id="g-end-session"`, `id="g-end-dialog"`, `id="g-end-confirm"`} {
		if !strings.Contains(page1, want) {
			t.Errorf("active session page missing %s", want)
		}
	}

	// Cross-site archive is refused with 403.
	if rec := call(t, h, "POST", "/api/grimm/sessions/"+id1+"/archive", `{}`, map[string]string{"Sec-Fetch-Site": "cross-site"}); rec.Code != 403 {
		t.Errorf("cross-site archive: %d, want 403", rec.Code)
	}

	// End session 1 via JSON API.
	archRec := call(t, h, "POST", "/api/grimm/sessions/"+id1+"/archive", `{}`, sameOriginFetch)
	if archRec.Code != http.StatusOK {
		t.Fatalf("archive session 1: %d %s", archRec.Code, archRec.Body)
	}
	var archBody struct {
		Status  string `json:"status"`
		NextURL string `json:"nextUrl"`
	}
	if err := json.Unmarshal(archRec.Body.Bytes(), &archBody); err != nil {
		t.Fatalf("decode archive response: %v (%s)", err, archRec.Body)
	}
	if archBody.Status != "archived" || archBody.NextURL != "/grimm/"+id2 {
		t.Errorf("archive response = %+v, want status=archived nextUrl=/grimm/%s", archBody, id2)
	}

	// Host reports mgr-1 and w-1 gone (archived in Paseo): transcripts MUST still stay readable via /api/sql.
	repGone := wfReport()
	repGone.Paseo.Providers = rep.Paseo.Providers
	if ack := stream.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &repGone}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	code, res := sqlPost(t, h, `{"query":"SELECT body FROM grimm_thread WHERE session_id = '`+id1+`' AND kind = 'assistant_message'"}`)
	if code != 200 || len(res.Rows) != 1 || res.Rows[0][0] != "Parser looks solid." {
		t.Fatalf("grimm_thread after archive + host report: %d %+v", code, res)
	}
	code, res = sqlPost(t, h, `SELECT name, status FROM grimm_workers WHERE session_id = '`+id1+`'`)
	_ = code
	// Use JSON body for sqlPost:
	code, res = sqlPost(t, h, `{"query":"SELECT name, status FROM grimm_workers WHERE session_id = '`+id1+`'"}`)
	if code != 200 || len(res.Rows) != 1 || res.Rows[0][0] != "athena" || res.Rows[0][1] != "archived" {
		t.Fatalf("grimm_workers after archive: %d %+v", code, res)
	}

	// Home page now opens session 2, and session 1 is in the collapsed Ended group (not active list).
	home := get(t, h, "/", nil).Body.String()
	if !strings.Contains(home, `data-session="`+id2+`"`) {
		t.Errorf("home did not move to active session 2")
	}
	if !strings.Contains(home, `class="g-ended-group"`) || !strings.Contains(home, `/grimm/`+id1) {
		t.Errorf("home picker missing collapsed Ended group with session 1")
	}

	// Opening ended session 1 directly shows read-only state (data-status="archived").
	endedPage := get(t, h, "/grimm/"+id1, nil).Body.String()
	if !strings.Contains(endedPage, `data-status="archived"`) || !strings.Contains(endedPage, `id="g-ended-banner"`) {
		t.Errorf("ended session page missing archived/read-only markers")
	}

	// End session 2 (the last active session): nextUrl is "/" (start-a-session form).
	archRec2 := call(t, h, "POST", "/api/grimm/sessions/"+id2+"/archive", `{}`, sameOriginFetch)
	if archRec2.Code != http.StatusOK {
		t.Fatalf("archive session 2: %d %s", archRec2.Code, archRec2.Body)
	}
	var archBody2 struct {
		Status  string `json:"status"`
		NextURL string `json:"nextUrl"`
	}
	_ = json.Unmarshal(archRec2.Body.Bytes(), &archBody2)
	if archBody2.NextURL != "/" {
		t.Errorf("last session archive nextUrl = %q, want /", archBody2.NextURL)
	}

	// Home page now shows the start-a-session form with collapsed Ended sessions.
	homeEmpty := get(t, h, "/", nil).Body.String()
	if !strings.Contains(homeEmpty, "Start a Grimm session") || !strings.Contains(homeEmpty, `<details class="g-ended-group`) {
		t.Errorf("home after ending all sessions missing start form or collapsed Ended group")
	}
}

// TestGrimmEndSessionBrowser serves two seeded sessions with throwaway data and runs the headless
// browser script at GRIMM_END_BROWSER_SCRIPT (python3) to verify desktop & 390px phone UI and
// capture sign-off + What's New screenshots. Skipped unless GRIMM_END_BROWSER_SCRIPT is set.
func TestGrimmEndSessionBrowser(t *testing.T) {
	t.Parallel()
	script := os.Getenv("GRIMM_END_BROWSER_SCRIPT")
	if script == "" {
		t.Skip("GRIMM_END_BROWSER_SCRIPT not set")
	}
	h, d := dbServer(t)
	stream := grimmRig(t, h)

	// Session 1: active session with manager + 2 workers (1 running, 1 idle) and realistic made-up messages.
	rec1 := call(t, h, "POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/csv-tool","title":"CSV Formatter","maxWorkers":10}`, sameOriginFetch)
	if rec1.Code != 201 {
		t.Fatalf("create 1: %d %s", rec1.Code, rec1.Body)
	}
	var c1 struct{ ID string }
	_ = json.Unmarshal(rec1.Body.Bytes(), &c1)
	succeed(t, stream, stream.command(), `{"workspaceId":"w-csv"}`)

	// Session 2: second active session so ending session 1 moves cleanly to session 2.
	rec2 := call(t, h, "POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/docs-site","title":"Docs Site","maxWorkers":8}`, sameOriginFetch)
	if rec2.Code != 201 {
		t.Fatalf("create 2: %d %s", rec2.Code, rec2.Body)
	}
	var c2 struct{ ID string }
	_ = json.Unmarshal(rec2.Body.Bytes(), &c2)
	succeed(t, stream, stream.command(), `{"workspaceId":"w-docs"}`)

	// Wait for supervisor to record both workspaces.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		_ = d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM grimm_sessions WHERE workspace_id <> ''`).Scan(&n)
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("workspaces never recorded")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Send a message to session 1 and hire two workers.
	if rec := call(t, h, "POST", "/api/grimm/sessions/"+c1.ID+"/messages", `{"text":"Add a --markdown flag to csvfmt with unit tests."}`, sameOriginFetch); rec.Code != 202 {
		t.Fatalf("message: %d %s", rec.Code, rec.Body)
	}
	succeed(t, stream, stream.command(), `{"agentId":"mgr-demo"}`)
	if rec := call(t, h, "POST", "/api/grimm/sessions/"+c1.ID+"/workers", `{"name":"athena","role":"builder","task":"Implement --markdown flag and table formatter"}`, sameOriginFetch); rec.Code != 201 {
		t.Fatalf("hire athena: %d %s", rec.Code, rec.Body)
	}
	succeed(t, stream, stream.command(), `{"agentId":"w-athena"}`)
	if rec := call(t, h, "POST", "/api/grimm/sessions/"+c1.ID+"/workers", `{"name":"hermes","role":"worker","task":"Write edge-case tests for quoted commas and empty cells"}`, sameOriginFetch); rec.Code != 201 {
		t.Fatalf("hire hermes: %d %s", rec.Code, rec.Body)
	}
	succeed(t, stream, stream.command(), `{"agentId":"w-hermes"}`)

	rep := wfReport()
	rep.Paseo.Providers = []hostclient.Provider{{Name: "antigravity", Available: true, Models: []hostclient.Model{{ID: "gemini-4-neon-medium"}}}}
	rep.Paseo.Agents = append(rep.Paseo.Agents,
		hostclient.Agent{ID: "mgr-demo", WorkspaceID: "w-csv", Provider: "antigravity", Status: "idle", CreatedAt: time.Now()},
		hostclient.Agent{ID: "w-athena", WorkspaceID: "w-csv", Provider: "antigravity", Status: "running", CreatedAt: time.Now()},
		hostclient.Agent{ID: "w-hermes", WorkspaceID: "w-csv", Provider: "antigravity", Status: "idle", CreatedAt: time.Now()},
	)
	if ack := stream.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	now := time.Now()
	batch := hostclient.TranscriptBatch{Host: "host-1", AgentID: "mgr-demo", Epoch: "e1", Replace: true, Entries: []hostclient.TranscriptEntry{
		{SeqStart: 1, SeqEnd: 1, At: now, Item: json.RawMessage(`{"type":"user_message","text":"first prompt"}`)},
		{SeqStart: 2, SeqEnd: 2, At: now.Add(time.Second), Item: json.RawMessage(`{"type":"assistant_message","text":"I hired **athena** to implement the Markdown table formatter and **hermes** to cover edge cases with unit tests. All checks are green and committed on the branch."}`)},
	}}
	if ack := stream.call(hostclient.Frame{Type: hostclient.TranscriptFrame, Transcript: &batch}); ack.Error != "" {
		t.Fatal(ack.Error)
	}

	// Wait for supervisor to record live manager + both workers.
	for {
		var mStatus string
		var wCount int
		_ = d.Owner.QueryRow(t.Context(), `SELECT status FROM grimm_managers WHERE session_id = $1 ORDER BY epoch DESC LIMIT 1`, c1.ID).Scan(&mStatus)
		_ = d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM grimm_workers WHERE session_id = $1 AND agent_id <> ''`, c1.ID).Scan(&wCount)
		if mStatus == "live" && wCount == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("demo session never became ready (mgr=%q workers=%d)", mStatus, wCount)
		}
		time.Sleep(50 * time.Millisecond)
	}

	srv := serve(t, h)
	c := h.(*testServer).cookie
	out := os.Getenv("GRIMM_END_BROWSER_OUT")
	if out == "" {
		out = t.TempDir()
	}
	py := os.Getenv("GRIMM_END_BROWSER_PYTHON")
	if py == "" {
		py = "python3"
	}
	cmd := exec.CommandContext(t.Context(), py, script, srv.URL, c1.ID, c2.ID, c.Name, c.Value, out)
	cmd.Env = os.Environ()
	b, err := cmd.CombinedOutput()
	fmt.Println(string(b))
	if err != nil {
		t.Fatalf("browser verification failed: %v", err)
	}
}

// TestGrimmEndDialogCountsOnlyLiveWorkers verifies that the End session confirm dialog counts only
// starting, running, and idle workers (matching grimm_session_status.live_workers) and excludes
// failed and archived workers.
func TestGrimmEndDialogCountsOnlyLiveWorkers(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	script := `
const fs = require("fs");
const vm = require("vm");
const listeners = {};
const elListeners = {};
const elStub = (id) => ({
  id, dataset: { session: "sess-1", status: "active", capLimit: "100", olderSql: "" },
  style: {}, classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
  setAttribute() {}, addEventListener(ev, fn) { (elListeners[id + ":" + ev] ||= []).push(fn); },
  focus() {}, select() {},
  querySelector() { return null; }, querySelectorAll() { return []; },
  append() {}, replaceChildren() {}, contains() { return false; },
  scrollHeight: 0, scrollTop: 0, clientHeight: 100, hidden: false, innerHTML: "", textContent: "", value: "",
});
const byId = {};
const doc = {
  body: elStub("body"),
  activeElement: null,
  querySelector(sel) {
    const m = sel.match(/^#([\w-]+)$/);
    if (m) return (byId[m[1]] ||= elStub(m[1]));
    return null;
  },
  querySelectorAll() { return []; },
  addEventListener() {},
  dispatchEvent() {},
  createElement(tag) { return elStub(tag); },
};
function EventSource() {
  return {
    addEventListener(ev, fn) { (listeners[ev] ||= []).push(fn); },
    close() {},
  };
}
const ctx = {
  document: doc, addEventListener() {}, location: { pathname: "/grimm/sess-1", href: "/grimm/sess-1" },
  EventSource, CustomEvent: function(t, o) { return { type: t, detail: o && o.detail }; },
  matchMedia: () => ({ matches: false }),
  requestAnimationFrame: (fn) => fn(),
  setInterval: () => 0, setTimeout: () => 0, clearTimeout: () => {},
  CSS: { escape: (s) => s }, sessionStorage: { getItem: () => "", setItem() {}, removeItem() {} },
  console,
};
ctx.window = ctx;
vm.runInNewContext(fs.readFileSync("grimm.js", "utf8"), ctx);

function emitState(workers, liveWorkers) {
  const payload = JSON.stringify({
    session: { id: "sess-1", status: "active", title: "Demo", host: "host-1", repo: "/w/app", branch: "grimm/12345678", max_workers: 10, live_workers: liveWorkers },
    workers,
    directives: [],
    quota: [],
  });
  for (const fn of listeners.state || []) fn({ data: payload });
}
function clickEnd() {
  for (const fn of elListeners["g-end-session:click"] || []) fn();
}

// 3 live (starting, running, idle) + 1 failed + 1 archived => must report 3 live workers, not 4.
emitState([
  { name: "athena", status: "running", provider: "claude", model: "opus", login: "a" },
  { name: "hermes", status: "idle", provider: "codex", model: "gpt", login: "a" },
  { name: "iris", status: "starting", provider: "codex", model: "gpt", login: "a" },
  { name: "atlas", status: "failed", provider: "claude", model: "opus", login: "a" },
  { name: "apollo", status: "archived", provider: "claude", model: "opus", login: "a" },
], 3);
clickEnd();
const lead1 = byId["g-end-lead"].textContent;
if (!lead1.includes("3 live workers")) {
  throw new Error("expected 3 live workers (excluding failed and archived), got: " + lead1);
}

// Only failed and archived workers => must report 0 live workers.
emitState([
  { name: "atlas", status: "failed", provider: "claude", model: "opus", login: "a" },
  { name: "apollo", status: "archived", provider: "claude", model: "opus", login: "a" },
], 0);
clickEnd();
const lead2 = byId["g-end-lead"].textContent;
if (!lead2.includes("0 live workers")) {
  throw new Error("expected 0 live workers (excluding failed and archived), got: " + lead2);
}
`
	cmd := exec.Command("node", "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("end dialog live worker count check failed: %v\n%s", err, out)
	}
}
