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

func TestGrimmNewSessionFormHasManagerModelSelector(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	stream := grimmRig(t, h)
	home := get(t, h, "/", nil).Body.String()
	for _, want := range []string{`id="g-new-model"`, `id="g-new-thinking"`, `data-models=`, `claude-opus-5-5`, `gemini-4-neon-medium`} {
		if !strings.Contains(home, want) {
			t.Errorf("new session page missing %q", want)
		}
	}
	rec := call(t, h, "POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/app","managerProvider":"antigravity","managerModel":"gemini-4-neon-medium"}`, sameOriginFetch)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with custom manager model: %d %s", rec.Code, rec.Body)
	}
	var created struct{ ID, URL string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	ws := stream.command()
	succeed(t, stream, ws, `{"workspaceId":"w-app"}`)
	mgr := stream.command()
	var args hostclient.CreateAgentArgs
	_ = json.Unmarshal(mgr.Args, &args)
	if mgr.Kind != hostclient.CreateAgentKind || args.Provider != "antigravity" || args.Model != "gemini-4-neon-medium" {
		t.Fatalf("manager = %s %s, want antigravity/gemini-4-neon-medium", mgr.Kind, mgr.Args)
	}
	page := get(t, h, created.URL, nil).Body.String()
	if !strings.Contains(page, `id="g-model"`) {
		t.Error("session page missing #g-model active manager model header badge")
	}
}

func TestGrimmSSESendsThreadBeforeStateAndResumesWithLastEventID(t *testing.T) {
	t.Parallel()
	h, d := dbServer(t)
	grimmRig(t, h)
	rec := call(t, h, "POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/app"}`, sameOriginFetch)
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO grimm_messages (session_id, account_id, channel, sender, kind, body)
		SELECT s.id, s.account_id, 'thread', 'intelligence', 'notice', 'initial note' FROM grimm_sessions s WHERE s.id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}

	type sseMsg struct {
		id, event, data string
	}
	open := func(lastID string) (<-chan sseMsg, context.CancelFunc) {
		srv := serve(t, h)
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/grimm/sessions/"+created.ID+"/stream", nil)
		req.AddCookie(h.(*testServer).cookie)
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			cancel()
			t.Fatalf("stream status %d", resp.StatusCode)
		}
		ch := make(chan sseMsg, 64)
		go func() {
			defer resp.Body.Close()
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 1<<20), 8<<20)
			var cur sseMsg
			for sc.Scan() {
				line := sc.Text()
				switch {
				case strings.HasPrefix(line, "id: "):
					cur.id = strings.TrimPrefix(line, "id: ")
				case strings.HasPrefix(line, "event: "):
					cur.event = strings.TrimPrefix(line, "event: ")
				case strings.HasPrefix(line, "data: "):
					cur.data = strings.TrimPrefix(line, "data: ")
					ch <- cur
					cur = sseMsg{}
				}
			}
			close(ch)
		}()
		return ch, cancel
	}

	// First connect: thread must arrive BEFORE state, and carry an SSE id.
	ch1, cancel1 := open("")
	first := <-ch1
	second := <-ch1
	cancel1()
	if first.event != "thread" || second.event != "state" {
		t.Fatalf("event order = %q then %q, want thread before state", first.event, second.event)
	}
	if first.id == "" || second.id == "" {
		t.Fatalf("missing SSE id: first=%q second=%q", first.id, second.id)
	}
	if !strings.Contains(first.data, `"reset":true`) || !strings.Contains(first.data, "initial note") {
		t.Fatalf("first thread = %s", first.data)
	}

	// Add a second message while disconnected, then reconnect with Last-Event-ID.
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO grimm_messages (session_id, account_id, channel, sender, kind, body)
		SELECT s.id, s.account_id, 'thread', 'intelligence', 'notice', 'resumed note' FROM grimm_sessions s WHERE s.id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	ch2, cancel2 := open(first.id)
	defer cancel2()
	resumed := <-ch2
	if resumed.event != "thread" {
		t.Fatalf("resumed first event = %q, want thread", resumed.event)
	}
	if strings.Contains(resumed.data, `"reset":true`) {
		t.Fatalf("resumed stream unexpectedly sent reset:true: %s", resumed.data)
	}
	if !strings.Contains(resumed.data, "resumed note") || strings.Contains(resumed.data, "initial note") {
		t.Fatalf("resumed thread should contain only the new item: %s", resumed.data)
	}
}

