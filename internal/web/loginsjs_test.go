package web

import (
	"os/exec"
	"strings"
	"testing"
)

// loginsHarness runs logins.js (argv[1]) against a small fake DOM and fetch, with login_alerts
// rows argv[2], and prints what the page ended up showing and what was written. With argv[3]
// "fix", it then clicks the first note's Mark fixed.
const loginsHarness = `
const fs = require("fs");
const [file, alertsJSON, act] = process.argv.slice(1);
function el(tag) {
  const e = { tag, children: [], attrs: {}, dataset: {}, hidden: false, textContent: "", className: "", listeners: {}, type: "",
    setAttribute(k, v) { this.attrs[k] = String(v); }, getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
    append(...xs) { for (const x of xs) this.children.push(typeof x === "string" ? { text: x } : x); },
    after(x) { inserted.push(x); }, remove() { this.removed = true; },
    addEventListener(k, f) { this.listeners[k] = f; } };
  return e;
}
function text(e) { return e.text !== undefined ? e.text : (e.textContent || "") + (e.children || []).map(text).join(""); }
function all(e, pred, out = []) { for (const c of e.children || []) { if (pred(c)) out.push(c); all(c, pred, out); } return out; }
const inserted = [], posts = [];
const top = el("header");
global.document = {
  readyState: "complete", title: "Hosts · Intelligence",
  getElementById: () => null,
  querySelector: (sel) => sel === "header.topbar" ? top : null,
  createElement: (tag) => el(tag),
  addEventListener() {},
};
global.location = { pathname: "/" };
Object.defineProperty(globalThis, "navigator", { configurable: true,
  value: { clipboard: { writeText: async (s) => { posts.push(["clipboard", s]); } } } });
global.fetch = async (url, opts) => {
  const body = JSON.parse(opts.body);
  if (url !== "/api/sql") { posts.push([url, body]); return { ok: true, status: 204 }; }
  if (/login_alerts/.test(body.query)) return { ok: true, status: 200, json: async () => ({ rows: JSON.parse(alertsJSON) }) };
  return { ok: true, status: 200, json: async () => ({ rows: [] }) };
};
eval(fs.readFileSync(file, "utf8"));
setTimeout(async () => {
  const wrap = inserted[0];
  const notes = wrap ? wrap.children.filter((c) => !c.removed) : [];
  const out = { title: document.title, notes: notes.map((n) => ({ role: n.getAttribute("role"), cls: n.className, text: text(n) })) };
  if (notes[0]) {
    const copy = all(notes[0], (c) => /^Copy (command|first step)$/.test(c.textContent))[0];
    if (copy) await copy.listeners.click();
  }
  if (act === "fix" && notes[0]) {
    const fixed = all(notes[0], (c) => c.textContent === "Mark fixed")[0];
    await fixed.listeners.click();
    out.removed = !!notes[0].removed;
  }
  out.posts = posts;
  console.log(JSON.stringify(out));
}, 50);
`

func TestLoginsScriptWarnsAndFixes(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	cmd := `T=$(mktemp -d) && mkdir -p \"$T/cfg\" \"$T/data\" && CLAUDE_CONFIG_DIR=\"$T/cfg\" claude auth login --claudeai --email 'ada@example.com' && cswap import \"$T/export.json\" --force`
	broken := `["broken", 7, "host-1", "claude", "ada@example.com", "ada@example.com", "login_expired", "Failed to authenticate: this login reached its time limit", "Paste Copy command into a terminal on the host.", "` + cmd + `", false, "transcript", "2026-11-19T04:38:05Z", null, 4, 1, 3, 0]`
	expiring := `["expiring", null, "host-1", "claude", "grace@example.com", "grace@example.com", "login_expiring", "The sign-in ends 2026-11-20 21:26 UTC", "Renew it before then. Paste Copy command into a terminal on the host.", "` + strings.ReplaceAll(cmd, "ada@", "grace@") + `", false, "meter", "2026-11-19T04:00:00Z", "2026-11-20T21:26:50Z", 0, 0, 0, 0]`
	unknown := `["broken", 8, "host-2", "claude", "", "claude", "login_expired", "Failed to authenticate", "First step: run claude auth login --claudeai on the host.", "claude auth login --claudeai", true, "transcript", "2026-11-19T04:38:05Z", null, 1, 1, 0, 0]`

	run := func(alerts, act string) string {
		t.Helper()
		out, err := exec.Command(node, "-e", loginsHarness, "logins.js", alerts, act).CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	got := run("["+broken+","+expiring+"]", "fix")
	for _, want := range []string{
		`"title":"⚠ Login broken · Hosts · Intelligence"`,
		`"role":"alert","cls":"login-alert broken"`,
		`Claude login ada@example.com on host-1 stopped working`,
		`4 agent turns failed on it: 3 resumed on a working login, 1 waiting for a working login.`,
		`"role":"status","cls":"login-alert expiring"`,
		`Claude login grace@example.com on host-1 ends`,
		`Renew it before then, or every agent using it stops.How to fix`,
		`["clipboard","T=$(mktemp -d) && mkdir -p \"$T/cfg\" \"$T/data\" && CLAUDE_CONFIG_DIR=\"$T/cfg\" claude auth login --claudeai --email 'ada@example.com' && cswap import \"$T/export.json\" --force"]`,
		`["/api/login-incidents/7/resolve",{"note":"marked fixed from /"}]`,
		`"removed":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in\n%s", want, got)
		}
	}
	// where cswap is not known to manage the login, the copy is only the first step, and says so
	if got := run("["+unknown+"]", ""); !strings.Contains(got, "Copy first step") || !strings.Contains(got, `["clipboard","claude auth login --claudeai"]`) {
		t.Errorf("unknown login: %s", got)
	}
	if got := run("[]", ""); got != `{"title":"Hosts · Intelligence","notes":[],"posts":[]}` {
		t.Errorf("no alerts: %s", got)
	}
}
