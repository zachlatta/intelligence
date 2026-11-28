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
)

// grimmRig streams as host-1 with Claude Opus 5.5 and Antigravity Gemini 4 Neon Medium in model groups with headroom.
func grimmRig(t *testing.T, h http.Handler) *rawHost {
	t.Helper()
	d := h.(*testServer).d
	rep := wfReport()
	rep.Paseo.Providers = []hostclient.Provider{
		{Name: "claude", Available: true, Models: []hostclient.Model{{ID: "claude-opus-5-5", Label: "Claude Opus 5.5", Thinking: []string{"low", "medium", "high"}, DefaultThinking: "medium"}}},
		{Name: "antigravity", Available: true, Models: []hostclient.Model{{ID: "gemini-4-neon-medium", Label: "Gemini 4 Neon Medium"}}},
	}
	stream := openStream(t, h, "host-1")
	if ack := stream.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	for _, q := range []string{
		`INSERT INTO usage_accounts(provider,account,first_seen,last_seen) VALUES('claude','one',now(),now())`,
		`INSERT INTO usage_sources(host,provider,account,ok,received_at) VALUES('host-1','claude','one',true,now())`,
		`INSERT INTO usage_readings(provider,account,window_id,window_seconds,used_pct,resets_at,first_observed,last_observed) VALUES('claude','one','7d',604800,20,now()+interval '1 day',now(),now())`,
		`INSERT INTO usage_active(host,provider,account,since,until) VALUES('host-1','claude','one',now(),now())`,
		`INSERT INTO usage_accounts(provider,account,first_seen,last_seen) VALUES('antigravity','one',now(),now())`,
		`INSERT INTO usage_sources(host,provider,account,ok,received_at) VALUES('host-1','antigravity','one',true,now())`,
		`INSERT INTO usage_readings(provider,account,window_id,window_seconds,used_pct,resets_at,first_observed,last_observed) VALUES('antigravity','one','gemini/7d',604800,30,now()+interval '1 day',now(),now())`,
		`INSERT INTO usage_active(host,provider,account,since,until) VALUES('host-1','antigravity','one',now(),now())`,
	} {
		if _, err := d.Owner.Exec(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"name":"Frontier","members":[
		{"provider":"claude","model":"claude-opus-5-5","thinking":"medium","login":"one","longWindow":"7d","enabled":true},
		{"provider":"antigravity","model":"gemini-4-neon-medium","thinking":"","login":"one","longWindow":"gemini/7d","enabled":true}
	]}`
	if rec := call(t, h, "POST", "/api/model-groups", body, sameOriginFetch); rec.Code != 201 {
		t.Fatalf("model group: %d %s", rec.Code, rec.Body)
	}
	return stream
}

func TestGrimmIsTheHomePageAndHostsMoveToHosts(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	grimmRig(t, h)
	home := get(t, h, "/", nil)
	body := home.Body.String()
	if home.Code != 200 || !strings.Contains(body, "Start a Grimm session") || !strings.Contains(body, `<option value="host-1"`) ||
		!strings.Contains(body, `href="/" aria-current="page">Grimm</a>`) || !strings.Contains(body, `/grimm.js`) {
		t.Fatalf("home page: %d\n%s", home.Code, body)
	}
	if !strings.Contains(body, `value="20"`) {
		t.Error("the new-session form does not default the cap to 20")
	}
	hosts := get(t, h, "/hosts", nil).Body.String()
	if !strings.Contains(hosts, "Login fix") || !strings.Contains(hosts, `href="/hosts" aria-current="page">Hosts</a>`) {
		t.Fatalf("hosts page:\n%s", hosts)
	}
}

// The new-session form starts in desk: each host option carries its desk checkout for the
// repository field, the default host is one with desk, and a session made with no repository
// named starts in desk.
func TestGrimmNewSessionDefaultsToDesk(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	stream := grimmRig(t, h)
	rep := wfReport()
	rep.Paseo.Projects = []hostclient.Project{{ID: "p-desk", Name: "desk", Kind: "git", Directory: "/w/desk"}}
	if ack := stream.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	body := get(t, h, "/", nil).Body.String()
	if !strings.Contains(body, `<option value="host-1" data-desk="/w/desk"`) || !strings.Contains(body, `data-desk="/w/desk" data-repos=`) {
		t.Fatalf("the host option does not carry its desk checkout:\n%s", body)
	}
	if strings.Contains(body, "does not run in the desk") {
		t.Error("the page still refuses desk")
	}
	rec := call(t, h, "POST", "/api/grimm/sessions", `{"host":"host-1"}`, sameOriginFetch)
	if rec.Code != http.StatusCreated {
		t.Fatalf("session with no repository: %d %s", rec.Code, rec.Body)
	}
	var repo string
	if err := h.(*testServer).d.Owner.QueryRow(t.Context(), `SELECT repo FROM grimm_sessions`).Scan(&repo); err != nil || repo != "/w/desk" {
		t.Fatalf("session repo = %q, %v", repo, err)
	}
}

func TestDefaultGrimmHostPrefersDesk(t *testing.T) {
	t.Parallel()
	hosts := []grimmHost{{Name: "a", Online: true}, {Name: "b", Online: true, Desk: "/w/desk"}, {Name: "c", Online: false, Desk: "/w/desk"}}
	for _, c := range []struct{ preferred, want string }{{"a", "a"}, {"", "b"}, {"c", "b"}, {"zz", "b"}} {
		if got := defaultGrimmHost(hosts, c.preferred); got != c.want {
			t.Errorf("defaultGrimmHost(%q) = %q, want %q", c.preferred, got, c.want)
		}
	}
	if got := defaultGrimmHost(hosts[:1], ""); got != "a" {
		t.Errorf("no desk anywhere: %q", got)
	}
}

func TestGrimmSessionStreamsItsThreadAndState(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	stream := grimmRig(t, h)
	rec := call(t, h, "POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/app","maxWorkers":12}`, sameOriginFetch)
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct{ ID, URL string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	ws := stream.command()
	if ws.Kind != hostclient.CreateWorkspaceKind {
		t.Fatalf("first command = %+v", ws)
	}
	succeed(t, stream, ws, `{"workspaceId":"w-app"}`)

	page := get(t, h, created.URL, nil).Body.String()
	if !strings.Contains(page, `data-session="`+created.ID+`"`) || !strings.Contains(page, `id="g-cap"`) {
		t.Fatalf("session page:\n%s", page)
	}
	if home := get(t, h, "/", nil).Body.String(); !strings.Contains(home, `data-session="`+created.ID+`"`) {
		t.Error("the home page does not open the newest session")
	}

	srv := serve(t, h)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/grimm/sessions/"+created.ID+"/stream", nil)
	req.AddCookie(h.(*testServer).cookie)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header)
	}
	events := make(chan [2]string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
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
	wait := func(what string, ok func(ev, data string) bool) {
		t.Helper()
		for {
			select {
			case e, open := <-events:
				if !open {
					t.Fatalf("stream ended waiting for %s", what)
				}
				if ok(e[0], e[1]) {
					return
				}
			case <-ctx.Done():
				t.Fatalf("no %s", what)
			}
		}
	}
	wait("the first state", func(ev, data string) bool {
		return ev == "state" && strings.Contains(data, `"max_workers":12`)
	})
	mgr := stream.command() // the supervisor pre-starts the manager as soon as the worktree is ready
	var args hostclient.CreateAgentArgs
	_ = json.Unmarshal(mgr.Args, &args)
	if mgr.Kind != hostclient.CreateAgentKind || args.Provider != "claude" || args.Model != "claude-opus-5-5" || args.Labels[hostclient.LiveLabel] != hostclient.LiveFast {
		t.Fatalf("manager = %s %s", mgr.Kind, mgr.Args)
	}
	succeed(t, stream, mgr, `{"agentId":"mgr-1"}`)
	if rec := call(t, h, "POST", "/api/grimm/sessions/"+created.ID+"/messages", `{"text":"Build the CSV tool"}`, sameOriginFetch); rec.Code != 202 {
		t.Fatalf("message: %d %s", rec.Code, rec.Body)
	}
	wait("the message, pushed", func(ev, data string) bool { return ev == "thread" && strings.Contains(data, "Build the CSV tool") })
	rep := wfReport()
	rep.Paseo.Providers = []hostclient.Provider{{Name: "claude", Available: true, Models: []hostclient.Model{{ID: "claude-opus-5-5", Thinking: []string{"medium"}}}}}
	rep.Paseo.Agents = append(rep.Paseo.Agents, hostclient.Agent{ID: "mgr-1", WorkspaceID: "w-app", Provider: "claude", Status: "running", CreatedAt: time.Now()})
	if ack := stream.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	wait("the manager working", func(ev, data string) bool {
		return ev == "state" && strings.Contains(data, `"manager_state":"working"`)
	})
	// the manager's words stream into the thread as its transcript grows
	batch := hostclient.TranscriptBatch{Host: "host-1", AgentID: "mgr-1", Epoch: "e1", Replace: true, Entries: []hostclient.TranscriptEntry{
		{SeqStart: 1, SeqEnd: 1, At: time.Now(), Item: json.RawMessage(`{"type":"user_message","text":"first prompt"}`)},
		{SeqStart: 2, SeqEnd: 2, At: time.Now(), Item: json.RawMessage(`{"type":"assistant_message","text":"Hiring a builder"}`)}}}
	if ack := stream.call(hostclient.Frame{Type: hostclient.TranscriptFrame, Transcript: &batch}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	wait("the manager's words", func(ev, data string) bool {
		return ev == "thread" && strings.Contains(data, "Hiring a builder") && !strings.Contains(data, "first prompt")
	})
	batch = hostclient.TranscriptBatch{Host: "host-1", AgentID: "mgr-1", Epoch: "e1", Entries: []hostclient.TranscriptEntry{
		{SeqStart: 2, SeqEnd: 3, At: time.Now(), Item: json.RawMessage(`{"type":"assistant_message","text":"Hiring a builder and a verifier"}`)}}}
	if ack := stream.call(hostclient.Frame{Type: hostclient.TranscriptFrame, Transcript: &batch}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	wait("the words growing in place", func(ev, data string) bool { return ev == "thread" && strings.Contains(data, "and a verifier") })
}

func TestGrimmRoutesKeepToTheAccountAndTheCallingAgent(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	grimmRig(t, h)
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/api/grimm/sessions", `{"host":"host-9","repo":"/w/app"}`, 400},
		{"POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/app","maxWorkers":500}`, 400},
		{"POST", "/api/grimm/sessions/00000000-0000-4000-8000-000000000009/messages", `{"text":"hi"}`, 404},
		{"GET", "/grimm/00000000-0000-4000-8000-000000000009", "", 404},
	} {
		if rec := call(t, h, tc.method, tc.path, tc.body, sameOriginFetch); rec.Code != tc.want {
			t.Errorf("%s %s: %d %s, want %d", tc.method, tc.path, rec.Code, rec.Body, tc.want)
		}
	}
	if rec := call(t, h, "POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/app"}`, map[string]string{"Sec-Fetch-Site": "cross-site"}); rec.Code != 403 {
		t.Errorf("a cross-site create: %d", rec.Code)
	}
	if rec := call(t, anon(h), "POST", "/api/grimm/agent", `{"verb":"roster"}`, nil); rec.Code != 401 {
		t.Errorf("no host key: %d", rec.Code)
	}
	key := keyOf(t, h, "host-1")
	rec := call(t, anon(h), "POST", "/api/grimm/agent", `{"verb":"roster"}`, map[string]string{"Authorization": "Bearer " + key, "X-Paseo-Agent-Id": "someone-else"})
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "not part of an active Grimm session") {
		t.Errorf("an agent outside Grimm: %d %s", rec.Code, rec.Body)
	}
}

