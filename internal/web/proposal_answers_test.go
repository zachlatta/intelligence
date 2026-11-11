package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/paseo"
	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func proposalForQuestion(t *testing.T, h http.Handler) string {
	t.Helper()
	r := call(t, anon(h), "POST", "/api/workflow-proposals", `{"operation":"create","summary":"**Explain this change**","workflow":`+wfBody(t, "Question workflow", workflow.Starting)+`}`, map[string]string{"Authorization": "Bearer " + keyOf(t, h, "host-1")})
	if r.Code != 201 {
		t.Fatal(r.Body)
	}
	var p struct{ ID string }
	if err := json.Unmarshal(r.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p.ID
}

func askProposal(t *testing.T, h http.Handler, id, text string) int64 {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"text": text})
	r := call(t, h, "POST", "/api/workflow-proposals/"+id+"/comments", string(body), sameOriginFetch)
	if r.Code != 201 {
		t.Fatalf("ask: %d %s", r.Code, r.Body)
	}
	var q struct{ ID int64 }
	if err := json.Unmarshal(r.Body.Bytes(), &q); err != nil || q.ID == 0 {
		t.Fatalf("question id: %s %v", r.Body, err)
	}
	return q.ID
}

// Exercise the actual dispatch transport and host executor against the fake Paseo
// daemon. Only the model's answer is simulated: it uses the production CLI and API.
func TestProposalQuestionDispatchAndAnswer(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	id := proposalForQuestion(t, h)
	question := askProposal(t, h, id, "Why is this safer?")
	var commandID, state string
	if err := d.Owner.QueryRow(t.Context(), "SELECT command_id::text,state FROM workflow_proposal_question_status WHERE question_id=$1", question).Scan(&commandID, &state); err != nil || state != "queued" {
		t.Fatalf("queued: %s %v", state, err)
	}
	// A new server instance can deliver the durable job after a restart.
	h = instance(t, d, d.Owner)
	srv := serve(t, h)
	daemon := paseotest.New(t, "tok")
	ctx, cancel := context.WithCancel(t.Context())
	client := &hostclient.Client{Name: "host-1", Version: "v", Settle: 10 * time.Millisecond,
		Watcher: &hostclient.Watcher{RetryEvery: 20 * time.Millisecond, Dial: func(ctx context.Context) (*paseo.Client, error) {
			return paseo.Dial(ctx, paseo.Target{URL: daemon.URL(), Token: "tok", ClientID: "test"})
		}},
		Link: &hostclient.Link{Server: srv.URL, Token: keyOf(t, h, "host-1"), Host: "host-1", RetryEvery: 20 * time.Millisecond}}
	stopped := make(chan struct{})
	go func() { client.Run(ctx); close(stopped) }()
	t.Cleanup(func() { cancel(); <-stopped })
	_, cmd := showCmd(t, h, "host-1", commandID, "?wait=10s")
	if cmd.Status != hostclient.CommandSucceeded {
		t.Fatalf("dispatch: %+v", cmd)
	}
	requests := daemon.Requests("agent.create.request")
	if len(requests) != 1 {
		t.Fatalf("agent requests: %+v", requests)
	}
	prompt, _ := requests[0]["initialPrompt"].(string)
	for _, want := range []string{id, "Why is this safer?", "Explain this change", "Workflow diff", "Thread", "answer-proposal", fmt.Sprint(question), "Do not execute"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if requests[0]["idempotencyKey"] != commandID {
		t.Fatal("dispatch lost its idempotency key")
	}
	// The host's agent publishes its answer using the CLI's existing host config.
	home := t.TempDir()
	keyFile := filepath.Join(home, "host.key")
	if err := os.WriteFile(keyFile, []byte(keyOf(t, h, "host-1")), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": home, "HOST_SERVER": srv.URL, "HOST_KEY_FILE": keyFile}
	var out, errOut bytes.Buffer
	args := []string{"answer-proposal", "--proposal", id, "--question", fmt.Sprint(question), "--note", "-"}
	for range 2 {
		code := workflow.CLI(t.Context(), args, func(k string) string { return env[k] }, strings.NewReader("It **reduces risk**."), &out, &errOut, http.DefaultClient)
		if code != 0 {
			t.Fatalf("answer CLI: %d %s %s", code, &out, &errOut)
		}
	}
	code, res := sqlPost(t, h, q("SELECT state,answer_comment_id FROM workflow_proposal_question_status"))
	if code != 200 || len(res.Rows) != 1 || res.Rows[0][0] != "answered" || res.Rows[0][1] == nil {
		t.Fatalf("answered: %+v", res)
	}
	code, res = sqlPost(t, h, q("SELECT text FROM workflow_proposal_comments WHERE author <> 'reviewer'"))
	if code != 200 || len(res.Rows) != 1 {
		t.Fatalf("duplicate answer: %+v", res)
	}
	page := get(t, h, "/workflow-proposals/"+id, nil).Body.String()
	for _, want := range []string{"<strong>reduces risk</strong>", "answered", `id="proposal-thread"`, "setInterval(refreshThread, 4000)"} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
	next := askProposal(t, h, id, "What about compatibility?")
	var argsJSON []byte
	if err := d.Owner.QueryRow(t.Context(), `SELECT c.args FROM commands c JOIN workflow_proposal_questions q ON q.command_id=c.id WHERE q.question_id=$1`, next).Scan(&argsJSON); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argsJSON), "reduces risk") {
		t.Fatal("follow-up lost previous answer")
	}
	_, token := mintMCP(t, h)
	got := rpc(t, h, token, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"comment_proposal","arguments":{"id":"%s","questionId":%d,"text":"MCP linked answer"}}}`, id, next))
	if !strings.Contains(got, `"isError":false`) {
		t.Fatal(got)
	}
	code, res = sqlPost(t, h, q(fmt.Sprintf("SELECT state FROM workflow_proposal_question_status WHERE question_id=%d", next)))
	if code != 200 || len(res.Rows) != 1 || res.Rows[0][0] != "answered" {
		t.Fatalf("MCP did not complete answer: %+v", res)
	}
}

func TestProposalQuestionFailureRetryAndIsolation(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	id := proposalForQuestion(t, h)
	question := askProposal(t, h, id, "Explain failure handling")
	retry := fmt.Sprintf("/api/workflow-proposals/%s/questions/%d/retry", id, question)
	if r := call(t, h, "POST", retry, `{}`, sameOriginFetch); r.Code != 409 {
		t.Fatalf("retry active: %d %s", r.Code, r.Body)
	}
	var first string
	if err := d.Owner.QueryRow(t.Context(), "SELECT command_id::text FROM workflow_proposal_questions WHERE question_id=$1", question).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := fleet.New(d.Owner).UpdateCommand(t.Context(), "host-1", hostclient.CommandUpdate{ID: first, Status: hostclient.CommandFailed, Error: "provider unavailable"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	page := get(t, h, "/workflow-proposals/"+id, nil).Body.String()
	if !strings.Contains(page, "provider unavailable") || !strings.Contains(page, "Retry answer") {
		t.Fatal("failed question has no visible recovery")
	}
	if r := call(t, h, "POST", retry, `{}`, sameOriginFetch); r.Code != 201 {
		t.Fatalf("retry failed: %d %s", r.Code, r.Body)
	}
	var second string
	d.Owner.QueryRow(t.Context(), "SELECT command_id::text FROM workflow_proposal_questions WHERE question_id=$1", question).Scan(&second)
	if second == first {
		t.Fatal("retry did not queue a fresh command")
	}
	// A successful dispatch is not a completed answer. Missing replies time out
	// independently of whether the host sent another frame.
	if err := fleet.New(d.Owner).UpdateCommand(t.Context(), "host-1", hostclient.CommandUpdate{ID: second, Status: hostclient.CommandSucceeded, Result: json.RawMessage(`{"agentId":"slow-answer"}`)}, time.Now().Add(-31*time.Minute)); err != nil {
		t.Fatal(err)
	}
	code, res := sqlPost(t, h, q("SELECT state FROM workflow_proposal_question_status"))
	if code != 200 || len(res.Rows) != 1 || res.Rows[0][0] != "timed_out" {
		t.Fatalf("answer timeout: %+v", res)
	}
	// Retrying a question that fell outside the recent-thread window must still
	// carry that exact question's text.
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_proposal_comments(proposal_id,author,text) SELECT $1,'host:host-1','Later discussion' FROM generate_series(1,101)`, id); err != nil {
		t.Fatal(err)
	}
	if r := call(t, h, "POST", retry, `{}`, sameOriginFetch); r.Code != 201 {
		t.Fatalf("retry timeout: %d %s", r.Code, r.Body)
	}
	var retryArgs []byte
	if err := d.Owner.QueryRow(t.Context(), `SELECT c.args FROM commands c JOIN workflow_proposal_questions q ON q.command_id=c.id WHERE q.question_id=$1`, question).Scan(&retryArgs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(retryArgs), "Explain failure handling") {
		t.Fatal("retry lost the original question")
	}
	d.Owner.QueryRow(t.Context(), "SELECT command_id::text FROM workflow_proposal_questions WHERE question_id=$1", question).Scan(&second)
	// A timeout is a SQL-visible state even when no host is connected to update it.
	if _, err := d.Owner.Exec(t.Context(), "UPDATE commands SET expires_at=now()-interval '1 second' WHERE id=$1", second); err != nil {
		t.Fatal(err)
	}
	code, res = sqlPost(t, h, q("SELECT state FROM workflow_proposal_question_status"))
	if code != 200 || len(res.Rows) != 1 || res.Rows[0][0] != "failed" {
		t.Fatalf("expiry: %+v", res)
	}
	otherKey := keyOf(t, h, "host-2")
	body := fmt.Sprintf(`{"questionId":%d,"text":"wrong host"}`, question)
	r := call(t, anon(h), "POST", "/api/workflow-proposals/"+id+"/comments/agent", body, map[string]string{"Authorization": "Bearer " + otherKey})
	if r.Code != 404 {
		t.Fatalf("other host answered: %d %s", r.Code, r.Body)
	}
	ts := h.(*testServer)
	other, err := ts.store.Create(t.Context(), "proposal-other", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	cookie := sessionFor(t, ts.store, other)
	r = call(t, h, "POST", "/api/sql", q("SELECT * FROM workflow_proposal_question_status"), sameOriginFetch, cookie)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"rowCount":0`) {
		t.Fatalf("job SQL isolation: %d %s", r.Code, r.Body)
	}
	r = call(t, h, "POST", retry, `{}`, sameOriginFetch, cookie)
	if r.Code != 404 {
		t.Fatalf("other account retry: %d %s", r.Code, r.Body)
	}
	// With no eligible host the write fails atomically, so the page can keep the
	// draft instead of showing a saved question that can never be dispatched.
	if _, err := d.Owner.Exec(t.Context(), "DELETE FROM host_keys WHERE account_id=$1", ts.acct.ID); err != nil {
		t.Fatal(err)
	}
	r = call(t, h, "POST", "/api/workflow-proposals/"+id+"/comments", `{"text":"No host question"}`, sameOriginFetch)
	if r.Code != 409 {
		t.Fatalf("unavailable host: %d %s", r.Code, r.Body)
	}
	code, res = sqlPost(t, h, q("SELECT text FROM workflow_proposal_comments WHERE text='No host question'"))
	if code != 200 || len(res.Rows) != 0 {
		t.Fatalf("orphaned question: %+v", res)
	}
}
