package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

func fe(seq int, item string) fleet.Entry {
	var k struct{ Type, Name, Status, Text string }
	json.Unmarshal([]byte(item), &k)
	e := fleet.Entry{TranscriptEntry: hostclient.TranscriptEntry{SeqStart: seq, SeqEnd: seq, At: t0, Item: json.RawMessage(item)},
		Kind: k.Type, Text: k.Text}
	if k.Type == "tool_call" {
		e.Tool, e.Status, e.Text = k.Name, k.Status, ""
	}
	return e
}

func shell(seq int, cmd, out string, exit int) fleet.Entry {
	b, _ := json.Marshal(map[string]any{"type": "tool_call", "callId": fmt.Sprint(seq), "name": "Bash", "status": "completed",
		"detail": map[string]any{"type": "shell", "command": cmd, "output": out, "exitCode": exit}, "error": nil})
	return fe(seq, string(b))
}

func TestBlocksGroupToolRunsAndStartTurnsAtUserMessages(t *testing.T) {
	bs := buildBlocks([]fleet.Entry{
		fe(1, `{"type":"user_message","text":"fix it"}`),
		fe(2, `{"type":"reasoning","text":"hmm"}`),
		shell(3, "ls", "a", 0),
		shell(4, "cat a", "b", 0),
		fe(5, `{"type":"assistant_message","text":"done"}`),
		shell(6, "git push", "", 0),
		fe(7, `{"type":"user_message","text":"thanks"}`),
	})
	var got []string
	for _, b := range bs {
		got = append(got, fmt.Sprintf("%s:%d:%d", b.Kind, b.Seq, len(b.Entries)))
	}
	want := "user:1:1 reasoning:2:1 tools:3:2 assistant:5:1 tools:6:1 user:7:1"
	if strings.Join(got, " ") != want {
		t.Errorf("blocks = %s\nwant     %s", strings.Join(got, " "), want)
	}
	if !bs[0].TurnStart || bs[1].TurnStart || !bs[5].TurnStart {
		t.Errorf("each user message starts a turn: %+v", bs)
	}
}

func TestMarkdownRendersButStaysSafe(t *testing.T) {
	html := string(markdown("**bold** `code`\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n<script>alert(1)</script>\n\n[x](javascript:alert(1)) [ok](https://example.com)"))
	for _, want := range []string{"<strong>bold</strong>", "<code>code</code>", "<table>", `href="https://example.com"`} {
		if !strings.Contains(html, want) {
			t.Errorf("markdown misses %q in %s", want, html)
		}
	}
	if !strings.Contains(html, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("raw HTML should show as text, not vanish: %s", html)
	}
	for _, bad := range []string{"<script", `href="javascript:`} {
		if strings.Contains(html, bad) {
			t.Errorf("markdown let %q through: %s", bad, html)
		}
	}
}