// openSSE opens a stream as the fixture's account and returns its events, until cancel.
func openSSE(t *testing.T, h http.Handler, path string) (<-chan [2]string, context.CancelFunc) {
	t.Helper()
	srv := serve(t, h)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
	req.AddCookie(h.(*testServer).cookie)
	resp, err := srv.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		cancel()
		t.Fatalf("%s: %d", path, resp.StatusCode)
	}
	events := make(chan [2]string, 256)
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
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
	return events, cancel
}

// readySession is a session on host-1 whose worktree is made, with worker athena running as
// agent w-1; it returns the session's id and the host's stream.
func readySession(t *testing.T, h http.Handler) (string, *rawHost) {
	t.Helper()
	stream := grimmRig(t, h)
	rec := call(t, h, "POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/app"}`, sameOriginFetch)
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	succeed(t, stream, stream.command(), `{"workspaceId":"w-app"}`)
	// The supervisor pre-starts the manager as soon as the worktree is ready.
	if mgrCmd := stream.command(); mgrCmd.Kind == hostclient.CreateAgentKind {
		succeed(t, stream, mgrCmd, `{"agentId":"mgr-ready"}`)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec = call(t, h, "POST", "/api/grimm/sessions/"+created.ID+"/workers", `{"name":"athena","role":"builder","task":"build it"}`, sameOriginFetch)
		if rec.Code == 201 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond) // the supervisor records the worktree
	}
	if rec.Code != 201 {
		t.Fatalf("hire: %d %s", rec.Code, rec.Body)
	}
	succeed(t, stream, stream.command(), `{"agentId":"w-1"}`)
	d := h.(*testServer).d
	for {
		var agent string
		_ = d.Owner.QueryRow(t.Context(), `SELECT agent_id FROM grimm_workers WHERE name = 'athena'`).Scan(&agent)
		if agent == "w-1" {
			return created.ID, stream
		}
		if time.Now().After(deadline) {
			t.Fatal("the worker never got its agent")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A worker's live log being open makes its host copy its transcript at once; closing it sets the
// steady pace back.
func TestGrimmWorkerFocusSpeedsUpItsTranscript(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	id, stream := readySession(t, h)
	events, cancel := openSSE(t, h, "/api/grimm/sessions/"+id+"/workers/athena/stream")
	watch := func(want string) {
		t.Helper()
		c := stream.command()
		var a hostclient.AgentArgs
		_ = json.Unmarshal(c.Args, &a)
		if c.Kind != hostclient.WatchAgentKind || a.AgentID != "w-1" || a.Live != want {
			t.Fatalf("command = %s %s, want watch_agent %s", c.Kind, c.Args, want)
		}
	}
	watch(hostclient.LiveFast)
	if e := <-events; e[0] != "worker" {
		t.Fatalf("first event %v", e)
	}
	cancel()
	watch(hostclient.LiveSteady)
}

// A long thread starts with its newest 200 items and says there are more; the page reads the
// older ones through /api/sql with the query it is given, a page at a time, to the first.
func TestGrimmThreadLoadsOlderItems(t *testing.T) {
	t.Parallel()
	h, d := dbServer(t)
	grimmRig(t, h)
	rec := call(t, h, "POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/app"}`, sameOriginFetch)
	var created struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO grimm_messages (session_id, account_id, channel, sender, kind, body, created_at)
		SELECT s.id, s.account_id, 'thread', 'intelligence', 'notice', 'note ' || n, now() - interval '1 hour' + n * interval '1 second'
		FROM grimm_sessions s, generate_series(1, 450) n`); err != nil {
		t.Fatal(err)
	}
	page := get(t, h, "/grimm/"+created.ID, nil).Body.String()
	if !strings.Contains(page, `data-older-sql="`) || !strings.Contains(page, `id="g-older"`) {
		t.Fatal("the page has no way to load older items")
	}
	events, cancel := openSSE(t, h, "/api/grimm/sessions/"+created.ID+"/stream")
	defer cancel()
	var first struct {
		Items []struct {
			ItemID string `json:"item_id"`
			At     string `json:"at"`
			Body   string `json:"body"`
		}
		Reset, More bool
	}
	for e := range events {
		if e[0] == "thread" {
			_ = json.Unmarshal([]byte(e[1]), &first)
			break
		}
	}
	if len(first.Items) != grimmThreadPage || !first.Reset || !first.More || first.Items[len(first.Items)-1].Body != "note 450" {
		t.Fatalf("first page: %d items, reset %v, more %v", len(first.Items), first.Reset, first.More)
	}
	lit := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	oldest := first.Items[0]
	seen := len(first.Items)
	for oldest.Body != "note 1" {
		query := strings.NewReplacer(":session", lit(created.ID), ":at", lit(oldest.At), ":item", lit(oldest.ItemID)).Replace(grimmOlderSQL)
		body, _ := json.Marshal(map[string]string{"query": query})
		code, r := sqlPost(t, h, string(body))
		if code != 200 || len(r.Rows) == 0 {
			t.Fatalf("older: %d %+v", code, r)
		}
		seen += len(r.Rows)
		var o struct {
			ItemID string `json:"item_id"`
			At     string `json:"at"`
			Body   string `json:"body"`
		}
		last, _ := json.Marshal(r.Rows[len(r.Rows)-1][0])
		_ = json.Unmarshal(last, &o)
		oldest.ItemID, oldest.At, oldest.Body = o.ItemID, o.At, o.Body
	}
	if seen != 450 {
		t.Errorf("read %d items in all, want 450", seen)
	}
}

