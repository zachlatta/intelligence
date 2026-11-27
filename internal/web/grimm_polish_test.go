package web

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/grimm"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

func readWebFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// within reports whether after appears within n bytes after the first before.
func within(s, before, after string, n int) bool {
	i := strings.Index(s, before)
	if i < 0 {
		return false
	}
	rest := s[i:]
	if len(rest) > n {
		rest = rest[:n]
	}
	return strings.Contains(rest, after)
}

// The header's worker cap is edited inline (a stepper or popover in the page), never with the
// browser's prompt() dialog.
func TestGrimmCapIsEditedInlineNotWithPrompt(t *testing.T) {
	t.Parallel()
	js := readWebFile(t, "grimm.js")
	// picker.prompt(...) is the attachment helper; a bare prompt( or window.prompt( is the dialog
	if m := regexp.MustCompile(`(^|[^.\w])(window\.)?prompt\s*\(`).FindString(js); m != "" {
		t.Errorf("grimm.js still opens a prompt() dialog: %q", m)
	}

	h, _ := dbServer(t)
	id, _ := readySession(t, h)
	page := get(t, h, "/grimm/"+id, nil).Body.String()
	limit := strconv.Itoa(grimm.MaxWorkersLimit)
	// a number field bounded 1..limit, in the page itself (it may start hidden in a popover)
	field := regexp.MustCompile(`<input[^>]*type="number"[^>]*>`).FindAllString(page, -1)
	ok := false
	for _, f := range field {
		if strings.Contains(f, `min="1"`) && strings.Contains(f, `max="`+limit+`"`) && strings.Contains(f, "cap") {
			ok = true
		}
	}
	if !ok {
		t.Errorf("the session page has no inline cap field (input type=number, min=1, max=%s, an id or label naming the cap); number inputs: %q", limit, field)
	}
}

// Saving a cap goes through PUT /api/grimm/sessions/{id}; the header's numbers update from the
// stream without a reload, and out-of-range values are refused with a message the page can show.
func TestGrimmCapSavesAndStreamsAndRefusesOutOfRange(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	id, _ := readySession(t, h)
	events, cancel := openSSE(t, h, "/api/grimm/sessions/"+id+"/stream")
	defer cancel()
	waitFor := func(what string, ok func(ev, data string) bool) {
		t.Helper()
		timeout := time.After(20 * time.Second)
		for {
			select {
			case e, open := <-events:
				if !open {
					t.Fatalf("stream ended waiting for %s", what)
				}
				if ok(e[0], e[1]) {
					return
				}
			case <-timeout:
				t.Fatalf("no %s", what)
			}
		}
	}
	waitFor("the first state", func(ev, data string) bool { return ev == "state" && strings.Contains(data, `"max_workers":20`) })

	for _, bad := range []string{"0", "-1", strconv.Itoa(grimm.MaxWorkersLimit + 1)} {
		rec := call(t, h, "PUT", "/api/grimm/sessions/"+id, `{"maxWorkers":`+bad+`}`, sameOriginFetch)
		var body struct{ Error string }
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != 400 || !strings.Contains(body.Error, "1 to "+strconv.Itoa(grimm.MaxWorkersLimit)) {
			t.Errorf("cap %s: %d %s, want 400 naming the range", bad, rec.Code, rec.Body)
		}
	}
	if rec := call(t, h, "PUT", "/api/grimm/sessions/"+id, `{"maxWorkers":3}`, sameOriginFetch); rec.Code/100 != 2 {
		t.Fatalf("cap 3: %d %s", rec.Code, rec.Body)
	}
	waitFor("the new cap, pushed", func(ev, data string) bool {
		return ev == "state" && strings.Contains(data, `"max_workers":3`) && strings.Contains(data, `"live_workers":1`)
	})
}

// Static checks of crobat polish items that read plainly in the source. Item ids match
// .factory-scratch/grimm-ux-inventory-quinn.md; the browser check covers the rest.
func TestGrimmPolishFromCrobat(t *testing.T) {
	t.Parallel()
	js, css, html := readWebFile(t, "grimm.js"), readWebFile(t, "grimm.css"), readWebFile(t, "grimm.html")
	att := readWebFile(t, "attachments.js")
	all := js + css + html + att
	cases := []struct {
		id, what string
		ok       bool
	}{
		{"T1", "thread items animate in (a keyframes animation on new thread items)", regexp.MustCompile(`(?s)#g-items\s*>[^{]*\{[^}]*animation`).MatchString(css)},
		{"T3", "the empty state shows the Grimm mark", regexp.MustCompile(`(?s)id="g-empty".{0,300}(<img|<svg|g-empty-mark)`).MatchString(html)},
		{"T7", `a tool call with unknown status is not shown as done`, regexp.MustCompile(`(?s)function statusHTML.{0,400}unknown`).MatchString(js)},
		{"T8", "the update marker is a pill between rules", regexp.MustCompile(`(?s)\.g-update[^{]*::?before`).MatchString(css)},
		{"T9", "notices are a boxed card (border)", regexp.MustCompile(`(?s)\.g-notice\s*\{[^}]*border`).MatchString(css)},
		{"C1", "the attach control is a paperclip icon button", strings.Contains(all, "<svg") && regexp.MustCompile(`(?i)aria-label[^\n]{0,40}attach`).MatchString(all)},
		{"C3", `a live "N image(s) ready" count`, regexp.MustCompile(`(?i)ready\s*·|images?\$?\{?[^\n]{0,30}ready`).MatchString(all)},
		{"C4", "the hint says images can be dropped or pasted", regexp.MustCompile(`(?i)class="g-hint"[^<]*(drop|paste)`).MatchString(html)},
		{"C5", "upload progress (Uploading image i of n)", regexp.MustCompile(`(?i)uploading`).MatchString(js + att)},
		{"C6", "the textarea is read-only while sending", regexp.MustCompile(`readOnly\s*=`).MatchString(js + att)},
		{"C8", "the composer refocuses after sending", within(js, "async function send(", ".focus(", 2000)},
		{"H3", "the branch shows with a ⎇ glyph", strings.Contains(css+js, "⎇")},
		{"H5", "Archive all says it is archiving before the request returns", regexp.MustCompile(`(?s)g-archive-all.{0,600}Archiving`).MatchString(js)},
		{"W1", "status pills have a leading dot", regexp.MustCompile(`\.g-status::?before`).MatchString(css)},
		{"W2", "archived worker cards are dimmed", regexp.MustCompile(`(?s)\.g-archived[^{]*\.g-card[^{]*\{[^}]*opacity`).MatchString(css)},
		{"W4", "the focus view slides in", regexp.MustCompile(`(?s)\.g-focus\s*\{[^}]*animation`).MatchString(css)},
		{"W5", "archiving one worker shows Archiving <name>… first", regexp.MustCompile("(?s)g-focus-archive.{0,400}Archiving \\$\\{").MatchString(js)},
		{"W6", "bus messages get a left border coloured by kind", regexp.MustCompile(`(?s)\.g-bus\s+li\.k-result[^{]*\{[^}]*border-left`).MatchString(css) || regexp.MustCompile(`(?s)\.g-bus\s+\.k-result\s*\{[^}]*border-left`).MatchString(css)},
		{"Q2", "an ineligible plan's reason is in the warning colour", regexp.MustCompile(`(?s)\.g-q-foot \.why\s*\{[^}]*color`).MatchString(css)},
	}
	for _, c := range cases {
		if !c.ok {
			t.Errorf("%s missing: %s", c.id, c.what)
		}
	}
}

