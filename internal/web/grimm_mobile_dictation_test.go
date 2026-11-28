package web

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

// TestGrimmOffersDictationOnlyWithAKey verifies that the Grimm session page wires the shared
// AssemblyAI dictation engine when ASSEMBLYAI_API_KEY is set, and omits dictation controls and
// scripts when no key is configured.
func TestGrimmOffersDictationOnlyWithAKey(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"k", ""} {
		d := dbtest.New(t)
		h := newServer(t, d, d.Owner, "v", func(c *Config) { c.AssemblyAIKey = key })
		id, _ := readySession(t, h)
		rec := get(t, h, "/grimm/"+id, nil)
		if rec.Code != 200 {
			t.Fatalf("key=%q: status %d: %s", key, rec.Code, rec.Body)
		}
		page := rec.Body.String()
		if strings.Contains(page, "SpeechRecognition") {
			t.Errorf("key=%q: Grimm page should use shared AssemblyAI dictation, not browser SpeechRecognition", key)
		}
		if key == "" {
			for _, bad := range []string{`id="dictate"`, `src="/dictation.js"`, `data-dictation=`} {
				if strings.Contains(page, bad) {
					t.Errorf("without key, Grimm page should not contain %s", bad)
				}
			}
			continue
		}
		for _, want := range []string{
			`data-dictation="/api/dictation/token"`,
			`id="dictate"`,
			`src="/dictation.js"`,
		} {
			if !strings.Contains(page, want) {
				t.Errorf("with key, Grimm page misses %s", want)
			}
		}
		if n := strings.Count(page, `id="dictate"`); n != 1 {
			t.Errorf("with key, Grimm page has %d dictate buttons, want 1", n)
		}
	}
}

// TestGrimmDictationWiringAndCSS checks that grimm.js wires window.dictation to #g-input, #dictate,
// and #g-actions, disables #dictate while sending, and that grimm.css hides #g-actions while the
// dictation bar is open.
func TestGrimmDictationWiringAndCSS(t *testing.T) {
	t.Parallel()
	js := readWebFile(t, "grimm.js")
	css := readWebFile(t, "grimm.css")

	for _, want := range []string{
		`window.dictation(`,
		`tokenPath: composer.dataset.dictation`,
		`row: $("#g-actions")`,
		`$("#dictate")`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("grimm.js misses dictation wiring %q", want)
		}
	}
	for _, want := range []string{
		`.g-actions[hidden]`,
		`.g-box .dictation`,
		`textarea.listening`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("grimm.css misses dictation style %q", want)
		}
	}
}

// TestDictationResumesSuspendedAudioContext checks that dictation.js handles suspended
// AudioContext (iOS Safari) and missing navigator.mediaDevices cleanly.
func TestDictationResumesSuspendedAudioContext(t *testing.T) {
	t.Parallel()
	js := readWebFile(t, "dictation.js")
	if !strings.Contains(js, "ctx.resume()") {
		t.Error("dictation.js should resume a suspended AudioContext for iOS Safari")
	}
	if !strings.Contains(js, "navigator.mediaDevices") {
		t.Error("dictation.js should guard navigator.mediaDevices availability")
	}
}

// TestGrimmMobileLayoutAndKeyboardHandling verifies the mobile-specific layout improvements:
// compact single-row topbar and session bar with expandable details toggle, hidden desktop hint on
// mobile/coarse-pointer viewports, visualViewport keyboard handling, and touch-friendly controls.
func TestGrimmMobileLayoutAndKeyboardHandling(t *testing.T) {
	t.Parallel()
	html := readWebFile(t, "grimm.html")
	css := readWebFile(t, "grimm.css")
	js := readWebFile(t, "grimm.js")

	if !strings.Contains(html, `id="g-bar-toggle"`) {
		t.Error("grimm.html misses mobile header details toggle #g-bar-toggle")
	}
	for _, want := range []string{
		`var(--g-vh, 100dvh)`,
		`#g-hint { display: none; }`,
		`.g-bar-toggle`,
		`.g-bar.is-expanded`,
		`.grimm-body.g-kbd`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("grimm.css misses mobile rule %q", want)
		}
	}
	if strings.Contains(css, "max-height: 540px") {
		t.Error("grimm.css must not unconditionally hide nav/tabs via max-height media query when keyboard is closed")
	}
	for _, want := range []string{
		`visualViewport`,
		`--g-vh`,
		`g-kbd`,
		`focused &&`,
		`g-bar-toggle`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("grimm.js misses mobile viewport/toggle handling %q", want)
		}
	}
}