func TestGrimmPageHasPolishAndInlineCap(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	stream := grimmRig(t, h)
	rec := call(t, h, "POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/app","maxWorkers":10}`, sameOriginFetch)
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct{ ID, URL string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	succeed(t, stream, stream.command(), `{"workspaceId":"w-app"}`)

	page := get(t, h, created.URL, nil).Body.String()
	for _, want := range []string{
		`id="g-cap-pop"`,
		`id="g-cap-dec"`,
		`id="g-cap-input"`,
		`id="g-cap-inc"`,
		`id="g-cap-error"`,
		`id="g-attach-drafts"`,
		`id="g-attach-btn"`,
		`id="g-attach-status"`,
		`class="g-empty-mark"`,
		`id="g-host"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("grimm page missing %s", want)
		}
	}
	if strings.Contains(string(grimmJS), "prompt(") {
		t.Error("grimm.js still uses window.prompt() for worker cap")
	}

	events, cancel := openSSE(t, h, "/api/grimm/sessions/"+created.ID+"/stream")
	defer cancel()
	for e := range events {
		if e[0] == "state" && strings.Contains(e[1], `"max_workers":10`) {
			break
		}
	}
	if rec := call(t, h, "PUT", "/api/grimm/sessions/"+created.ID, `{"maxWorkers":6}`, sameOriginFetch); rec.Code != 204 {
		t.Fatalf("update cap: %d %s", rec.Code, rec.Body)
	}
	for e := range events {
		if e[0] == "state" && strings.Contains(e[1], `"max_workers":6`) {
			return
		}
	}
	t.Fatal("SSE stream did not push updated max_workers")
}

