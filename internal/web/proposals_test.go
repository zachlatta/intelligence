package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestProposalReviewAndUndo(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	id := createWorkflow(t, h, workflow.Starting)
	body := `{"operation":"update","workflowId":"` + id + `","baseVersion":1,"summary":"Improve prompt","workflow":` + wfBody(t, "Changed", workflow.Starting) + `}`
	rec := call(t, anon(h), "POST", "/api/workflow-proposals", body, map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != 201 {
		t.Fatalf("propose: %d %s", rec.Code, rec.Body)
	}
	var p struct{ ID, ApprovalURL string }
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p.ApprovalURL, "/workflow-proposals/"+p.ID) {
		t.Fatalf("approval URL: %+v", p)
	}
	code, result := sqlPost(t, h, q("SELECT name FROM workflows WHERE id = '"+id+"'"))
	if code != 200 || result.Rows[0][0] == "Changed" {
		t.Fatal("proposal changed workflow")
	}
	for _, action := range []string{"approve", "reject", "undo"} {
		r := call(t, anon(h), "POST", "/api/workflow-proposals/"+p.ID+"/"+action, `{}`, map[string]string{"Authorization": "Bearer " + key})
		if r.Code != 401 {
			t.Fatalf("host can %s: %d", action, r.Code)
		}
	}
	page := get(t, h, "/workflow-proposals/"+p.ID, nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Changed") || !strings.Contains(page.Body.String(), "Before") {
		t.Fatalf("review: %d %s", page.Code, page.Body)
	}
	for _, action := range []string{"approve", "undo"} {
		r := call(t, h, "POST", "/api/workflow-proposals/"+p.ID+"/"+action, `{}`, sameOriginFetch)
		if r.Code != 200 {
			t.Fatalf("%s: %d %s", action, r.Code, r.Body)
		}
	}
	code, result = sqlPost(t, h, q("SELECT name, version FROM workflows WHERE id = '"+id+"'"))
	if code != 200 || result.Rows[0][0] != "Task from Intelligence" || result.Rows[0][1] != float64(3) {
		t.Fatalf("undo: %+v", result)
	}
	code, result = sqlPost(t, h, q("SELECT status, before_state IS NOT NULL, after_state IS NOT NULL, undone_at IS NOT NULL FROM workflow_proposals"))
	if code != 200 || len(result.Rows) != 1 || result.Rows[0][0] != "undone" || result.Rows[0][3] != true {
		t.Fatalf("audit: %+v", result)
	}
}

