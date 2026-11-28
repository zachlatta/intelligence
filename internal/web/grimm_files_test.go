package web

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/hostclient"
)

func TestGrimmMarkdownFileLinks(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	script := `
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");
const elStub = (id) => ({
  id, dataset: { session: "11111111-1111-4111-8111-111111111111", capLimit: "100", olderSql: "" },
  style: { setProperty() {}, removeProperty() {} }, classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
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
function EventSource() { return { addEventListener() {}, close() {} }; }
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
const { md, parseFileURL, safeRawBlobType } = ctx.window.grimmLabels;

// 1. Standard markdown file:// link renders as formatted clickable link, not raw literal text
const p = "/workspace/worktrees/demo/grimm-1/work/2026-11-27-launch/brief.md";
const out1 = md("See [brief.md](file://" + p + ") for details.", ["brief"]);
assert.ok(out1.includes('class="g-file-link"'), "expected g-file-link class: " + out1);
assert.ok(out1.includes('data-file-path="' + p + '"'), "expected data-file-path: " + out1);
assert.ok(out1.includes('href="#file-' + encodeURIComponent(p) + '"'), "expected hash href (no GET write endpoint): " + out1);
assert.ok(!out1.includes("[brief.md](file://"), "must not leave raw markdown link: " + out1);
assert.ok(!out1.includes("g-wchip"), "worker name inside file link label must not become a worker chip: " + out1);

// 2. Line anchor (#L10-L25) and localhost host and percent-encoded space
const out2 = md("[my doc.md](file://localhost/workspace/my%20doc.md#L10-L25)");
assert.ok(out2.includes('data-file-path="/workspace/my doc.md"'), "expected decoded path: " + out2);
assert.ok(out2.includes('data-file-hash="#L10-L25"'), "expected hash: " + out2);
assert.ok(out2.includes("my doc.md</a>"), "expected label: " + out2);

// 3. Inline code inside file link label
const out3 = md("[" + String.fromCharCode(96) + "main.go" + String.fromCharCode(96) + "](file:///workspace/main.go)");
assert.ok(out3.includes("<code>main.go</code></a>"), "expected inline code in label: " + out3);

// 4. Bare file:/// URL
const out4 = md("Wrote file:///workspace/report.md now.");
assert.ok(out4.includes('data-file-path="/workspace/report.md"'), "expected bare file link: " + out4);
assert.ok(out4.includes("report.md</a>"), "expected basename label for bare link: " + out4);

// 5. Unsafe links stay unlinked
const bad = md("[x](javascript:alert(1)) [y](data:text/html,hi) [z](file://evil.example/etc/passwd)");
assert.ok(!bad.includes("<a "), "unsafe links must not become anchors: " + bad);

// 6. parseFileURL handles special characters and rejects non-local hosts
const parsed = parseFileURL("file:///workspace/a&amp;b.md#L5");
assert.strictEqual(parsed && parsed.path, "/workspace/a&b.md");
assert.strictEqual(parsed && parsed.hash, "#L5");
assert.strictEqual(parseFileURL("file://remote-host/etc/passwd"), null);
assert.strictEqual(parseFileURL("file:///"), null);

// 7. safeRawBlobType neutralizes active content (HTML, SVG, JS, XML) to text/plain
assert.strictEqual(safeRawBlobType("text/html; charset=utf-8"), "text/plain; charset=utf-8");
assert.strictEqual(safeRawBlobType("image/svg+xml"), "text/plain; charset=utf-8");
assert.strictEqual(safeRawBlobType("application/javascript"), "text/plain; charset=utf-8");
assert.strictEqual(safeRawBlobType("image/png"), "image/png");
assert.strictEqual(safeRawBlobType("application/pdf"), "application/pdf");
`
	cmd := exec.Command("node", "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("grimm.js file:// markdown check failed: %v\n%s", err, out)
	}
}