// TestGrimmPolishBrowser serves a seeded session and runs the headless-browser check at
// GRIMM_BROWSER_SCRIPT (node, Playwright via NODE_PATH) against it. Skipped unless set.
func TestGrimmPolishBrowser(t *testing.T) {
	t.Parallel()
	script := os.Getenv("GRIMM_BROWSER_SCRIPT")
	if script == "" {
		t.Skip("GRIMM_BROWSER_SCRIPT not set")
	}
	h, d := dbServer(t)
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
		{SeqStart: 1, SeqEnd: 1, At: now, Item: json.RawMessage(`{"type":"user_message","text":"first prompt"}`)},
		{SeqStart: 2, SeqEnd: 2, At: now.Add(time.Second), Item: json.RawMessage(`{"type":"tool_call","name":"Bash","status":"completed","detail":{"type":"shell","command":"intelligence grimm ctl hire --role builder --name athena","output":"hired athena (builder) on antigravity/gemini-4-neon-medium"}}`)},
		{SeqStart: 3, SeqEnd: 3, At: now.Add(2 * time.Second), Item: json.RawMessage(`{"type":"tool_call","name":"Bash","status":"unknown","detail":{"type":"shell","command":"go test ./..."}}`)},
		{SeqStart: 4, SeqEnd: 4, At: now.Add(3 * time.Second), Item: json.RawMessage(`{"type":"assistant_message","text":"I hired **athena** to build it. She is writing ` + "`csv2md`" + ` now.\n\n- parser\n- CLI"}`)}}}
	if ack := stream.call(hostclient.Frame{Type: hostclient.TranscriptFrame, Transcript: &batch}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	var acct int64
	if err := d.Owner.QueryRow(t.Context(), `SELECT account_id FROM grimm_sessions WHERE id = $1`, id).Scan(&acct); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO grimm_messages (session_id, account_id, channel, sender, recipient, kind, body, data, created_at) VALUES ($1,$2,'thread','system','','update','athena finished','{"summaries":["athena finished"]}', now()+interval '5 seconds')`,
		`INSERT INTO grimm_messages (session_id, account_id, channel, sender, recipient, kind, body, data, created_at) VALUES ($1,$2,'thread','system','','notice','The host went offline for a moment.','{"level":"error"}', now()+interval '6 seconds')`,
		`INSERT INTO grimm_messages (session_id, account_id, channel, sender, recipient, kind, body) VALUES ($1,$2,'bus','athena','grimm','result','Done: csv2md works, 12 tests pass.')`,
		`INSERT INTO grimm_messages (session_id, account_id, channel, sender, recipient, kind, body) VALUES ($1,$2,'bus','athena','all','handoff','Parser is done; take the CLI.')`,
		`INSERT INTO grimm_messages (session_id, account_id, channel, sender, recipient, kind, body) VALUES ($1,$2,'bus','grimm','athena','system','Hired athena (builder).')`,
	} {
		if _, err := d.Owner.Exec(t.Context(), q, id, acct); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if rec := call(t, h, "POST", "/api/grimm/sessions/"+id+"/directives", `{"text":"Never push to main."}`, sameOriginFetch); rec.Code/100 != 2 {
		t.Fatalf("directive: %d %s", rec.Code, rec.Body)
	}
	srv := serve(t, h)
	c := h.(*testServer).cookie
	out := os.Getenv("GRIMM_BROWSER_OUT")
	if out == "" {
		out = t.TempDir()
	}
	cmd := exec.CommandContext(t.Context(), "node", script, srv.URL+"/grimm/"+id, c.Name, c.Value, out, strconv.Itoa(grimm.MaxWorkersLimit))
	cmd.Env = os.Environ()
	b, err := cmd.CombinedOutput()
	fmt.Println(string(b))
	if err != nil {
		t.Fatalf("browser check failed: %v", err)
	}
}