func TestGrimmEmptyThreadDoesNotThrow(t *testing.T) {
	t.Parallel()
	// Regression: a new session's first SSE thread event has items: null (or omitted).
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	script := `
const fs = require("fs");
const vm = require("vm");
const listeners = {};
const elStub = (id) => ({
  id, dataset: { session: "sess-1", capLimit: "100", olderSql: "" },
  style: {}, classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
  setAttribute() {}, addEventListener() {}, focus() {}, select() {},
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
  document: doc, addEventListener() {}, location: { pathname: "/", href: "/" },
  EventSource, CustomEvent: function(t, o) { return { type: t, detail: o && o.detail }; },
  matchMedia: () => ({ matches: false }),
  requestAnimationFrame: (fn) => fn(),
  setInterval: () => 0, setTimeout: () => 0, clearTimeout: () => {},
  CSS: { escape: (s) => s }, sessionStorage: { getItem: () => "", setItem() {}, removeItem() {} },
  console,
};
ctx.window = ctx;
vm.runInNewContext(fs.readFileSync("grimm.js", "utf8"), ctx);
for (const payload of ['{"items":null,"reset":true,"more":false}', '{"reset":true,"more":false}']) {
  for (const fn of listeners.thread || []) fn({ data: payload });
}
`
	cmd := exec.Command("node", "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("grimm.js threw on empty thread event: %v\n%s", err, out)
	}
}