const dictationWithoutMicHarness = `
const assert = require("assert"), fs = require("fs");
// Verify that grimm.js parses cleanly with Node and that dictation.js guards missing mediaDevices.
const dictationSrc = fs.readFileSync(process.argv[1], "utf8");
const grimmSrc = fs.readFileSync(process.argv[2], "utf8");
new Function(dictationSrc);
new Function(grimmSrc);

function makeEl() {
  const listeners = {};
  const el = {
    value: "", selectionStart: 0, selectionEnd: 0, hidden: false, readOnly: false, textContent: "", title: "",
    classList: { toggle() {}, remove() {}, add() {}, contains: () => false },
    firstChild: { textContent: "" },
    children: {},
    after() {},
    focus() {},
    setSelectionRange() {},
    dispatchEvent() {},
    addEventListener(k, fn) { (listeners[k] ||= []).push(fn); },
    fire(k, ev) { return Promise.all((listeners[k] || []).map((fn) => fn(ev || {}))); },
    querySelector(sel) { return el.children[sel] || (el.children[sel] = makeEl()); },
  };
  return el;
}
globalThis.window = globalThis;
globalThis.document = { createElement: () => makeEl(), addEventListener() {} };
globalThis.fetch = () => new Promise(() => {});
globalThis.navigator = {}; // no mediaDevices (e.g. insecure context)
eval(dictationSrc);

(async () => {
  let warned = "";
  const text = makeEl(), mic = makeEl(), row = makeEl();
  window.dictation({
    text, tokenPath: "/api/dictation/token", mic, row,
    label: () => "Send", send: () => {}, warn: (m) => { warned = m; },
  });
  await mic.fire("click");
  assert.ok(warned.toLowerCase().includes("microphone"), "expected clear microphone warning when mediaDevices is absent, got: " + warned);
  console.log("ok");
})().catch((e) => { console.error(e); process.exit(1); });
`

// TestGrimmDictationMissingMediaDevicesWarning: grimm.js and dictation.js parse, and where the
// browser has no navigator.mediaDevices (an insecure context), the mic warns instead of throwing.
func TestGrimmDictationMissingMediaDevicesWarning(t *testing.T) {
	t.Parallel()
	node := requireNode(t)
	out, err := exec.Command(node, "-e", dictationWithoutMicHarness, "dictation.js", "grimm.js").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("dictation/grimm JS check failed: %v\n%s", err, out)
	}
}

// TestGrimmMobileDictationBrowser serves a seeded Grimm session with dictation enabled and runs
// the Playwright script at GRIMM_MOBILE_SCRIPT against it. Skipped unless set.
func TestGrimmMobileDictationBrowser(t *testing.T) {
	t.Parallel()
	script := os.Getenv("GRIMM_MOBILE_SCRIPT")
	if script == "" {
		t.Skip("GRIMM_MOBILE_SCRIPT not set")
	}
	d := dbtest.New(t)
	aai := fakeAssemblyAI(t, "demo-key", "demo-tok")
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.AssemblyAIKey = "demo-key"; c.AssemblyAITokenURL = aai.URL })
	id, stream := readySession(t, h)
	if rec := call(t, h, "POST", "/api/grimm/sessions/"+id+"/messages", `{"text":"Build a CSV to Markdown tool with tests"}`, sameOriginFetch); rec.Code != 202 {
		t.Fatalf("message: %d %s", rec.Code, rec.Body)
	}
	mgr := stream.command()
	succeed(t, stream, mgr, `{"agentId":"mgr-1"}`)
	rep := wfReport()
	rep.Paseo.Providers = []hostclient.Provider{{Name: "antigravity", Available: true, Models: []hostclient.Model{{ID: "gemini-4-neon-medium"}}}}
	rep.Paseo.Agents = append(rep.Paseo.Agents,
		hostclient.Agent{ID: "mgr-1", WorkspaceID: "w-app", Provider: "antigravity", Status: "idle", CreatedAt: time.Now()},
		hostclient.Agent{ID: "w-1", WorkspaceID: "w-app", Provider: "antigravity", Status: "running", CreatedAt: time.Now()})
	if ack := stream.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	now := time.Now()
	batch := hostclient.TranscriptBatch{Host: "host-1", AgentID: "mgr-1", Epoch: "e1", Replace: true, Entries: []hostclient.TranscriptEntry{
		{SeqStart: 1, SeqEnd: 1, At: now, Item: json.RawMessage(`{"type":"user_message","text":"Build a CSV to Markdown tool with tests"}`)},
		{SeqStart: 2, SeqEnd: 2, At: now.Add(time.Second), Item: json.RawMessage(`{"type":"tool_call","name":"Bash","status":"completed","detail":{"type":"shell","command":"intelligence grimm ctl hire --role builder --name athena","output":"hired athena (builder) on antigravity/gemini-4-neon-medium"}}`)},
		{SeqStart: 3, SeqEnd: 3, At: now.Add(2 * time.Second), Item: json.RawMessage(`{"type":"assistant_message","text":"I hired **athena** to build the CSV-to-Markdown CLI.\n\nShe is implementing:\n- Streaming CSV parser with quoted fields\n- Aligned Markdown table formatter\n- Unit tests covering edge cases\n\nI will ping you as soon as the test suite passes."}`)},
	}}
	if ack := stream.call(hostclient.Frame{Type: hostclient.TranscriptFrame, Transcript: &batch}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	srv := serve(t, h)
	c := h.cookie
	out := os.Getenv("GRIMM_MOBILE_OUT")
	if out == "" {
		out = t.TempDir()
	}
	py := os.Getenv("GRIMM_MOBILE_PYTHON")
	if py == "" {
		py = "python3"
	}
	cmd := exec.CommandContext(t.Context(), py, script, srv.URL+"/grimm/"+id, c.Name, c.Value, out)
	cmd.Env = os.Environ()
	b, err := cmd.CombinedOutput()
	fmt.Println(string(b))
	if err != nil {
		t.Fatalf("browser check failed: %v", err)
	}
}