func TestToolSummariesByKind(t *testing.T) {
	cases := []struct {
		item string
		want []string
	}{
		{`{"type":"tool_call","name":"Bash","status":"completed","detail":{"type":"shell","command":"go test ./...","cwd":"/w","output":"ok","exitCode":0},"error":null}`,
			[]string{"go test ./...", "ok"}},
		{`{"type":"tool_call","name":"Bash","status":"completed","detail":{"type":"shell","command":"false","output":"boom","exitCode":1},"error":null}`,
			[]string{"exit 1", "boom"}},
		{`{"type":"tool_call","name":"Edit","status":"completed","detail":{"type":"edit","filePath":"/w/a.go","unifiedDiff":"@@ -1 +1 @@\n-old\n+new"},"error":null}`,
			[]string{"/w/a.go", `class="add">&#43;new`, `class="del">-old`}},
		{`{"type":"tool_call","name":"Edit","status":"completed","detail":{"type":"edit","filePath":"/w/b.go","oldString":"x := 1","newString":"x := 2"},"error":null}`,
			[]string{"/w/b.go", `class="del">-x := 1`, `class="add">&#43;x := 2`}},
		{`{"type":"tool_call","name":"Read","status":"completed","detail":{"type":"read","filePath":"/w/README.md","content":"hello","offset":10,"limit":20},"error":null}`,
			[]string{"/w/README.md", "lines 10–29", "hello"}},
		{`{"type":"tool_call","name":"Write","status":"completed","detail":{"type":"write","filePath":"/w/new.go","content":"package x\n"},"error":null}`,
			[]string{"/w/new.go", "package x"}},
		{`{"type":"tool_call","name":"Grep","status":"completed","detail":{"type":"search","query":"TODO","toolName":"grep","numFiles":3,"numMatches":7},"error":null}`,
			[]string{"TODO", "7 matches in 3 files"}},
		{`{"type":"tool_call","name":"WebFetch","status":"completed","detail":{"type":"fetch","url":"https://example.com/x","code":200},"error":null}`,
			[]string{"https://example.com/x", "200"}},
		{`{"type":"tool_call","name":"Task","status":"completed","detail":{"type":"sub_agent","subAgentType":"Explore","description":"find the parser"},"error":null}`,
			[]string{"Explore", "find the parser"}},
		{`{"type":"tool_call","name":"task_notification","status":"completed","detail":{"type":"plain_text","label":"Background command finished","text":"done"},"error":null}`,
			[]string{"Background command finished"}},
		{`{"type":"tool_call","name":"ExitPlanMode","status":"completed","detail":{"type":"plan","text":"1. **do it**"},"error":null}`,
			[]string{"<strong>do it</strong>"}},
		{`{"type":"tool_call","name":"Monitor","status":"failed","detail":{"type":"unknown","input":{"command":"watch <x>"},"output":null},"error":{"message":"timed out"}}`,
			[]string{"Monitor", "failed", "watch &lt;x&gt;", "timed out"}},
	}
	for _, c := range cases {
		page := renderBlocksForTest(t, []fleet.Entry{fe(1, c.item)})
		for _, w := range c.want {
			if !strings.Contains(page, w) {
				t.Errorf("tool %s: page misses %q\n%s", c.item[:60], w, page)
			}
		}
	}
}

func TestLongOutputIsCutWithALinkToTheWhole(t *testing.T) {
	out := strings.Repeat("line\n", 500)
	page := renderBlocksForTest(t, []fleet.Entry{shell(9, "seq 500", out, 0)})
	if strings.Count(page, "line\n") > 100 {
		t.Errorf("500 lines of output were not cut")
	}
	if !strings.Contains(page, "/entries/9") || !strings.Contains(page, "more lines") {
		t.Errorf("a cut output should say so and link to the whole entry")
	}
}

