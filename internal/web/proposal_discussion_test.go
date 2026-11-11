package web

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestProposalDiscussion(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	headers := map[string]string{"Authorization": "Bearer " + keyOf(t, h, "host-1")}
	r := call(t, anon(h), "POST", "/api/workflow-proposals", `{"operation":"create","summary":"**Safer workflow**\n\nWhy: <script>alert(1)</script>","workflow":`+wfBody(t, "New", workflow.Starting)+`}`, headers)
	if r.Code != 201 {
		t.Fatal(r.Body)
	}
	var p struct{ ID string }
	json.Unmarshal(r.Body.Bytes(), &p)
	path := "/api/workflow-proposals/" + p.ID + "/comments"
	r = call(t, h, "POST", path, `{"text":"Why this change?"}`, sameOriginFetch)
	if r.Code != 201 {
		t.Fatalf("comment: %d %s", r.Code, r.Body)
	}
	r = call(t, anon(h), "POST", path+"/agent", `{"text":"It **reduces risk**."}`, headers)
	if r.Code != 201 {
		t.Fatalf("agent comment: %d %s", r.Code, r.Body)
	}
	for _, body := range []string{`{"text":" "}`, `{"text":"fake","author":"agent"}`} {
		if got := call(t, h, "POST", path, body, sameOriginFetch); got.Code != 400 {
			t.Fatalf("bad comment: %d", got.Code)
		}
	}
	if got := call(t, anon(h), "POST", path, `{"text":"anon"}`, sameOriginFetch); got.Code != 401 {
		t.Fatalf("anonymous: %d", got.Code)
	}
	if got := call(t, h, "POST", path, `{"text":"cross site"}`, map[string]string{"Sec-Fetch-Site": "cross-site"}); got.Code != 403 {
		t.Fatalf("cross-site: %d", got.Code)
	}
	long, _ := json.Marshal(map[string]string{"text": strings.Repeat("a", 20001)})
	if got := call(t, h, "POST", path, string(long), sameOriginFetch); got.Code != 400 {
		t.Fatalf("oversize: %d", got.Code)
	}
	store := h.(*testServer).store
	other, _ := store.Create(t.Context(), "other-commenter", testPassword)
	cookie := sessionFor(t, store, other)
	if got := call(t, h, "POST", path, `{"text":"other"}`, map[string]string{"Cookie": cookie.String(), "Content-Type": "application/json", "Origin": "http://example.com"}); got.Code != 404 {
		t.Fatalf("other account: %d %s", got.Code, got.Body)
	}
	if got := call(t, h, "POST", "/api/sql", q("SELECT * FROM workflow_proposal_comments"), sameOriginFetch, cookie); got.Code != 200 || !strings.Contains(got.Body.String(), `"rowCount":0`) {
		t.Fatalf("thread isolation: %d %s", got.Code, got.Body)
	}
	code, res := sqlPost(t, h, q("SELECT author,text FROM workflow_proposal_comments ORDER BY id"))
	if code != 200 || len(res.Rows) != 2 || res.Rows[0][0] != "reviewer" || res.Rows[1][0] != "host:host-1" {
		t.Fatalf("SQL: %d %+v", code, res)
	}
	page := get(t, h, "/workflow-proposals/"+p.ID, nil).Body.String()
	for _, want := range []string{"<strong>Safer workflow</strong>", "Why this change?", "<strong>reduces risk</strong>", "does not automatically wake", `id="proposal-comment"`, `class="review-inline"`} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Fatal("unsafe markdown")
	}
	if strings.Index(page, "Approve change") > strings.Index(page, "Changed fields") {
		t.Fatal("actions buried after diff")
	}
	_, token := mintMCP(t, h)
	got := rpc(t, h, token, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"comment_proposal","arguments":{"id":"`+p.ID+`","text":"An MCP answer"}}}`)
	if !strings.Contains(got, `"isError":false`) {
		t.Fatal(got)
	}
	code, res = sqlPost(t, h, q("SELECT text FROM workflow_proposal_comments WHERE author LIKE 'mcp:%'"))
	if code != 200 || len(res.Rows) != 1 || res.Rows[0][0] != "An MCP answer" {
		t.Fatalf("MCP comment not readable: %+v", res)
	}
}

func TestProposalSummaryFallback(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	r := call(t, anon(h), "POST", "/api/workflow-proposals", `{"operation":"create","summary":"Legacy","workflow":`+wfBody(t, "New", workflow.Starting)+`}`, map[string]string{"Authorization": "Bearer " + keyOf(t, h, "host-1")})
	if r.Code != 201 {
		t.Fatal(r.Body)
	}
	var p struct{ ID string }
	json.Unmarshal(r.Body.Bytes(), &p)
	if _, err := d.Owner.Exec(t.Context(), "UPDATE workflow_proposals SET summary='' WHERE id=$1", p.ID); err != nil {
		t.Fatal(err)
	}
	page := get(t, h, "/workflow-proposals/"+p.ID, nil).Body.String()
	if !strings.Contains(page, "Proposed create to New:") || !strings.Contains(page, "<li>Workflow name</li>") {
		t.Fatal("missing legacy fallback")
	}
}

func TestProposalInlineDiff(t *testing.T) {
	got := string(proposalInlineDiff("Keep the text.", "Keep the new text."))
	if !strings.Contains(got, "<ins>new </ins>") || strings.Contains(got, "<del>Keep") {
		t.Fatal(got)
	}
	got = string(proposalInlineDiff("<script>old</script>", "<script>new</script>"))
	if strings.Contains(got, "<script>") {
		t.Fatal(got)
	}
	if got := string(proposalInlineDiff("remove", "")); !strings.Contains(got, "<del>remove</del>") {
		t.Fatal(got)
	}
}
