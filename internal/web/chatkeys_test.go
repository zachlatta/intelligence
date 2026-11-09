package web

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// chatKeysHarness runs the agent page's chat script in Node with a small fake DOM and fetch: just
// what the script touches. The commands POST stays pending until the test lets it go.
const chatKeysHarness = `
const assert = require("assert");
const listeners = {};
function el(props = {}) {
  const e = Object.assign({ dataset: {}, hidden: false, disabled: false, textContent: "", value: "", className: "", own: {} }, props);
  e.addEventListener = (type, f) => { (e.own[type] ||= []).push(f); };
  e.fire = (type, ev) => (e.own[type] || []).forEach((f) => f(ev));
  e.focus = () => {};
  e.replaceChildren = () => {};
  return e;
}
const text = el();
const buttons = { queue: el(), interrupt: el(), steer: el() };
const form = el({ elements: { text }, dataset: { commands: "/api/hosts/h/commands", agent: "a1", pendingSql: "SELECT 1", queueTtl: "60s", status: "running" } });
form.querySelector = (sel) => buttons[sel.match(/=(\w+)/)[1]];
form.querySelectorAll = () => Object.values(buttons);
const byId = { chat: form, "chat-hint": el(), pending: el(), working: el(), "working-for": el() };
globalThis.document = {
  title: "Agent", getElementById: (id) => byId[id],
  addEventListener: () => {}, dispatchEvent: () => {}, createElement: () => el(),
};
globalThis.sessionStorage = { getItem: () => null, setItem: () => {}, removeItem: () => {} };
globalThis.location = { pathname: "/" };
globalThis.setInterval = () => 0;

const posts = [];
let release;
globalThis.fetch = (url, init) => {
  if (url === "/api/sql") return Promise.resolve({ ok: true, json: async () => ({ rows: [] }) });
  posts.push(JSON.parse(init.body).args.behavior);
  return new Promise((ok) => { release = () => ok({ ok: true, status: 200,
    json: async () => ({ id: "00000000-0000-4000-8000-00000000000" + posts.length }) }); });
};
eval(process.argv[1]);

function key(keys = {}) {
  const e = Object.assign({ key: "Enter", shiftKey: false, altKey: false, metaKey: false, ctrlKey: false, isComposing: false, repeat: false, prevented: false }, keys);
  e.preventDefault = () => { e.prevented = true; };
  text.fire("keydown", e);
  return e.prevented;
}
const tick = () => new Promise((ok) => setTimeout(ok, 0));

(async () => {
  text.value = "go";
  assert(key(), "Enter is the chat's own");
  assert.deepStrictEqual(posts, ["steer"]);
  // while that send is in flight nothing sends again, by key or by button
  assert(key({ metaKey: true }), "⌘ Enter still keeps its newline out");
  key({ ctrlKey: true }); key({ altKey: true }); key();
  form.fire("submit", { preventDefault() {}, submitter: buttons.interrupt });
  assert.deepStrictEqual(posts, ["steer"], "a second send while the first is in flight");
  release(); await tick(); await tick();
  assert(!buttons.interrupt.disabled, "the buttons come back");

  // a held key sends once
  text.value = "again";
  assert(key({ metaKey: true, repeat: true }), "a repeat still keeps its newline out");
  assert.deepStrictEqual(posts, ["steer"], "a held key's repeat sends nothing");

  // each key keeps its behaviour
  key({ metaKey: true }); assert.deepStrictEqual(posts.at(-1), "interrupt"); release(); await tick(); await tick();
  text.value = "x"; key({ ctrlKey: true }); assert.deepStrictEqual(posts.at(-1), "interrupt"); release(); await tick(); await tick();
  text.value = "x"; key({ altKey: true }); assert.deepStrictEqual(posts.at(-1), "queue"); release(); await tick(); await tick();
  text.value = "x"; assert(!key({ shiftKey: true }), "Shift Enter is a new line");
  assert.strictEqual(posts.length, 4);

  // a disabled button's behaviour does not send
  buttons.interrupt.disabled = true;
  key({ metaKey: true });
  assert.strictEqual(posts.length, 4, "a disabled Interrupt");
  buttons.interrupt.disabled = false;
  console.log("ok");
})().catch((e) => { console.error(e); process.exit(1); });
`

// The chat box's keys, in Node with a fake DOM. Skipped where there is no Node.
func TestChatKeysSendOnceAtATime(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	src, err := os.ReadFile("transcript.html")
	if err != nil {
		t.Fatal(err)
	}
	_, shared, foundShared := strings.Cut(string(src), "<script>\nconst isWorking")
	isWorking, _, _ := strings.Cut(shared, "\n")
	_, script, ok := strings.Cut(string(src), "<script>\n// The chat box writes")
	script, _, ok2 := strings.Cut(script, "</script>")
	if !foundShared || !ok || !ok2 {
		t.Fatal("transcript.html: no chat script")
	}
	out, err := exec.Command(node, "-e", chatKeysHarness, "const isWorking"+isWorking+"\n// The chat box writes"+script).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("the chat script: %v\n%s", err, out)
	}
}
