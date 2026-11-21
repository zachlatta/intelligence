package web

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

// hostAlertsHarness runs hostalerts.js (argv[1]) against a small fake DOM and fetch, with
// host_silence_current rows argv[2], and prints what the page ended up showing.
const hostAlertsHarness = `
const fs = require("fs");
const [file, rowsJSON] = process.argv.slice(1);
function el(tag) {
  const e = { tag, children: [], attrs: {}, textContent: "", className: "", href: "",
    setAttribute(k, v) { this.attrs[k] = String(v); }, getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
    append(...xs) { for (const x of xs) this.children.push(typeof x === "string" ? { text: x } : x); },
    after(x) { inserted.push(x); } };
  return e;
}
function text(e) { return e.text !== undefined ? e.text : (e.textContent || "") + (e.children || []).map(text).join(""); }
const inserted = [], queries = [];
const top = el("header");
global.document = {
  readyState: "complete", title: "Hosts · Intelligence",
  querySelector: (sel) => sel === "header.topbar" ? top : null,
  createElement: (tag) => el(tag),
  addEventListener() {},
};
global.fetch = async (url, opts) => {
  const body = JSON.parse(opts.body);
  queries.push(url);
  if (url === "/api/sql" && /host_silence_current/.test(body.query)) return { ok: true, status: 200, json: async () => ({ rows: JSON.parse(rowsJSON) }) };
  return { ok: false, status: 404 };
};
global.Date.now = () => Date.parse("2026-11-19T12:40:00Z");
eval(fs.readFileSync(file, "utf8"));
setTimeout(() => {
  const wrap = inserted[0];
  const notes = wrap ? wrap.children : [];
  console.log(JSON.stringify({ title: document.title, queries, notes: notes.map((n) => ({ role: n.getAttribute("role"), cls: n.className, text: text(n) })) }));
}, 50);
`

func TestHostAlertsScriptShowsARedBannerPerSilentHost(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	run := func(rows string) string {
		t.Helper()
		out, err := exec.Command(node, "-e", hostAlertsHarness, "hostalerts.js", rows).CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// host, last_seen, opened_at, active_runs, oom_kills, last_oom_at, oom_comms, notify, alert_sent_at, alert_error
	got := run(`[["box-1", "2026-11-19T12:07:00Z", "2026-11-19T12:17:30Z", 4, 3, "2026-11-19T12:05:00Z", "ffmpeg, python3", true, "2026-11-19T12:17:31Z", ""]]`)
	for _, want := range []string{
		`"title":"⚠ Host silent · Hosts · Intelligence"`,
		`"role":"alert","cls":"login-alert broken host-silent"`,
		`box-1 stopped reporting`,
		`33 min`,
		`4 runs are waiting on it`,
		`Last recorded: 3 OOM kills up to`,
		`(ffmpeg, python3)`,
		`You were sent a phone notification`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in\n%s", want, got)
		}
	}
	failed := run(`[["box-2", "2026-11-19T12:07:00Z", "2026-11-19T12:17:30Z", 1, 0, null, "", true, null, "PDW notify HTTP 502"]]`)
	for _, want := range []string{`1 run is waiting on it`, `No OOM kills recorded.`, `The phone notification failed: PDW notify HTTP 502`} {
		if !strings.Contains(failed, want) {
			t.Errorf("missing %s in\n%s", want, failed)
		}
	}
	if got := run("[]"); got != `{"title":"Hosts · Intelligence","queries":["/api/sql"],"notes":[]}` {
		t.Errorf("no alerts: %s", got)
	}
}

func TestEveryPageLoadsTheHostAlerts(t *testing.T) {
	h, _ := cmdServer(t)
	if js := get(t, h, "/hostalerts.js", nil); js.Code != http.StatusOK || !strings.Contains(js.Body.String(), "host_silence_current") {
		t.Fatalf("hostalerts.js: %d", js.Code)
	}
	if !strings.Contains(layoutHTML, `<script src="/hostalerts.js" defer></script>`) {
		t.Fatal("the layout does not load hostalerts.js")
	}
}