func TestGrimmReadFileAPIAndSQLPolling(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	id, stream := readySession(t, h)
	rep := wfReport()
	rep.Paseo.Workspaces[0].Directory = "/w/worktrees/grimm-1"
	if ack := stream.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}

	wtFile := "/w/worktrees/grimm-1/work/brief.md"
	apiURL := "/api/grimm/sessions/" + id + "/read-file"
	body, _ := json.Marshal(map[string]string{"path": wtFile})

	// 1. No GET read/write routes exist (API-first: writes are POST, reads are /api/sql)
	if rec := get(t, h, "/grimm/"+id+"/file?path="+wtFile, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("GET /grimm/{id}/file: %d, want 404", rec.Code)
	}
	if rec := call(t, h, "GET", "/api/grimm/sessions/"+id+"/file?path="+wtFile, "", sameOriginFetch); rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/grimm/sessions/{id}/file: %d, want 404", rec.Code)
	}

	// 2. Unauthenticated refused
	if rec := call(t, anon(h), "POST", apiURL, string(body), sameOriginFetch); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon POST read-file: %d %s", rec.Code, rec.Body)
	}

	// 3. Cross-site refused
	if rec := call(t, h, "POST", apiURL, string(body), map[string]string{"Sec-Fetch-Site": "cross-site", "Content-Type": "application/json"}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST read-file: %d %s", rec.Code, rec.Body)
	}

	// 4. Path outside workspace/repo refused with 403 before queuing
	if rec := call(t, h, "POST", apiURL, `{"path":"/etc/passwd"}`, sameOriginFetch); rec.Code != http.StatusForbidden {
		t.Fatalf("outside path: %d %s", rec.Code, rec.Body)
	}

	// 5. Path traversal refused with 400
	if rec := call(t, h, "POST", apiURL, `{"path":"/w/worktrees/grimm-1/../../etc/passwd"}`, sameOriginFetch); rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal path: %d %s", rec.Code, rec.Body)
	}

	// 6. Unknown session refused with 404
	if rec := call(t, h, "POST", "/api/grimm/sessions/00000000-0000-4000-8000-000000000009/read-file", string(body), sameOriginFetch); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown session: %d %s", rec.Code, rec.Body)
	}

	// 7. Valid POST enqueues read_file command and returns 202 {commandId, path, name};
	//    browser reads the result from commands via /api/sql.
	rec := call(t, h, "POST", apiURL, string(body), sameOriginFetch)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("valid POST read-file: %d %s", rec.Code, rec.Body)
	}
	var queued struct {
		CommandID string `json:"commandId"`
		Path      string `json:"path"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &queued); err != nil {
		t.Fatal(err)
	}
	if queued.CommandID == "" || queued.Path != wtFile || queued.Name != "brief.md" {
		t.Fatalf("queued = %+v", queued)
	}

	c := stream.command()
	if c.ID != queued.CommandID || c.Kind != hostclient.ReadFileKind {
		t.Fatalf("host command = %s %s, want %s %s", c.ID, c.Kind, queued.CommandID, hostclient.ReadFileKind)
	}
	var a hostclient.ReadFileArgs
	_ = json.Unmarshal(c.Args, &a)
	if a.Path != wtFile {
		t.Fatalf("command path = %q, want %q", a.Path, wtFile)
	}
	resJSON, _ := json.Marshal(hostclient.CommandResult{
		Content:   "# Brief\n\nAll good.",
		Encoding:  "utf-8",
		MediaType: "text/markdown; charset=utf-8",
		Size:      18,
	})
	succeed(t, stream, c, string(resJSON))

	// Read result through /api/sql exactly as grimm.js does.
	qBody, _ := json.Marshal(map[string]string{
		"query": "SELECT status, result, error FROM commands WHERE id = '" + queued.CommandID + "'",
	})
	code, sqlRes := sqlPost(t, h, string(qBody))
	if code != http.StatusOK || len(sqlRes.Rows) != 1 {
		t.Fatalf("/api/sql commands read: %d %+v", code, sqlRes)
	}
	if sqlRes.Rows[0][0] != "succeeded" {
		t.Fatalf("command status = %v, want succeeded", sqlRes.Rows[0][0])
	}
	gotResult, _ := json.Marshal(sqlRes.Rows[0][1])
	if !strings.Contains(string(gotResult), "# Brief\\n\\nAll good.") {
		t.Fatalf("command result = %s", gotResult)
	}
}
