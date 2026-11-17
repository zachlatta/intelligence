package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestProposalReopen(t *testing.T) {
	for _, scenario := range []string{"success", "drift", "expired", "pending"} {
		t.Run(scenario, func(t *testing.T) {
			h, d := dbServer(t)
			sendReport(t, h, wfReport())
			wid := createWorkflow(t, h, workflow.Starting)
			r := call(t, anon(h), "POST", "/api/workflow-proposals", `{"operation":"delete","workflowId":"`+wid+`","baseVersion":1,"summary":"Remove unused workflow"}`, map[string]string{"Authorization": "Bearer " + keyOf(t, h, "host-1")})
			if r.Code != 201 {
				t.Fatal(r.Body)
			}
			var p struct{ ID string }
			json.Unmarshal(r.Body.Bytes(), &p)
			path := "/api/workflow-proposals/" + p.ID
			if scenario != "pending" {
				if r := call(t, h, "POST", path+"/reject", `{}`, sameOriginFetch); r.Code != 200 {
					t.Fatal(r.Body)
				}
			}
			if scenario == "drift" {
				if r := call(t, h, "PUT", "/api/workflows/"+wid, wfBody(t, "Changed", workflow.Starting), sameOriginFetch); r.Code != 200 {
					t.Fatal(r.Body)
				}
			}
			if scenario == "expired" {
				if _, err := d.Owner.Exec(t.Context(), "UPDATE workflow_proposals SET expires_at=now()-interval '1 second' WHERE id=$1", p.ID); err != nil {
					t.Fatal(err)
				}
			}
			if r := call(t, anon(h), "POST", path+"/reopen", `{}`, map[string]string{"Authorization": "Bearer " + keyOf(t, h, "host-1")}); r.Code != 401 {
				t.Fatalf("host reopen: %d", r.Code)
			}
			page := get(t, h, "/workflow-proposals/"+p.ID, nil).Body.String()
			if strings.Contains(page, `data-action="reopen"`) != (scenario == "success" || scenario == "drift") {
				t.Fatal("incorrect Reopen visibility")
			}
			want := 409
			if scenario == "success" {
				want = 200
			}
			r = call(t, h, "POST", path+"/reopen", `{}`, sameOriginFetch)
			if r.Code != want {
				t.Fatalf("reopen: %d %s", r.Code, r.Body)
			}
			if scenario == "success" {
				code, res := sqlPost(t, h, q("SELECT status,decided_at IS NULL,before_state->>'version',proposed_state->>'deleted' FROM workflow_proposals WHERE id='"+p.ID+"'"))
				if code != 200 || res.Rows[0][0] != "pending" || res.Rows[0][1] != true || res.Rows[0][2] != "1" || res.Rows[0][3] != "true" {
					t.Fatalf("snapshot: %+v", res)
				}
				code, res = sqlPost(t, h, q("SELECT actor,reopened_at IS NOT NULL,rejected_at IS NOT NULL FROM workflow_proposal_reopens"))
				if code != 200 || len(res.Rows) != 1 || res.Rows[0][0] != "reviewer" || res.Rows[0][1] != true || res.Rows[0][2] != true {
					t.Fatalf("audit: %+v", res)
				}
				if r := call(t, h, "POST", path+"/approve", `{}`, sameOriginFetch); r.Code != 200 {
					t.Fatal(r.Body)
				}
			}
		})
	}
}

func TestProposalOwnerCommentReopenAnswer(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	id := proposalForQuestion(t, h)
	path := "/api/workflow-proposals/" + id
	if r := call(t, h, "POST", path+"/reject", `{}`, sameOriginFetch); r.Code != 200 {
		t.Fatal(r.Body)
	}
	question := askProposal(t, h, id, "Please reopen this proposal so I can review it again.")
	var args []byte
	if err := d.Owner.QueryRow(t.Context(), `SELECT c.args FROM commands c JOIN workflow_proposal_questions q ON q.command_id=c.id WHERE q.question_id=$1`, question).Scan(&args); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--reopen") {
		t.Fatal("agent has no reopen permission or command")
	}
	body := fmt.Sprintf(`{"questionId":%d,"text":"Reopened: [review proposal](/workflow-proposals/%s)","reopen":true}`, question, id)
	for range 2 {
		r := call(t, anon(h), "POST", path+"/comments/agent", body, map[string]string{"Authorization": "Bearer " + keyOf(t, h, "host-1")})
		if r.Code != 201 {
			t.Fatalf("answer/reopen: %d %s", r.Code, r.Body)
		}
	}
	code, res := sqlPost(t, h, q("SELECT status,(SELECT count(*) FROM workflow_proposal_reopens), (SELECT count(*) FROM workflow_proposal_comments WHERE author <> 'reviewer') FROM workflow_proposals WHERE id='"+id+"'"))
	if code != 200 || res.Rows[0][0] != "pending" || res.Rows[0][1] != float64(1) || res.Rows[0][2] != float64(1) {
		t.Fatalf("reopen answer: %+v", res)
	}
}

