package web

import (
	"encoding/json"
	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/workflow"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestRunProposals(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	wf := createWorkflow(t, h, workflow.Starting)
	_, token := mintMCP(t, h)
	input := "Investigate the login page\n<script>untrusted()</script>\nKeep the full task."
	propose := func(replaces string, host bool) string {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"operation": "run", "workflowId": wf, "input": input, "summary": "Run the investigation", "proposedBy": "test agent", "replacesId": replaces})
		var id string
		if host {
			r := call(t, anon(h), "POST", "/api/workflow-proposals", string(b), map[string]string{"Authorization": "Bearer " + keyOf(t, h, "host-1")})
			var p struct{ ID, ApprovalURL string }
			json.Unmarshal(r.Body.Bytes(), &p)
			if r.Code != 201 || !strings.HasSuffix(p.ApprovalURL, "/workflow-proposals/"+p.ID) {
				t.Fatalf("host proposal: %d %s", r.Code, r.Body)
			}
			id = p.ID
		} else {
			got := rpc(t, h, token, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"propose_mutation","arguments":`+string(b)+`}}`)
			var msg struct {
				Result struct {
					StructuredContent struct{ ID, ApprovalURL string }
				}
			}
			json.Unmarshal([]byte(got), &msg)
			p := msg.Result.StructuredContent
			if p.ID == "" || !strings.HasSuffix(p.ApprovalURL, "/workflow-proposals/"+p.ID) {
				t.Fatal(got)
			}
			id = p.ID
		}
		return id
	}
	decide := func(id, action string, want int) {
		t.Helper()
		r := call(t, h, "POST", "/api/workflow-proposals/"+id+"/"+action, `{}`, sameOriginFetch)
		if r.Code != want {
			t.Fatalf("%s: %d %s", action, r.Code, r.Body)
		}
	}
	count := func(want int) {
		t.Helper()
		var n int
		if err := d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM workflow_runs`).Scan(&n); err != nil || n != want {
			t.Fatalf("runs: %d want %d: %v", n, want, err)
		}
	}
	id := propose("", false)
	count(0)
	var commands int
	if err := d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM commands`).Scan(&commands); err != nil || commands != 0 {
		t.Fatalf("proposal queued commands: %d %v", commands, err)
	}
	page := get(t, h, "/workflow-proposals/"+id, nil)
	for _, term := range []string{"Approve run", "Full task input", "&lt;script&gt;untrusted()&lt;/script&gt;", "Keep the full task."} {
		if !strings.Contains(page.Body.String(), term) {
			t.Errorf("review missing %s", term)
		}
	}
	if strings.Contains(page.Body.String(), "<script>untrusted()") {
		t.Fatal("input executed as HTML")
	}
	if !strings.Contains(get(t, h, "/workflow-proposals", nil).Body.String(), "Run the investigation") {
		t.Fatal("missing from list")
	}
	for _, action := range []string{"approve", "reject", "undo"} {
		r := call(t, anon(h), "POST", "/api/workflow-proposals/"+id+"/"+action, `{}`, map[string]string{"Authorization": "Bearer " + token})
		if r.Code != 401 {
			t.Fatalf("MCP can %s: %d", action, r.Code)
		}
	}
	ts := h.(*testServer)
	other, err := ts.store.Create(t.Context(), "otherproposer", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	cookie := sessionFor(t, ts.store, other)
	otherH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { r.AddCookie(cookie); anon(h).ServeHTTP(w, r) })
	_, otherToken := mintMCP(t, otherH)
	got := rpc(t, h, otherToken, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"propose_mutation","arguments":{"operation":"run","workflowId":"`+wf+`","input":"x","summary":"x"}}}`)
	if !strings.Contains(got, `"isError":true`) {
		t.Fatal(got)
	}
	if call(t, h, "POST", "/api/workflow-proposals/"+id+"/approve", `{}`, sameOriginFetch, cookie).Code != 404 {
		t.Fatal("cross-account approve")
	}
	if get(t, otherH, "/workflow-proposals/"+id, nil).Code != 404 {
		t.Fatal("cross-account read")
	}
	for _, args := range []string{
		`{"operation":"run","workflowId":"` + wf + `","summary":"x"}`,
		`{"operation":"run","workflowId":"` + wf + `","summary":"x","input":" "}`,
		`{"operation":"run","workflowId":"` + wf + `","summary":"x","input":"x","baseVersion":999}`,
		`{"operation":"run","workflowId":"` + wf + `","summary":"x","input":"x","workflow":{}}`,
	} {
		got := rpc(t, h, token, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"propose_mutation","arguments":`+args+`}}`)
		if !strings.Contains(got, `"isError":true`) {
			t.Fatalf("invalid run proposal: %s", got)
		}
	}
	if r := call(t, anon(h), "POST", "/api/workflow-runs", `{"workflowId":"`+wf+`","input":"x"}`, map[string]string{"Authorization": "Bearer " + token}); r.Code != 401 {
		t.Fatal("MCP token directly started run")
	}
	// Only the originating token may withdraw or replace.
	_, secondToken := mintMCP(t, h)
	withdraw := func(tok, p string) string {
		return rpc(t, h, tok, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"withdraw_mutation","arguments":{"id":"`+p+`","reason":"No longer needed"}}}`)
	}
	if !strings.Contains(withdraw(secondToken, id), `"isError":true`) {
		t.Fatal("another token withdrew")
	}
	replaced := propose(id, false)
	decide(id, "approve", 409)
	if strings.Contains(withdraw(token, replaced), `"isError":true`) {
		t.Fatal("withdraw failed")
	}
	decide(replaced, "approve", 409)
	rejected := propose("", true)
	decide(rejected, "reject", 200)
	decide(rejected, "approve", 409)
	expired := propose("", false)
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_proposals SET expires_at=now()-interval '1 hour' WHERE id=$1`, expired); err != nil {
		t.Fatal(err)
	}
	decide(expired, "approve", 409)
	count(0)
	// Concurrent browser approvals must commit one run and one launch command.
	approved := propose("", false)
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- call(t, h, "POST", "/api/workflow-proposals/"+approved+"/approve", `{}`, sameOriginFetch).Code
		}()
	}
	wg.Wait()
	close(codes)
	sum := 0
	for c := range codes {
		sum += c
	}
	if sum != 609 {
		t.Fatalf("approval codes: %d", sum)
	}
	count(1)
	decide(approved, "approve", 409)
	decide(approved, "undo", 409)
	code, res := sqlPost(t, h, q("SELECT p.started_run_id::text,r.input,r.version,p.status FROM workflow_proposals p JOIN workflow_runs r ON r.id=p.started_run_id WHERE p.id='"+approved+"'"))
	if code != 200 || len(res.Rows) != 1 || res.Rows[0][1] != input || res.Rows[0][2] != float64(1) || res.Rows[0][3] != "applied" {
		t.Fatalf("SQL audit: %d %+v", code, res)
	}
	page = get(t, h, "/workflow-proposals/"+approved, nil)
	if !strings.Contains(page.Body.String(), "View workflow run") || strings.Contains(page.Body.String(), `data-action="undo"`) {
		t.Fatal("applied run lacks link or offers undo")
	}
	// Discussion sees the full task as quoted data, without starting a workflow run.
	question := askProposal(t, h, approved, "What task would this run perform?")
	var prompt string
	if err := d.Owner.QueryRow(t.Context(), `SELECT c.args->>'prompt' FROM workflow_proposal_questions q JOIN commands c ON c.id=q.command_id WHERE q.question_id=$1`, question).Scan(&prompt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Keep the full task.") {
		t.Fatal("discussion omitted task")
	}
	stale := propose("", false)
	if r := call(t, h, "PUT", "/api/workflows/"+wf, wfBody(t, "Edited", workflow.Starting), sameOriginFetch); r.Code != 200 {
		t.Fatal(r.Body)
	}
	code, res = sqlPost(t, h, q("SELECT effective_status FROM workflow_proposal_status WHERE id='"+stale+"'"))
	if code != 200 || res.Rows[0][0] != "stale" {
		t.Fatalf("stale: %+v", res)
	}
	decide(stale, "approve", 409)
	deleted := propose("", false)
	if _, err := d.Owner.Exec(t.Context(), `UPDATE workflow_runs SET status='succeeded'; UPDATE workflows SET deleted_at=now()`); err != nil {
		t.Fatal(err)
	}
	decide(deleted, "approve", 409)
	count(1)
	if err := db.CheckContract(t.Context(), d.Owner, d.Readonly); err != nil {
		t.Fatal(err)
	}
}

func TestRunProposalToolSchema(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Type string
			Enum []string
		}
	}
	if err := json.Unmarshal(proposalSchema, &schema); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, op := range schema.Properties["operation"].Enum {
		if op == "run" {
			found = true
		}
	}
	if !found || schema.Properties["input"].Type != "string" {
		t.Fatal("run proposal not advertised")
	}
	for _, term := range []string{"operation=run", "started_run_id", "browser approval", "insert model group tokens", "{{frontier_model}}"} {
		if !strings.Contains(proposalHelp, term) {
			t.Errorf("help missing %s", term)
		}
	}
	for _, tool := range mcpTools() {
		if tool.Name == "submit_proposal" {
			t.Fatal("immediate-start tool must not exist")
		}
	}
}