func renderBlocksForTest(t *testing.T, entries []fleet.Entry) string {
	t.Helper()
	var b strings.Builder
	if err := blocksTmpl.ExecuteTemplate(&b, "blocks", blocksData{Base: "/hosts/host-1/agents/a1", Blocks: buildBlocks(entries), Now: t0}); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestTranscriptLiveUpdateSendsBlocksFromASeq(t *testing.T) {
	h := reported(t)
	sendBatch(t, h, batch(true, "ep1",
		tentry(1, `{"type":"user_message","text":"go"}`),
		tentry(2, `{"type":"tool_call","callId":"c","name":"Bash","status":"completed","detail":{"type":"shell","command":"ls","output":"a"},"error":null}`),
		tentry(3, `{"type":"assistant_message","text":"half"}`)))
	base := "/hosts/host-1/agents/" + agentA1
	rec := get(t, h, base+"/blocks?from=3", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "half") || strings.Contains(body, ">go<") || strings.Contains(body, "ls") {
		t.Errorf("from=3 should send only the block starting at seq 3: %s", body)
	}
	if rec.Header().Get("X-Epoch") != "ep1" || rec.Header().Get("X-Agent-Status") != "running" {
		t.Errorf("headers = %v", rec.Header())
	}
	// the streamed message grew: the same block comes back whole
	sendBatch(t, h, batch(false, "ep1", tentry(3, `{"type":"assistant_message","text":"half and whole"}`)))
	if body := get(t, h, base+"/blocks?from=3", nil).Body.String(); !strings.Contains(body, "half and whole") {
		t.Errorf("grown block not sent: %s", body)
	}
}

func TestTranscriptEntryShowsTheWholeItem(t *testing.T) {
	h := reported(t)
	sendBatch(t, h, batch(true, "ep1",
		tentry(1, `{"type":"tool_call","callId":"c","name":"Bash","status":"completed","detail":{"type":"shell","command":"cat <big>","output":"`+strings.Repeat("y", 9000)+`"},"error":null}`)))
	rec := get(t, h, "/hosts/host-1/agents/"+agentA1+"/entries/1", nil)
	if rec.Code != http.StatusOK || strings.Count(rec.Body.String(), "y") < 9000 || !strings.Contains(rec.Body.String(), "cat &lt;big&gt;") {
		t.Fatalf("entry page: %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if rec := get(t, h, "/hosts/host-1/agents/"+agentA1+"/entries/99", nil); rec.Code != http.StatusNotFound {
		t.Errorf("missing entry: %d", rec.Code)
	}
}

func TestTranscriptPageHasTheToolbarAndOtherAgents(t *testing.T) {
	h := reported(t)
	sendBatch(t, h, batch(true, "ep1", tentry(1, `{"type":"user_message","text":"go"}`)))
	page := get(t, h, "/hosts/host-1/agents/"+agentA1, nil).Body.String()
	for _, want := range []string{`id="search"`, `data-toggle="reasoning"`, `data-toggle="tools"`,
		"Audit",                               // the other agent in the same workspace
		`href="/hosts/host-1/agents/a3000000`, // links to it
		"claude-opus-5-5", "1 entry"} {
		if !strings.Contains(page, want) {
			t.Errorf("transcript page misses %q", want)
		}
	}
}

func TestMarkdownImagesBecomePlaceholders(t *testing.T) {
	html := string(markdown("before ![Screenshot of the grid](/tmp/shots/a.png) after ![](https://example.com/b.png)"))
	if strings.Contains(html, "<img") {
		t.Errorf("images point at files on the host the page cannot load; want a placeholder: %s", html)
	}
	for _, want := range []string{"Screenshot of the grid", "/tmp/shots/a.png", `href="https://example.com/b.png"`} {
		if !strings.Contains(html, want) {
			t.Errorf("placeholder misses %q: %s", want, html)
		}
	}
}

func TestSharedImagesRenderInlineWithoutAllowingOtherOrigins(t *testing.T) {
	url := "https://files.zachlatta.com/unknown-namespace/screenshot.png"
	html := string(markdown(`![Grid & chart](` + url + `)`))
	for _, want := range []string{`<img`, `src="` + url + `"`, `alt="Grid &amp; chart"`, `loading="lazy"`, `referrerpolicy="no-referrer"`} {
		if !strings.Contains(html, want) {
			t.Errorf("shared image misses %q: %s", want, html)
		}
	}
	for _, dest := range []string{
		"/home/someone/shot.png", "file:///shot.png", "javascript:alert(1)",
		"https://files.zachlatta.com.evil.example/shot.png",
		"https://files.zachlatta.com@example.com/shot.png",
		"http://files.zachlatta.com/shot.png", "//files.zachlatta.com/shot.png",
	} {
		if got := string(markdown("![shot](" + dest + ")")); strings.Contains(got, "<img") {
			t.Errorf("untrusted image loaded: %s", got)
		}
	}
}

func TestToolLabelsAreFriendly(t *testing.T) {
	page := renderBlocksForTest(t, []fleet.Entry{fe(1,
		`{"type":"tool_call","name":"task_notification","status":"completed","detail":{"type":"plain_text","label":"Done","text":"x"},"error":null}`)})
	if strings.Contains(page, "plain_text") || !strings.Contains(page, ">note<") {
		t.Errorf("tool type labels should read as words: %s", page)
	}
}