func TestProposalReopenAnswerRefusals(t *testing.T) {
	for _, scenario := range []string{"drift", "expired", "wrong host", "ordinary answer", "standalone"} {
		t.Run(scenario, func(t *testing.T) {
			h, d := dbServer(t)
			sendReport(t, h, wfReport())
			wid := createWorkflow(t, h, workflow.Starting)
			r := call(t, anon(h), "POST", "/api/workflow-proposals", `{"operation":"delete","summary":"Remove workflow","workflowId":"`+wid+`","baseVersion":1}`, map[string]string{"Authorization": "Bearer " + keyOf(t, h, "host-1")})
			if r.Code != 201 {
				t.Fatal(r.Body)
			}
			var p struct{ ID string }
			json.Unmarshal(r.Body.Bytes(), &p)
			path := "/api/workflow-proposals/" + p.ID
			if r := call(t, h, "POST", path+"/reject", `{}`, sameOriginFetch); r.Code != 200 {
				t.Fatal(r.Body)
			}
			question := askProposal(t, h, p.ID, "Please reopen this")
			if scenario == "drift" {
				if r := call(t, h, "PUT", "/api/workflows/"+wid, wfBody(t, "New revision", workflow.Starting), sameOriginFetch); r.Code != 200 {
					t.Fatal(r.Body)
				}
			}
			if scenario == "expired" {
				if _, err := d.Owner.Exec(t.Context(), "UPDATE workflow_proposals SET expires_at=now()-interval '1 second' WHERE id=$1", p.ID); err != nil {
					t.Fatal(err)
				}
			}
			host := "host-1"
			want := 409
			reopen := true
			if scenario == "wrong host" {
				host = "host-2"
				want = 404
			}
			if scenario == "ordinary answer" {
				reopen = false
				want = 201
			}
			if scenario == "standalone" {
				question = 0
				want = 400
			}
			body := fmt.Sprintf(`{"questionId":%d,"text":"Answer","reopen":%t}`, question, reopen)
			r = call(t, anon(h), "POST", path+"/comments/agent", body, map[string]string{"Authorization": "Bearer " + keyOf(t, h, host)})
			if r.Code != want {
				t.Fatalf("answer: %d %s", r.Code, r.Body)
			}
			if scenario == "ordinary answer" {
				r = call(t, anon(h), "POST", path+"/comments/agent", strings.Replace(body, `"reopen":false`, `"reopen":true`, 1), map[string]string{"Authorization": "Bearer " + keyOf(t, h, host)})
				if r.Code != 409 {
					t.Fatalf("changed replay claimed reopen: %d %s", r.Code, r.Body)
				}
			}
			code, res := sqlPost(t, h, q("SELECT status,(SELECT count(*) FROM workflow_proposal_reopens) FROM workflow_proposals"))
			if code != 200 || res.Rows[0][0] != "rejected" || res.Rows[0][1] != float64(0) {
				t.Fatalf("refusal changed review state: %+v", res)
			}
			if scenario != "ordinary answer" {
				code, res = sqlPost(t, h, q("SELECT count(*) FROM workflow_proposal_comments WHERE author <> 'reviewer'"))
				if code != 200 || res.Rows[0][0] != float64(0) {
					t.Fatalf("refusal posted success answer: %+v", res)
				}
			}
		})
	}
}

func TestProposalReopenAccountIsolation(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	id := proposalForQuestion(t, h)
	path := "/api/workflow-proposals/" + id
	if r := call(t, h, "POST", path+"/reject", `{}`, sameOriginFetch); r.Code != 200 {
		t.Fatal(r.Body)
	}
	ts := h.(*testServer)
	other, err := ts.store.Create(t.Context(), "other-reviewer", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	cookie := sessionFor(t, ts.store, other)
	if r := call(t, h, "POST", path+"/reopen", `{}`, sameOriginFetch, cookie); r.Code != 404 {
		t.Fatalf("other owner reopen: %d", r.Code)
	}
	if r := call(t, h, "POST", path+"/reopen", `{}`, map[string]string{"Origin": "https://evil.example"}); r.Code != 403 {
		t.Fatalf("cross origin: %d", r.Code)
	}
	if r := call(t, h, "POST", path+"/reopen", `{}`, sameOriginFetch); r.Code != 200 {
		t.Fatal(r.Body)
	}
	r := call(t, h, "POST", "/api/sql", q("SELECT * FROM workflow_proposal_reopens"), sameOriginFetch, cookie)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"rowCount":0`) {
		t.Fatalf("audit leaked: %d %s", r.Code, r.Body)
	}
}
