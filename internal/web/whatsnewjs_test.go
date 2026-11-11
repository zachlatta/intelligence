package web

import (
	"os/exec"
	"strings"
	"testing"
)

// whatsNewHarness runs whatsnew.js (argv[1]) against a small fake DOM and fetch, on a page at
// argv[2], and prints what the top bar ended up saying and what was written.
const whatsNewHarness = `
const fs = require("fs"), assert = require("assert");
const [file, path, unseenJSON, act] = process.argv.slice(1);
function el(tag, attrs = {}) {
  const e = { tag, children: [], attrs: { ...attrs }, dataset: {}, hidden: false, textContent: "", className: "", listeners: {},
    setAttribute(k, v) { this.attrs[k] = String(v); }, getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
    removeAttribute(k) { delete this.attrs[k]; },
    append(...xs) { for (const x of xs) this.children.push(typeof x === "string" ? { text: x } : x); },
    after(x) { inserted.push(x); }, remove() { this.removed = true; },
    addEventListener(k, f) { this.listeners[k] = f; },
    querySelector(sel) { return find(this, sel); } };
  return e;
}
function find(root, sel) {
  const cls = sel.startsWith(".") ? sel.slice(1) : null;
  for (const c of root.children || []) {
    if (cls && (c.className || "").split(" ").includes(cls)) return c;
    const f = find(c, sel); if (f) return f;
  }
  return null;
}
const inserted = [], posts = [];
const count = el("span", { "aria-hidden": "true" }); count.className = "unseen-count"; count.hidden = true;
const link = el("a", { href: "/whats-new" }); link.textContent = "What's New"; link.children.push(count);
const top = el("header"); top.className = "topbar";
const cards = path === "/whats-new" ? JSON.parse(unseenJSON).map((u) => { const c = el("article"); c.dataset.unseen = u[0]; return c; }) : [];
global.document = {
  readyState: "complete",
  getElementById: (id) => id === "nav-whats-new" ? link : null,
  querySelector: (sel) => sel === "header.topbar" ? top : sel === ".whatsnew-note" ? (inserted.find((x) => !x.removed) || null) : null,
  querySelectorAll: (sel) => sel === "[data-unseen]" ? cards : [],
  createElement: (tag) => el(tag),
  addEventListener() {},
};
global.location = { pathname: path };
let unseen = JSON.parse(unseenJSON);
global.fetch = async (url, opts) => {
  const body = JSON.parse(opts.body);
  if (url === "/api/whats-new/seen") { posts.push(body.ids); unseen = unseen.filter((u) => !body.ids.includes(u[0])); return { ok: true, status: 204 }; }
  return { ok: true, status: 200, json: async () => ({ rows: unseen }) };
};
eval(fs.readFileSync(file, "utf8"));
setTimeout(async () => {
  const note = inserted.find((x) => !x.removed);
  const out = { label: link.getAttribute("aria-label"), count: count.hidden ? "" : count.textContent, posts: posts.slice(), note: !!note };
  if (act === "dismiss" && note) {
    const dismiss = (function walk(e) { for (const c of e.children || []) { if (c.textContent === "Dismiss") return c; const f = walk(c); if (f) return f; } })(note);
    await dismiss.listeners.click();
    out.after = { label: link.getAttribute("aria-label"), count: count.hidden ? "" : count.textContent, posts, note: !note.removed };
  }
  console.log(JSON.stringify(out));
}, 50);
`

// The top bar says how many entries are unseen in words, not only with a mark: the tab's
// accessible name carries the count, and the mark is the count itself.
func TestWhatsNewScriptBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	three := `[["c","Third"],["b","Second"],["a","First"]]`
	for _, tc := range []struct{ name, path, unseen, act, want string }{
		{"elsewhere, three unseen", "/", three, "",
			`{"label":"What's New, 3 unseen","count":"3","posts":[],"note":true}`},
		{"elsewhere, one unseen", "/", `[["a","First"]]`, "",
			`{"label":"What's New, 1 unseen","count":"1","posts":[],"note":true}`},
		{"nothing unseen", "/", `[]`, "",
			`{"label":null,"count":"","posts":[],"note":false}`},
		{"the What's New page marks what it shows", "/whats-new", three, "",
			`{"label":null,"count":"","posts":[["c","b","a"]],"note":false}`},
		{"dismissing the note", "/", three, "dismiss",
			`{"label":"What's New, 3 unseen","count":"3","posts":[],"note":true,"after":{"label":null,"count":"","posts":[["c","b","a"]],"note":false}}`},
	} {
		out, err := exec.Command(node, "-e", whatsNewHarness, "whatsnew.js", tc.path, tc.unseen, tc.act).CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != tc.want {
			t.Errorf("%s: %v\n got %s\nwant %s", tc.name, err, strings.TrimSpace(string(out)), tc.want)
		}
	}
}