func TestGrimmJSReasoningOptimisticSendAndRosterTimestamps(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	script := `
const fs = require("fs");
const vm = require("vm");
const listeners = {};
const docListeners = {};
const elListeners = new WeakMap();
function makeEl(tag, id) {
  const classes = new Set();
  const children = [];
  const node = {
    tagName: tag.toUpperCase(),
    id: id || "",
    dataset: id === "grimm" ? { session: "sess-1", capLimit: "100", olderSql: "" } : {},
    style: {},
    hidden: false,
    disabled: false,
    readOnly: false,
    value: "",
    title: "",
    textContent: "",
    _innerHTML: "",
    get innerHTML() { return this._innerHTML; },
    set innerHTML(v) { this._innerHTML = String(v); children.length = 0; },
    get className() { return [...classes].join(" "); },
    set className(v) { classes.clear(); String(v || "").split(/\s+/).filter(Boolean).forEach((c) => classes.add(c)); },
    classList: {
      add(...cs) { cs.forEach((c) => classes.add(c)); },
      remove(...cs) { cs.forEach((c) => classes.delete(c)); },
      toggle(c, force) { if (force === undefined) { if (classes.has(c)) classes.delete(c); else classes.add(c); } else if (force) classes.add(c); else classes.delete(c); },
      contains(c) { return classes.has(c); },
    },
    children,
    get firstChild() { return children[0] || null; },
    get nextSibling() { return null; },
    setAttribute() {},
    addEventListener(ev, fn) { let m = elListeners.get(node); if (!m) { m = {}; elListeners.set(node, m); } (m[ev] ||= []).push(fn); },
    dispatch(ev, arg) { const m = elListeners.get(node); for (const fn of (m && m[ev]) || []) fn(arg || { preventDefault() {} }); },
    focus() {}, select() {},
    querySelector(sel) {
      if (sel.startsWith(".")) {
        const cls = sel.slice(1).split("[")[0];
        if (classes.has(cls)) return node;
        for (const ch of children) { const f = ch.querySelector(sel); if (f) return f; }
        if (this._innerHTML.includes(cls)) return makeEl("span");
      }
      return null;
    },
    querySelectorAll(sel) {
      const out = [];
      if (sel.includes("g-since")) {
        const walk = (n) => {
          if (n.classList.contains("g-since")) out.push(n);
          for (const ch of n.children) walk(ch);
        };
        walk(node);
      }
      return out;
    },
    append(...ns) { for (const n of ns) { children.push(n); n.parentNode = node; } },
    insertBefore(n, ref) {
      const i = children.indexOf(n);
      if (i >= 0) children.splice(i, 1);
      const r = ref ? children.indexOf(ref) : children.length;
      children.splice(r < 0 ? children.length : r, 0, n);
      n.parentNode = node;
    },
    replaceWith(fresh) {
      if (!node.parentNode) return;
      const i = node.parentNode.children.indexOf(node);
      if (i >= 0) node.parentNode.children[i] = fresh;
      fresh.parentNode = node.parentNode;
    },
    remove() {
      if (!node.parentNode) return;
      const i = node.parentNode.children.indexOf(node);
      if (i >= 0) node.parentNode.children.splice(i, 1);
      node.parentNode = null;
    },
    replaceChildren() { children.length = 0; },
    contains() { return false; },
    scrollHeight: 0, scrollTop: 0, clientHeight: 100,
  };
  return node;
}
const byId = {};
const doc = {
  body: makeEl("body", "body"),
  activeElement: null,
  visibilityState: "visible",
  querySelector(sel) {
    const m = sel.match(/^#([\w-]+)$/);
    if (m) return (byId[m[1]] ||= makeEl("div", m[1]));
    return null;
  },
  querySelectorAll() { return []; },
  addEventListener(ev, fn) { (docListeners[ev] ||= []).push(fn); },
  dispatchEvent() {},
  createElement(tag) { return makeEl(tag); },
};
let esURLs = [];
function EventSource(url) {
  esURLs.push(url);
  return {
    readyState: 1,
    addEventListener(ev, fn) { (listeners[ev] ||= []).push(fn); },
    close() { this.readyState = 2; },
  };
}
let resolvePost;
const ctx = {
  document: doc, addEventListener() {}, location: { pathname: "/", href: "/" },
  EventSource, CustomEvent: function(t, o) { return { type: t, detail: o && o.detail }; },
  matchMedia: () => ({ matches: false }),
  requestAnimationFrame: (fn) => fn(),
  setInterval: () => 0, setTimeout: (fn) => { fn(); return 0; }, clearTimeout: () => {},
  CSS: { escape: (s) => s }, sessionStorage: { getItem: () => "", setItem() {}, removeItem() {} },
  fetch: () => new Promise((r) => { resolvePost = r; }),
  console,
};
ctx.window = ctx;
vm.runInNewContext(fs.readFileSync("grimm.js", "utf8"), ctx);

// 1. Live reasoning renders as .g-think and is marked .done when assistant_message arrives.
for (const fn of listeners.thread || []) {
  fn({ lastEventId: "m0:t1.1:b0:e0", data: JSON.stringify({
    reset: true, more: false,
    items: [{ item_id: "t1.1", at: "2026-11-28T07:00:00Z", kind: "reasoning", body: "Checking tests" }],
  }) });
}
const itemsBox = byId["g-items"];
if (itemsBox.children.length !== 1 || !itemsBox.children[0].classList.contains("g-think")) {
  throw new Error("expected .g-think node for reasoning");
}
for (const fn of listeners.thread || []) {
  fn({ lastEventId: "m0:t1.2:b0:e0", data: JSON.stringify({
    reset: false,
    items: [{ item_id: "t1.2", at: "2026-11-28T07:00:01Z", kind: "assistant_message", body: "All set!" }],
  }) });
}
if (!itemsBox.children[0].classList.contains("done")) {
  throw new Error("expected reasoning node to be marked .done once assistant_message arrives");
}

// 2. Optimistic send: composer clears immediately and pending bubble appears BEFORE fetch resolves.
for (const fn of listeners.state || []) {
  fn({ lastEventId: "m0:t1.2:b0:e0", data: JSON.stringify({
    session: { id: "sess-1", host: "host-1", repo: "/w/app", title: "app", branch: "grimm/1", max_workers: 20, manager_state: "idle", manager_provider: "claude", manager_model: "claude-opus-5-5" },
    workers: [], directives: [], quota: [],
  }) });
}
byId["g-input"].value = "Ship it";
byId["g-composer"].dispatch("submit", { preventDefault() {} });
if (byId["g-input"].value !== "") throw new Error("composer did not clear immediately on optimistic send");
if (byId["g-pill"].dataset.state !== "working") throw new Error("pill did not switch to working immediately");
const hasPending = itemsBox.children.some((n) => String(n.dataset.id || "").startsWith("pending-"));
if (!hasPending) throw new Error("no optimistic pending bubble inserted");

// Reconcile when real SSE item arrives.
for (const fn of listeners.thread || []) {
  fn({ lastEventId: "m5:t1.2:b0:e0", data: JSON.stringify({
    reset: false,
    items: [{ item_id: "m5", at: "2026-11-28T07:00:02Z", kind: "human", body: "Ship it", delivered_at: "2026-11-28T07:00:02Z" }],
  }) });
}
if (itemsBox.children.some((n) => String(n.dataset.id || "").startsWith("pending-"))) {
  throw new Error("optimistic pending bubble was not reconciled on SSE arrival");
}

// 3. M3: Sending a follow-up while already working -> going idle clears pill, typing, and Stop.
for (const fn of listeners.state || []) {
  fn({ lastEventId: "m5:t1.2:b0:e0", data: JSON.stringify({
    session: { id: "sess-1", host: "host-1", repo: "/w/app", title: "app", branch: "grimm/1", max_workers: 20, manager_state: "working", manager_provider: "claude", manager_model: "claude-opus-5-5" },
    workers: [], directives: [], quota: [],
  }) });
}
byId["g-input"].value = "Follow-up while working";
byId["g-composer"].dispatch("submit", { preventDefault() {} });
for (const fn of listeners.thread || []) {
  fn({ lastEventId: "m6:t1.2:b0:e0", data: JSON.stringify({
    reset: false,
    items: [{ item_id: "m6", at: "2026-11-28T07:00:03Z", kind: "human", body: "Follow-up while working", delivered_at: "2026-11-28T07:00:03Z" }],
  }) });
}
for (const fn of listeners.state || []) {
  fn({ lastEventId: "m6:t1.2:b0:e0", data: JSON.stringify({
    session: { id: "sess-1", host: "host-1", repo: "/w/app", title: "app", branch: "grimm/1", max_workers: 20, manager_state: "idle", manager_provider: "claude", manager_model: "claude-opus-5-5" },
    workers: [], directives: [], quota: [],
  }) });
}
if (byId["g-pill"].dataset.state !== "idle") throw new Error("M3: pill stuck in " + byId["g-pill"].dataset.state + " after going idle");
if (!byId["g-typing"].hidden) throw new Error("M3: typing indicator stayed visible after going idle");
if (!byId["g-stop"].hidden) throw new Error("M3: Stop button stayed visible after going idle");

// 4. M1: Incremental SSE delta appends to existing assistant_message in place.
for (const fn of listeners.thread || []) {
  fn({ lastEventId: "m6:t1.2:b0:e0", data: JSON.stringify({
    reset: false, items: [],
    deltas: [{ item_id: "t1.2", kind: "assistant_message", append: " More text." }],
  }) });
}

// 5. visibilitychange reconnects with lastEventId when EventSource closed.
doc.visibilityState = "hidden";
for (const fn of docListeners.visibilitychange || []) fn();
doc.visibilityState = "visible";
for (const fn of docListeners.visibilitychange || []) fn();
if (esURLs.length < 1) throw new Error("EventSource was not opened");
`
	cmd := exec.Command("node", "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("grimm.js streaming behavior check failed: %v\n%s", err, out)
	}
}

