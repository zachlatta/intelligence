package web

import (
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestTheSubmitKeysScriptIsServed(t *testing.T) {
	h, _ := dbServer(t)
	rec := get(t, h, "/submitkeys.js", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("submitkeys.js: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec.Body.String() != string(submitKeysJS) || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("submitkeys.js should be served whole and revalidated on each load")
	}
}

// Every page loads the shared ⌘/Ctrl Enter handler from the layout, so a new text box gets it too.
func TestEveryPageLoadsTheSubmitKeys(t *testing.T) {
	h, _ := cmdServer(t)
	c := signInAs(t, h)
	sendBatch(t, h, batch(true, "ep1", tentry(1, `{"type":"user_message","text":"go"}`)))
	wf := createWorkflow(t, h, workflow.Starting)
	for _, path := range []string{"/", "/usage", "/resources", "/login", "/account", "/schedules", "/hosts/host-1/schedules/new", "/workflows/email", "/gmail/callback", "/workflows", "/workflows/" + wf,
		newAgentPath, "/hosts/host-1/workspaces/new", "/hosts/host-1/agents/" + agentA1} {
		rec := call(t, h, http.MethodGet, path, "", nil, c)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d", path, rec.Code)
			continue
		}
		if n := strings.Count(rec.Body.String(), `<script src="/submitkeys.js" defer></script>`); n != 1 {
			t.Errorf("%s loads submitkeys.js %d times, want 1", path, n)
		}
	}
}

// A text box outside a form names the button ⌘/Ctrl Enter presses, with data-submit on an ancestor.
func TestTextBoxesOutsideAFormNameTheirSendButton(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	wf := createWorkflow(t, h, workflow.Starting)
	list := get(t, h, "/workflows", nil).Body.String()
	if !strings.Contains(list, `<div class="composer" data-submit="#submit-task"`) {
		t.Error("the new task box should send with #submit-task")
	}
	ed := get(t, h, "/workflows/"+wf, nil).Body.String()
	for _, want := range []string{`<main class="container page" data-submit="#save">`, `<div class="card" data-submit="#json-apply">`} {
		if !strings.Contains(ed, want) {
			t.Errorf("the editor misses %q", want)
		}
	}
	email, err := os.ReadFile("email.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`class="owners" data-submit="#save-owners"`, `id="paste" data-submit="#paste-go"`} {
		if !strings.Contains(string(email), want) {
			t.Errorf("email text box misses %q", want)
		}
	}

	// the reply box only shows while a run waits on Zach, so check its template
	src, err := os.ReadFile("workflows.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `<div class="reply-host" data-submit="#send"`) {
		t.Error("the reply box should send with #send")
	}
}

// submitKeysHarness drives submitkeys.js in Node with a small fake DOM: just what the script touches.
const submitKeysHarness = `
const assert = require("assert");
let handler;
const byId = {};
globalThis.document = {
  addEventListener(type, f) { if (type === "keydown") handler = f; },
  querySelector(sel) { return byId[sel] || null; },
};
const log = [];
function el(tag, parent, props = {}) {
  const e = Object.assign({ tagName: tag.toUpperCase(), parent, dataset: {}, disabled: false, attrs: {} }, props);
  e.closest = (sels) => {
    for (let n = e; n; n = n.parent)
      for (const s of sels.split(",").map((s) => s.trim()))
        if (s.startsWith("[") ? s.slice(1, -1) in n.attrs : n.tagName === s.toUpperCase()) return n;
    return null;
  };
  e.click = () => log.push("click " + e.id);
  e.requestSubmit = (b) => log.push("submit " + e.id + " via " + (b ? b.id : "none"));
  if (props.id) byId["#" + props.id] = e;
  return e;
}
eval(require("fs").readFileSync(process.argv[1], "utf8"));
assert(handler, "a keydown listener on document");

function press(target, keys = {}) {
  log.length = 0;
  const e = Object.assign({ key: "Enter", target, metaKey: false, ctrlKey: false, shiftKey: false, altKey: false,
    isComposing: false, repeat: false, defaultPrevented: false }, keys);
  e.preventDefault = () => { e.defaultPrevented = true; };
  handler(e);
  return { log: log.slice(), prevented: e.defaultPrevented };
}

// a form: the default (first submit) button submits it, through the form's own submit handler
const form = el("form", null, { id: "f" });
const dictate = el("button", form, { id: "dictate", type: "button" });
const save = el("button", form, { id: "save", type: "submit" });
const cancel = el("button", form, { id: "cancel", type: "submit" });
form.elements = [dictate, save, cancel];
const area = el("textarea", el("div", form));
const input = el("input", form, { type: "text" });

assert.deepStrictEqual(press(area, { ctrlKey: true }), { log: ["submit f via save"], prevented: true }, "Ctrl Enter");
assert.deepStrictEqual(press(area, { metaKey: true }), { log: ["submit f via save"], prevented: true }, "⌘ Enter");
assert.deepStrictEqual(press(input, { metaKey: true }), { log: ["submit f via save"], prevented: true }, "⌘ Enter in an input");
assert.deepStrictEqual(press(area), { log: [], prevented: false }, "plain Enter keeps its newline");
assert.deepStrictEqual(press(area, { metaKey: true, shiftKey: true }), { log: [], prevented: false }, "⌘ Shift Enter");
assert.deepStrictEqual(press(area, { ctrlKey: true, altKey: true }), { log: [], prevented: false }, "Ctrl ⌥ Enter");
assert.deepStrictEqual(press(area, { key: "a", metaKey: true }), { log: [], prevented: false }, "⌘ A");
assert.deepStrictEqual(press(area, { metaKey: true, isComposing: true }), { log: [], prevented: false }, "while composing");
assert.deepStrictEqual(press(area, { metaKey: true, repeat: true }).log, [], "a held key sends once");
assert.deepStrictEqual(press(area, { metaKey: true, defaultPrevented: true }).log, [], "a box's own handler wins");
save.disabled = true;
assert.deepStrictEqual(press(area, { metaKey: true }).log, [], "a disabled button does not send");
save.disabled = false;
const bare = el("form", null, { id: "bare", elements: [] });
assert.deepStrictEqual(press(el("input", bare), { ctrlKey: true }).log, ["submit bare via none"], "a form with no button");

// outside a form: data-submit names the button, and the nearest of the two wins
const page = el("main", null, { attrs: { "data-submit": "" }, dataset: { submit: "#send" } });
const send = el("button", null, { id: "send" });
const reply = el("textarea", el("div", page));
assert.deepStrictEqual(press(reply, { metaKey: true }), { log: ["click send"], prevented: true }, "data-submit");
send.disabled = true;
assert.deepStrictEqual(press(reply, { metaKey: true }).log, [], "a disabled data-submit button");
send.disabled = false;
const inner = el("form", page, { id: "inner" });
inner.elements = [el("button", inner, { id: "ok", type: "submit" })];
assert.deepStrictEqual(press(el("textarea", inner), { metaKey: true }).log, ["submit inner via ok"], "the form inside wins");
const lost = el("div", null, { attrs: { "data-submit": "" }, dataset: { submit: "#nothing" } });
assert.deepStrictEqual(press(el("textarea", lost), { metaKey: true }).log, [], "a missing button");

// a box with nothing to send, and a key outside any box, do nothing
assert.deepStrictEqual(press(el("input", el("div", null), { type: "search" }), { metaKey: true }), { log: [], prevented: false }, "no form");
assert.deepStrictEqual(press(el("div", form), { metaKey: true }), { log: [], prevented: false }, "not a text box");
const editable = el("div", form, { attrs: { contenteditable: "" } });
assert.deepStrictEqual(press(editable, { metaKey: true }).log, ["submit f via save"], "contenteditable");
console.log("ok");
`

// The handler itself, in Node with a fake DOM. Skipped where there is no Node.
func TestSubmitKeysBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	out, err := exec.Command(node, "-e", submitKeysHarness, "submitkeys.js").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("submitkeys.js: %v\n%s", err, out)
	}
}