func TestProposalCreateDeleteExpiryAndIsolation(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	headers := map[string]string{"Authorization": "Bearer " + key}
	propose := func(body string) string {
		r := call(t, anon(h), "POST", "/api/workflow-proposals", body, headers)
		if r.Code != 201 {
			t.Fatalf("propose: %d %s", r.Code, r.Body)
		}
		var p struct{ ID string }
		json.Unmarshal(r.Body.Bytes(), &p)
		return p.ID
	}
	decide := func(id, action string, want int) {
		r := call(t, h, "POST", "/api/workflow-proposals/"+id+"/"+action, `{}`, sameOriginFetch)
		if r.Code != want {
			t.Fatalf("%s: %d %s", action, r.Code, r.Body)
		}
	}
	p := propose(`{"operation":"create","summary":"New workflow","workflow":` + wfBody(t, "New", workflow.Starting) + `}`)
	// A different account cannot read, decide or withdraw the proposal.
	ts := h.(*testServer)
	other, err := ts.store.Create(t.Context(), "other", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	cookie := sessionFor(t, ts.store, other)
	otherKey := joinHost(t, ts.store, other, "other-host")
	r := call(t, h, "GET", "/workflow-proposals/"+p, "", nil, cookie)
	if r.Code != 404 {
		t.Fatal("proposal leaked")
	}
	r = call(t, h, "POST", "/api/workflow-proposals/"+p+"/approve", `{}`, sameOriginFetch, cookie)
	if r.Code != 404 {
		t.Fatal("other account approved")
	}
	r = call(t, anon(h), "POST", "/api/workflow-proposals/"+p+"/withdraw", `{"reason":"No longer needed"}`, map[string]string{"Authorization": "Bearer " + otherKey})
	if r.Code != 404 {
		t.Fatal("other account withdrew")
	}
	pool, err := ts.store.Reader(t.Context(), other)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM workflow_proposals").Scan(&count); err != nil || count != 0 {
		t.Fatalf("RLS: %d %v", count, err)
	}
	// Same account, another host still cannot withdraw the proposer's work.
	second := keyOf(t, h, "host-2")
	r = call(t, anon(h), "POST", "/api/workflow-proposals/"+p+"/withdraw", `{"reason":"No longer needed"}`, map[string]string{"Authorization": "Bearer " + second})
	if r.Code != 404 {
		t.Fatal("another proposer withdrew")
	}
	decide(p, "approve", 200)
	var wid string
	d.Owner.QueryRow(t.Context(), "SELECT workflow_id::text FROM workflow_proposals WHERE id=$1", p).Scan(&wid)
	decide(p, "approve", 409)
	del := propose(`{"operation":"delete","summary":"Remove new workflow","workflowId":"` + wid + `","baseVersion":1}`)
	decide(del, "approve", 200)
	if get(t, h, "/workflows/"+wid, nil).Code != 404 {
		t.Fatal("deleted workflow visible")
	}
	decide(del, "undo", 200)
	if get(t, h, "/workflows/"+wid, nil).Code != 200 {
		t.Fatal("undo did not restore")
	}
	decide(p, "undo", 409) // restoring deletion advanced the version
	exp := propose(`{"operation":"delete","summary":"Expire this","workflowId":"` + wid + `","baseVersion":3}`)
	if _, err := d.Owner.Exec(t.Context(), "UPDATE workflow_proposals SET expires_at=$2 WHERE id=$1", exp, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	decide(exp, "approve", 409)
	var status string
	d.Owner.QueryRow(t.Context(), "SELECT status FROM workflow_proposals WHERE id=$1", exp).Scan(&status)
	if status != "expired" {
		t.Fatal(status)
	}
	// Undo a creation preserves its versions as a tombstone.
	created := propose(`{"operation":"create","summary":"Temporary workflow","workflow":` + wfBody(t, "Temporary", workflow.Starting) + `}`)
	decide(created, "approve", 200)
	decide(created, "undo", 200)
	decide(created, "undo", 409)
	if err := db.CheckContract(t.Context(), d.Owner, d.Readonly); err != nil {
		t.Fatal(err)
	}
}

func TestProposalConcurrentApprovalAndUndoConflict(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	id := createWorkflow(t, h, workflow.Starting)
	r := call(t, anon(h), "POST", "/api/workflow-proposals", `{"operation":"update","summary":"Change once","workflowId":"`+id+`","baseVersion":1,"workflow":`+wfBody(t, "Changed", workflow.Starting)+`}`, map[string]string{"Authorization": "Bearer " + key})
	if r.Code != 201 {
		t.Fatal(r.Body)
	}
	var p struct{ ID string }
	json.Unmarshal(r.Body.Bytes(), &p)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := call(t, h, "POST", "/api/workflow-proposals/"+p.ID+"/approve", `{}`, sameOriginFetch)
			codes <- r.Code
		}()
	}
	wg.Wait()
	close(codes)
	sum := 0
	for c := range codes {
		sum += c
	}
	if sum != 609 {
		t.Fatalf("concurrent codes total: %d", sum)
	}
	r = call(t, h, "PUT", "/api/workflows/"+id, wfBody(t, "Later edit", workflow.Starting), sameOriginFetch)
	if r.Code != 200 {
		t.Fatal(r.Body)
	}
	r = call(t, h, "POST", "/api/workflow-proposals/"+p.ID+"/undo", `{}`, sameOriginFetch)
	if r.Code != 409 {
		t.Fatal("undo overwrote later work")
	}
	var name string
	d.Owner.QueryRow(t.Context(), "SELECT name FROM workflows WHERE id=$1", id).Scan(&name)
	if name != "Later edit" {
		t.Fatal(name)
	}
}