func TestGrimmMobileCSSKeepsManagerModelVisible(t *testing.T) {
	t.Parallel()
	css := string(grimmCSS)
	idx := strings.Index(css, "@media (max-width: 780px)")
	if idx < 0 {
		t.Fatal("missing mobile media query in grimm.css")
	}
	mobile := css[idx:]
	if strings.Contains(mobile, ".g-model { display: none") || strings.Contains(mobile, ".g-model,") && strings.Contains(mobile, "display: none") {
		t.Fatalf("M4: .g-model must remain visible on mobile (<=780px):\n%s", mobile)
	}
}

func TestGrimmSSEEmitsIncrementalDeltaOnGrowingReply(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	stream := grimmRig(t, h)
	rec := call(t, h, "POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/app"}`, sameOriginFetch)
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	succeed(t, stream, stream.command(), `{"workspaceId":"w-app"}`)
	succeed(t, stream, stream.command(), `{"agentId":"mgr-1"}`)
	if rec := call(t, h, "POST", "/api/grimm/sessions/"+created.ID+"/messages", `{"text":"Hi"}`, sameOriginFetch); rec.Code != 202 {
		t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}
	rep := wfReport()
	rep.Paseo.Agents = append(rep.Paseo.Agents, hostclient.Agent{ID: "mgr-1", WorkspaceID: "w-app", Provider: "claude", Status: "running", CreatedAt: time.Now()})
	if ack := stream.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	events, cancel := openSSE(t, h, "/api/grimm/sessions/"+created.ID+"/stream")
	defer cancel()
	now := time.Now()
	if ack := stream.call(hostclient.Frame{Type: hostclient.TranscriptFrame, Transcript: &hostclient.TranscriptBatch{
		Host: "host-1", AgentID: "mgr-1", Epoch: "e1", Replace: true,
		Entries: []hostclient.TranscriptEntry{
			{SeqStart: 1, SeqEnd: 1, At: now, Item: json.RawMessage(`{"type":"assistant_message","text":"Hello"}`)},
		},
	}}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-events:
			if e[0] == "thread" && strings.Contains(e[1], "Hello") {
				goto sentFirst
			}
		case <-deadline:
			t.Fatal("never saw initial Hello")
		}
	}
sentFirst:
	if ack := stream.call(hostclient.Frame{Type: hostclient.TranscriptFrame, Transcript: &hostclient.TranscriptBatch{
		Host: "host-1", AgentID: "mgr-1", Epoch: "e1",
		Entries: []hostclient.TranscriptEntry{
			{SeqStart: 1, SeqEnd: 2, At: now, Item: json.RawMessage(`{"type":"assistant_message","text":"Hello world"}`)},
		},
	}}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	for {
		select {
		case e := <-events:
			if e[0] == "thread" && strings.Contains(e[1], `"deltas"`) && strings.Contains(e[1], `"append":" world"`) {
				if strings.Contains(e[1], `"Hello world"`) {
					t.Fatalf("M1: expected incremental delta suffix only, got full body: %s", e[1])
				}
				return
			}
		case <-deadline:
			t.Fatal("M1: never saw incremental delta event for growing reply")
		}
	}
}

func TestGrimmNewSessionFormOmitsDisabledManagerProviders(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	stream := grimmRig(t, h)
	rep := wfReport()
	rep.Paseo.Providers = []hostclient.Provider{
		{Name: "claude", Available: true, Models: []hostclient.Model{{ID: "claude-opus-5-5", Label: "Claude Opus 5.5"}}},
		{Name: "muse", Available: true, Models: []hostclient.Model{{ID: "muse-disabled-model", Label: "Muse"}}},
	}
	if ack := stream.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	home := get(t, h, "/", nil).Body.String()
	if !strings.Contains(home, `claude-opus-5-5`) {
		t.Fatal("new session page missing the default manager model")
	}
	if strings.Contains(home, `muse-disabled-model`) {
		t.Error("new session page offers a disabled provider's model")
	}
}