func TestProposalDeleteClearsDefaultAndUndoChecksDefaultConflict(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	id := createWorkflow(t, h, workflow.Starting)
	r := call(t, h, "PUT", "/api/workflows/"+id+"/default", `{"default":true}`, sameOriginFetch)
	if r.Code != 204 {
		t.Fatal(r.Body)
	}
	r = call(t, anon(h), "POST", "/api/workflow-proposals", `{"operation":"delete","summary":"Delete default","workflowId":"`+id+`","baseVersion":1}`, map[string]string{"Authorization": "Bearer " + key})
	if r.Code != 201 {
		t.Fatal(r.Body)
	}
	var p struct{ ID string }
	json.Unmarshal(r.Body.Bytes(), &p)
	r = call(t, h, "POST", "/api/workflow-proposals/"+p.ID+"/approve", `{}`, sameOriginFetch)
	if r.Code != 200 {
		t.Fatal(r.Body)
	}
	var chosen bool
	d.Owner.QueryRow(t.Context(), "SELECT is_default FROM workflows WHERE id=$1", id).Scan(&chosen)
	if chosen {
		t.Fatal("deleted workflow is still default")
	}
	other := createWorkflow(t, h, workflow.Starting)
	r = call(t, h, "PUT", "/api/workflows/"+other+"/default", `{"default":true}`, sameOriginFetch)
	if r.Code != 204 {
		t.Fatal(r.Body)
	}
	r = call(t, h, "POST", "/api/workflow-proposals/"+p.ID+"/undo", `{}`, sameOriginFetch)
	if r.Code != 409 {
		t.Fatalf("undo replaced default: %d %s", r.Code, r.Body)
	}
}

func TestProposalStaleWithdrawReject(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	id := createWorkflow(t, h, workflow.Starting)
	propose := func() string {
		r := call(t, anon(h), http.MethodPost, "/api/workflow-proposals", `{"operation":"delete","workflowId":"`+id+`","baseVersion":1,"summary":"Remove unused workflow"}`, map[string]string{"Authorization": "Bearer " + key})
		if r.Code != 201 {
			t.Fatalf("propose: %d %s", r.Code, r.Body)
		}
		var p struct{ ID string }
		json.Unmarshal(r.Body.Bytes(), &p)
		return p.ID
	}
	withdrawn := propose()
	rejected := propose()
	stale := propose()
	r := call(t, anon(h), "POST", "/api/workflow-proposals/"+withdrawn+"/withdraw", `{"reason":"No longer needed"}`, map[string]string{"Authorization": "Bearer " + key})
	if r.Code != 200 {
		t.Fatalf("withdraw: %d %s", r.Code, r.Body)
	}
	r = call(t, h, "POST", "/api/workflow-proposals/"+rejected+"/reject", `{}`, sameOriginFetch)
	if r.Code != 200 {
		t.Fatalf("reject: %d", r.Code)
	}
	r = call(t, h, "PUT", "/api/workflows/"+id, wfBody(t, "New version", workflow.Starting), sameOriginFetch)
	if r.Code != 200 {
		t.Fatal(r.Body)
	}
	for _, p := range []string{withdrawn, rejected, stale} {
		r = call(t, h, "POST", "/api/workflow-proposals/"+p+"/approve", `{}`, sameOriginFetch)
		if r.Code != 409 {
			t.Fatalf("approve invalid: %d %s", r.Code, r.Body)
		}
	}
	code, result := sqlPost(t, h, q("SELECT effective_status FROM workflow_proposal_status WHERE id='"+stale+"'"))
	if code != 200 || len(result.Rows) != 1 || result.Rows[0][0] != "stale" {
		t.Fatalf("effective status: %d %+v", code, result)
	}
}
