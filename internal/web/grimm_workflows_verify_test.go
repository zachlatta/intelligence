package web

// sage's spec tests for "Grimm starts a workflow with approval in the web interface, and the card
// monitors the run as it runs". Written from the spec (and the README's run proposals), not from
// the builder's code: the only things taken from the build are the hooks at the top of this file.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/grimm"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// ---- hooks from the build (the CLI syntax only) ----

// verifyRequestArgs is the command line of `intelligence grimm ctl workflow request`.
func verifyRequestArgs(wf, summary, input string) []string {
	return []string{"ctl", "workflow", "request", "--workflow", wf, "--summary", summary, input}
}

// verifyEditPath is the browser-only route that edits a pending request's input, with {"input"}.
func verifyEditPath(session, proposal string) string {
	return "/api/grimm/sessions/" + session + "/workflow-requests/" + proposal + "/input"
}

// ---- rig ----

type wfReqRig struct {
	h       http.Handler
	d       dbtest.DB
	stream  *rawHost
	session string
	wf      string
	key     string
}

// newWfReqRig is a session on host-1 whose manager is agent mgr-1 and whose worker athena is w-1,
// with a workflow on host-1.
func newWfReqRig(t *testing.T) *wfReqRig {
	t.Helper()
	h, d := dbServer(t)
	stream := grimmRig(t, h)
	wf := createWorkflow(t, h, workflow.Starting)
	rec := call(t, h, "POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/app"}`, sameOriginFetch)
	if rec.Code != 201 {
		t.Fatalf("create session: %d %s", rec.Code, rec.Body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	succeed(t, stream, stream.command(), `{"workspaceId":"w-app"}`)
	if rec := call(t, h, "POST", "/api/grimm/sessions/"+created.ID+"/messages", `{"text":"Run the workflow when it makes sense"}`, sameOriginFetch); rec.Code != 202 {
		t.Fatalf("message: %d %s", rec.Code, rec.Body)
	}
	mgr := stream.command()
	if mgr.Kind != hostclient.CreateAgentKind {
		t.Fatalf("manager command = %s", mgr.Kind)
	}
	succeed(t, stream, mgr, `{"agentId":"mgr-1"}`)
	r := &wfReqRig{h: h, d: d, stream: stream, session: created.ID, wf: wf, key: keyOf(t, h, "host-1")}
	r.waitSQL(t, "the manager's agent", `SELECT count(*) FROM grimm_agents WHERE session_id = $1 AND role = 'manager' AND agent_id = 'mgr-1'`)
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec = call(t, h, "POST", "/api/grimm/sessions/"+created.ID+"/workers", `{"name":"athena","role":"builder","task":"build it"}`, sameOriginFetch)
		if rec.Code == 201 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if rec.Code != 201 {
		t.Fatalf("hire: %d %s", rec.Code, rec.Body)
	}
	succeed(t, stream, r.nextCommand(t, hostclient.CreateAgentKind), `{"agentId":"w-1"}`)
	r.waitSQL(t, "the worker's agent", `SELECT count(*) FROM grimm_agents WHERE session_id = $1 AND role <> 'manager' AND agent_id = 'w-1'`)
	return r
}

func (r *wfReqRig) waitSQL(t *testing.T, what, query string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var n int
		if err := r.d.Owner.QueryRow(t.Context(), query, r.session).Scan(&n); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never saw %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// nextCommand skips the host's other commands (watch_agent, directives) until one of kind.
func (r *wfReqRig) nextCommand(t *testing.T, kind string) hostclient.Command {
	t.Helper()
	for range 20 {
		c := r.stream.command()
		if c.Kind == kind {
			return c
		}
	}
	t.Fatalf("no %s command", kind)
	return hostclient.Command{}
}

// agent runs an `intelligence grimm` command line as agent on host-1, as the CLI would send it.
func (r *wfReqRig) agent(t *testing.T, agent string, args ...string) (int, string) {
	t.Helper()
	c, err := grimm.ParseCLI(args, strings.NewReader(""))
	if err != nil {
		t.Fatalf("the CLI does not parse %q: %v", args, err)
	}
	body, _ := json.Marshal(c)
	rec := call(t, anon(r.h), "POST", grimm.AgentPath, string(body), map[string]string{"Authorization": "Bearer " + r.key, grimm.AgentHeader: agent})
	return rec.Code, rec.Body.String()
}

func (r *wfReqRig) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := r.d.Owner.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// proposals are the session's run proposals, newest first.
func (r *wfReqRig) proposals(t *testing.T) []string {
	t.Helper()
	rows, err := r.d.Owner.Query(t.Context(), `SELECT id::text FROM workflow_proposals WHERE operation = 'run' ORDER BY created_at DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids
}

// tiedToSession says whether proposal is linked to the session in SQL a read-only client can see:
// a column of workflow_proposals, or a public relation with the proposal's and the session's ids.
func (r *wfReqRig) tiedToSession(t *testing.T, proposal string) bool {
	t.Helper()
	rows, err := r.d.Owner.Query(t.Context(), `SELECT c.table_name, c.column_name, p.column_name FROM information_schema.columns c
		JOIN information_schema.columns p ON p.table_schema = c.table_schema AND p.table_name = c.table_name
		WHERE c.table_schema = 'public' AND c.column_name ~ 'session' AND (p.column_name ~ 'proposal' OR c.table_name IN ('workflow_proposals','workflow_proposal_status') AND p.column_name = 'id')`)
	if err != nil {
		t.Fatal(err)
	}
	type rel struct{ table, sess, prop string }
	var rels []rel
	for rows.Next() {
		var x rel
		_ = rows.Scan(&x.table, &x.sess, &x.prop)
		rels = append(rels, x)
	}
	rows.Close()
	for _, x := range rels {
		code, res := sqlPost(t, r.h, q(`SELECT count(*) FROM "`+x.table+`" WHERE "`+x.sess+`"::text = '`+r.session+`' AND "`+x.prop+`"::text = '`+proposal+`'`))
		if code == 200 && len(res.Rows) == 1 && res.Rows[0][0] != float64(0) {
			return true
		}
	}
	return false
}

func (r *wfReqRig) runs(t *testing.T) int { return r.count(t, `SELECT count(*) FROM workflow_runs`) }

// wakeupsMention: the manager's queued or delivered news mentioning any of words.
func (r *wfReqRig) wakeupsMention(t *testing.T, words ...string) bool {
	t.Helper()
	for _, w := range words {
		if r.count(t, `SELECT count(*) FROM grimm_wakeups WHERE session_id = $1 AND (summary || ' ' || detail) ILIKE '%' || $2 || '%'`, r.session, w) > 0 {
			return true
		}
	}
	return false
}

func (r *wfReqRig) request(t *testing.T, agent, summary, input string) (int, string) {
	t.Helper()
	return r.agent(t, agent, verifyRequestArgs(r.wf, summary, input)...)
}

// ---- tests ----

// The manager can ask for a run, which becomes a pending run proposal tied to the session; it
// never starts one, and a worker cannot ask at all.
func TestVerifyGrimmManagerRequestsARunProposalAndNeverStartsOne(t *testing.T) {
	t.Parallel()
	r := newWfReqRig(t)
	if code, body := r.request(t, "w-1", "worker wants a run", "Do the task"); code != 403 {
		t.Errorf("a worker requested a run: %d %s", code, body)
	}
	if n := len(r.proposals(t)); n != 0 {
		t.Fatalf("a worker's request made %d proposals", n)
	}
	input := "Investigate the flaky login test\nKeep the full task."
	code, body := r.request(t, "mgr-1", "Find the flake", input)
	if code/100 != 2 {
		t.Fatalf("the manager's request: %d %s", code, body)
	}
	ids := r.proposals(t)
	if len(ids) != 1 {
		t.Fatalf("proposals: %v", ids)
	}
	p := ids[0]
	if !strings.Contains(body, p) {
		t.Errorf("the manager is not told the proposal's id or link: %s", body)
	}
	if r.runs(t) != 0 {
		t.Fatal("requesting a run started one")
	}
	if n := r.count(t, `SELECT count(*) FROM commands WHERE kind = 'create_agent' AND args::text LIKE '%Investigate the flaky%'`); n != 0 {
		t.Fatal("requesting a run queued a launch")
	}
	code, res := sqlPost(t, r.h, q(`SELECT effective_status, workflow_id::text, input FROM workflow_proposal_status WHERE id = '`+p+`'`))
	if code != 200 || len(res.Rows) != 1 || res.Rows[0][0] != "pending" || res.Rows[0][1] != r.wf {
		t.Fatalf("the proposal through /api/sql: %d %+v", code, res)
	}
	if s, _ := res.Rows[0][2].(string); !strings.Contains(s, "Keep the full task.") {
		t.Errorf("the proposal lost the full input: %v", res.Rows[0][2])
	}
	if !r.tiedToSession(t, p) {
		t.Error("the proposal is not tied to the Grimm session in SQL")
	}
	// The thread has a card for it.
	code, res = sqlPost(t, r.h, q(`SELECT count(*) FROM grimm_thread t WHERE t.session_id = '`+r.session+`' AND to_jsonb(t)::text LIKE '%`+p+`%'`))
	if code != 200 || res.Rows[0][0] == float64(0) {
		t.Errorf("no thread item for the proposal: %d %+v", code, res)
	}
	// No agent route starts a run directly: the manager cannot approve its own request.
	for _, args := range [][]string{{"ctl", "workflow", "approve", p}, {"ctl", "workflow", "start", r.wf}} {
		if c, err := grimm.ParseCLI(args, strings.NewReader("")); err == nil {
			b, _ := json.Marshal(c)
			call(t, anon(r.h), "POST", grimm.AgentPath, string(b), map[string]string{"Authorization": "Bearer " + r.key, grimm.AgentHeader: "mgr-1"})
		}
	}
	if r.runs(t) != 0 {
		t.Fatal("an agent command started a run")
	}
}

// Only Zach, in a signed-in browser, approves or rejects: the agent's host key cannot, and a
// cross-site request cannot.
func TestVerifyGrimmRunApprovalIsBrowserOnly(t *testing.T) {
	t.Parallel()
	r := newWfReqRig(t)
	if code, body := r.request(t, "mgr-1", "Run it", "Do the thing"); code/100 != 2 {
		t.Fatalf("request: %d %s", code, body)
	}
	p := r.proposals(t)[0]
	for _, action := range []string{"approve", "reject"} {
		if rec := call(t, anon(r.h), "POST", "/api/workflow-proposals/"+p+"/"+action, `{}`, map[string]string{"Authorization": "Bearer " + r.key, grimm.AgentHeader: "mgr-1"}); rec.Code/100 == 2 {
			t.Errorf("the host key could %s: %d", action, rec.Code)
		}
		if rec := call(t, r.h, "POST", "/api/workflow-proposals/"+p+"/"+action, `{}`, map[string]string{"Sec-Fetch-Site": "cross-site"}); rec.Code/100 == 2 {
			t.Errorf("a cross-site request could %s: %d", action, rec.Code)
		}
	}
	// Any new Grimm-specific decision route must refuse the host key too.
	for _, path := range []string{"/api/grimm/sessions/" + r.session + "/workflow-requests/" + p + "/approve", verifyEditPath(r.session, p)} {
		if rec := call(t, anon(r.h), "POST", path, `{"input":"agent edit"}`, map[string]string{"Authorization": "Bearer " + r.key, grimm.AgentHeader: "mgr-1"}); rec.Code/100 == 2 {
			t.Errorf("host key approved through %s: %d", path, rec.Code)
		}
	}
	if r.runs(t) != 0 {
		t.Fatal("a refused approval started a run")
	}
	if n := r.count(t, `SELECT count(*) FROM workflow_proposals WHERE id = $1 AND status = 'pending'`, p); n != 1 {
		t.Fatal("a refused decision changed the proposal")
	}
}

// Approving starts exactly one run through the engine and links it; a second approval, even
// concurrent, starts nothing; the manager hears of it; the run's status reaches the session's
// stream live as it changes, through to the end, and the manager hears of the end.
func TestVerifyGrimmApprovalStartsOneRunAndTheSessionFollowsIt(t *testing.T) {
	t.Parallel()
	r := newWfReqRig(t)
	events, cancel := openSSE(t, r.h, "/api/grimm/sessions/"+r.session+"/stream")
	defer cancel()
	var mu sync.Mutex
	var seen []string
	go func() {
		for e := range events {
			mu.Lock()
			seen = append(seen, e[0]+" "+e[1])
			mu.Unlock()
		}
	}()
	waitEvent := func(what string, ok func(s string) bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for i := 0; ; {
			mu.Lock()
			for ; i < len(seen); i++ {
				if ok(seen[i]) {
					mu.Unlock()
					return
				}
			}
			mu.Unlock()
			if time.Now().After(deadline) {
				t.Fatalf("the stream never pushed %s", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if code, body := r.request(t, "mgr-1", "Run it", "Do the approved thing"); code/100 != 2 {
		t.Fatalf("request: %d %s", code, body)
	}
	p := r.proposals(t)[0]
	waitEvent("the request's card", func(s string) bool { return strings.Contains(s, p) })

	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- call(t, r.h, "POST", "/api/workflow-proposals/"+p+"/approve", `{}`, sameOriginFetch).Code
		}()
	}
	wg.Wait()
	close(codes)
	ok := 0
	for c := range codes {
		if c/100 == 2 {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("%d approvals succeeded, want 1", ok)
	}
	if rec := call(t, r.h, "POST", "/api/workflow-proposals/"+p+"/approve", `{}`, sameOriginFetch); rec.Code != 409 {
		t.Errorf("a third approval: %d", rec.Code)
	}
	if rec := call(t, r.h, "POST", "/api/workflow-proposals/"+p+"/reject", `{}`, sameOriginFetch); rec.Code/100 == 2 {
		t.Errorf("reject after approve: %d", rec.Code)
	}
	if n := r.runs(t); n != 1 {
		t.Fatalf("runs after approval: %d", n)
	}
	var run, input string
	if err := r.d.Owner.QueryRow(t.Context(), `SELECT r.id::text, r.input FROM workflow_proposals p JOIN workflow_runs r ON r.id = p.started_run_id WHERE p.id = $1`, p).Scan(&run, &input); err != nil || input != "Do the approved thing" {
		t.Fatalf("the approved run is not linked: %v %q", err, input)
	}
	waitEvent("the run's id on the card", func(s string) bool { return strings.Contains(s, run) })
	r.waitSQL(t, "the manager told of the approval", `SELECT count(*) FROM grimm_wakeups WHERE session_id = $1 AND (summary || ' ' || detail) ~* 'approv|started|run'`)

	// The run moves on: cancel it from its page, and the stream shows it ended, unprompted.
	if rec := call(t, r.h, "POST", "/api/workflow-runs/"+run+"/cancel", `{}`, sameOriginFetch); rec.Code/100 != 2 {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	canceled := regexp.MustCompile(`(?i)cancel`)
	waitEvent("the run's end", func(s string) bool {
		return strings.Contains(s, run) && canceled.MatchString(s) || strings.Contains(s, p) && canceled.MatchString(s)
	})
	r.waitSQL(t, "the manager told the run ended", `SELECT count(*) FROM grimm_wakeups WHERE session_id = $1 AND (summary || ' ' || detail) ~* 'cancel'`)
	if n := r.runs(t); n != 1 {
		t.Fatalf("runs at the end: %d", n)
	}
}

// Rejecting records the decision and tells the manager; nothing starts and it cannot then be approved.
func TestVerifyGrimmRejectionRecordsAndTellsTheManager(t *testing.T) {
	t.Parallel()
	r := newWfReqRig(t)
	if code, body := r.request(t, "mgr-1", "Run it", "Do the rejected thing"); code/100 != 2 {
		t.Fatalf("request: %d %s", code, body)
	}
	p := r.proposals(t)[0]
	if rec := call(t, r.h, "POST", "/api/workflow-proposals/"+p+"/reject", `{}`, sameOriginFetch); rec.Code/100 != 2 {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, r.h, "POST", "/api/workflow-proposals/"+p+"/approve", `{}`, sameOriginFetch); rec.Code != 409 {
		t.Errorf("approve after reject: %d", rec.Code)
	}
	if r.runs(t) != 0 {
		t.Fatal("a rejected request started a run")
	}
	code, res := sqlPost(t, r.h, q(`SELECT effective_status, decided_at IS NOT NULL FROM workflow_proposal_status WHERE id = '`+p+`'`))
	if code != 200 || len(res.Rows) != 1 || res.Rows[0][0] != "rejected" || res.Rows[0][1] != true {
		t.Fatalf("rejection in SQL: %+v", res)
	}
	r.waitSQL(t, "the manager told of the rejection", `SELECT count(*) FROM grimm_wakeups WHERE session_id = $1 AND (summary || ' ' || detail) ~* 'reject'`)
}

// Approval goes through the run queue: with the host near capacity, the run is queued (not
// refused or forced), and the card shows it queued.
func TestVerifyGrimmApprovedRunQueuesWhenTheHostIsFull(t *testing.T) {
	t.Parallel()
	r := newWfReqRig(t)
	hostMinute(t, r.stream, time.Now().UTC().Truncate(time.Minute).Add(-time.Minute), 97, 0.5)
	events, cancel := openSSE(t, r.h, "/api/grimm/sessions/"+r.session+"/stream")
	defer cancel()
	if code, body := r.request(t, "mgr-1", "Run it", "Queued thing"); code/100 != 2 {
		t.Fatalf("request: %d %s", code, body)
	}
	p := r.proposals(t)[0]
	if rec := call(t, r.h, "POST", "/api/workflow-proposals/"+p+"/approve", `{}`, sameOriginFetch); rec.Code/100 != 2 {
		t.Fatalf("approve on a full host: %d %s", rec.Code, rec.Body)
	}
	var run, status string
	if err := r.d.Owner.QueryRow(t.Context(), `SELECT r.id::text, r.status FROM workflow_proposals p JOIN workflow_runs r ON r.id = p.started_run_id WHERE p.id = $1`, p).Scan(&run, &status); err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Fatalf("approved run on a busy host is %s, want queued", status)
	}
	timeout := time.After(20 * time.Second)
	for {
		select {
		case e, open := <-events:
			if !open {
				t.Fatal("stream ended")
			}
			if strings.Contains(e[1], run) && strings.Contains(e[1], "queued") {
				return
			}
		case <-timeout:
			t.Fatal("the card never showed the run queued")
		}
	}
}

// Every Grimm workflow-request relation is in public, readable read-only, and the contract holds.
func TestVerifyGrimmWorkflowRequestsKeepTheSQLContract(t *testing.T) {
	t.Parallel()
	r := newWfReqRig(t)
	if code, body := r.request(t, "mgr-1", "Run it", "Contract thing"); code/100 != 2 {
		t.Fatalf("request: %d %s", code, body)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := db.CheckContract(ctx, r.d.Owner, r.d.Readonly); err != nil {
		t.Fatal(err)
	}
}

// Zach can edit a pending request's input before approving; the run gets the edited input. Only a
// signed-in browser can edit, and only while the request is pending.
func TestVerifyGrimmEditInputBeforeApproval(t *testing.T) {
	t.Parallel()
	r := newWfReqRig(t)
	if code, body := r.request(t, "mgr-1", "Run it", "Original input"); code/100 != 2 {
		t.Fatalf("request: %d %s", code, body)
	}
	p := r.proposals(t)[0]
	if rec := call(t, anon(r.h), "POST", verifyEditPath(r.session, p), `{"input":"agent edit"}`, map[string]string{"Authorization": "Bearer " + r.key, grimm.AgentHeader: "mgr-1"}); rec.Code/100 == 2 {
		t.Errorf("the host key edited the input: %d", rec.Code)
	}
	if rec := call(t, r.h, "POST", verifyEditPath(r.session, p), `{"input":"cross-site edit"}`, map[string]string{"Sec-Fetch-Site": "cross-site"}); rec.Code/100 == 2 {
		t.Errorf("a cross-site edit: %d", rec.Code)
	}
	if rec := call(t, r.h, "POST", verifyEditPath(r.session, p), `{"input":"  "}`, sameOriginFetch); rec.Code/100 == 2 {
		t.Errorf("an empty input was accepted: %d", rec.Code)
	}
	if rec := call(t, r.h, "POST", verifyEditPath(r.session, p), `{"input":"Edited by Zach\nwith care"}`, sameOriginFetch); rec.Code/100 != 2 {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body)
	}
	if r.runs(t) != 0 {
		t.Fatal("editing started a run")
	}
	// All requests the session has point at one pending proposal for this card (an edit that
	// replaces the proposal must leave the old one unapprovable).
	ids := r.proposals(t)
	pending := r.count(t, `SELECT count(*) FROM workflow_proposal_status WHERE effective_status = 'pending'`)
	if pending != 1 {
		t.Fatalf("%d pending proposals after an edit (%v)", pending, ids)
	}
	cur := ids[0]
	for _, id := range ids[1:] {
		if rec := call(t, r.h, "POST", "/api/workflow-proposals/"+id+"/approve", `{}`, sameOriginFetch); rec.Code/100 == 2 {
			t.Fatal("the pre-edit proposal could still be approved")
		}
	}
	if rec := call(t, r.h, "POST", "/api/workflow-proposals/"+cur+"/approve", `{}`, sameOriginFetch); rec.Code/100 != 2 {
		t.Fatalf("approve the edited request: %d %s", rec.Code, rec.Body)
	}
	var input string
	if err := r.d.Owner.QueryRow(t.Context(), `SELECT r.input FROM workflow_proposals p JOIN workflow_runs r ON r.id = p.started_run_id WHERE p.id = $1`, cur).Scan(&input); err != nil {
		t.Fatal(err)
	}
	if input != "Edited by Zach\nwith care" {
		t.Fatalf("the run's input = %q", input)
	}
	if rec := call(t, r.h, "POST", verifyEditPath(r.session, cur), `{"input":"too late"}`, sameOriginFetch); rec.Code/100 == 2 {
		t.Errorf("edited after approval: %d", rec.Code)
	}
	if n := r.runs(t); n != 1 {
		t.Fatalf("runs: %d", n)
	}
}

// TestVerifyGrimmWorkflowBrowser serves a session with a pending workflow request and runs the
// headless-browser check at GRIMM_WORKFLOW_BROWSER_SCRIPT (node, Playwright via NODE_PATH) against
// it: the script approves on the card, the host starts the run here, and the script watches the
// card follow it. Skipped unless set.
func TestVerifyGrimmWorkflowBrowser(t *testing.T) {
	t.Parallel()
	script := os.Getenv("GRIMM_WORKFLOW_BROWSER_SCRIPT")
	if script == "" {
		t.Skip("GRIMM_WORKFLOW_BROWSER_SCRIPT not set")
	}
	for _, vp := range []struct {
		name          string
		width, height int
	}{{"desktop", 1280, 860}, {"phone", 390, 844}} {
		t.Run(vp.name, func(t *testing.T) {
			r := newWfReqRig(t)
			r.wf = fixedWorkflow(t, r.h) // a fixed project: the run goes straight to starting, then running
			if code, body := r.request(t, "mgr-1", "Find the flaky login test", "Investigate the flaky login test and fix it"); code/100 != 2 {
				t.Fatalf("request: %d %s", code, body)
			}
			srv := serve(t, r.h)
			c := r.h.(*testServer).cookie
			out := os.Getenv("GRIMM_BROWSER_OUT")
			if out == "" {
				out = t.TempDir()
			}
			// Play the host: the approved run's launch commands succeed, so the run moves on live.
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go func() {
				for ctx.Err() == nil {
					var n int
					_ = r.d.Owner.QueryRow(ctx, `SELECT count(*) FROM workflow_proposals WHERE started_run_id IS NOT NULL`).Scan(&n)
					if n > 0 {
						break
					}
					time.Sleep(200 * time.Millisecond)
				}
				time.Sleep(2 * time.Second) // let the card show the first status
				for i := 0; ctx.Err() == nil && i < 3; i++ {
					cmds, err := r.d.Owner.Query(ctx, `SELECT id::text, kind FROM commands WHERE status IN ('queued','sent','running') AND kind IN ('create_workspace','create_agent') ORDER BY created_at`)
					if err != nil {
						return
					}
					type pc struct{ id, kind string }
					var pending []pc
					for cmds.Next() {
						var x pc
						_ = cmds.Scan(&x.id, &x.kind)
						pending = append(pending, x)
					}
					cmds.Close()
					for _, x := range pending {
						result := `{"agentId":"run-agent"}`
						if x.kind == hostclient.CreateWorkspaceKind {
							result = `{"workspaceId":"w-run"}`
						}
						_ = r.stream.call(hostclient.Frame{Type: hostclient.CommandUpdateFrame, Update: &hostclient.CommandUpdate{ID: x.id, Status: hostclient.CommandSucceeded, Result: json.RawMessage(result)}})
					}
					time.Sleep(1500 * time.Millisecond)
				}
			}()
			cmd := exec.CommandContext(t.Context(), "node", script, srv.URL+"/grimm/"+r.session, c.Name, c.Value, out, vp.name, strconv.Itoa(vp.width), strconv.Itoa(vp.height))
			cmd.Env = os.Environ()
			b, err := cmd.CombinedOutput()
			fmt.Println(string(b))
			if err != nil {
				t.Fatalf("browser check failed: %v", err)
			}
		})
	}
}

// The edit route edits only a pending, unexpired request of this session, and the review page then
// shows the edited input, not a stale snapshot.
func TestVerifyGrimmEditInputGuards(t *testing.T) {
	t.Parallel()
	r := newWfReqRig(t)
	edit := func(session, p, input string) int {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"input": input})
		return call(t, r.h, "POST", verifyEditPath(session, p), string(b), sameOriginFetch).Code
	}
	request := func(input string) string {
		t.Helper()
		if code, body := r.request(t, "mgr-1", "Run it", input); code/100 != 2 {
			t.Fatalf("request: %d %s", code, body)
		}
		return r.proposals(t)[0]
	}
	inputOf := func(p string) string {
		t.Helper()
		var s string
		if err := r.d.Owner.QueryRow(t.Context(), `SELECT input FROM workflow_proposals WHERE id = $1`, p).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// Edited in place, and the review page shows it.
	p := request("Original review input")
	if c := edit(r.session, p, "Edited review input"); c/100 != 2 {
		t.Fatalf("edit: %d", c)
	}
	cur := r.proposals(t)[0]
	if cur != p {
		t.Logf("the edit replaced the proposal (%s -> %s) rather than editing in place", p, cur)
	}
	if got := inputOf(cur); got != "Edited review input" {
		t.Errorf("input after edit = %q", got)
	}
	page := get(t, r.h, "/workflow-proposals/"+cur, nil).Body.String()
	if !strings.Contains(page, "Edited review input") || strings.Contains(page, "Original review input") {
		t.Error("the review page does not show the edited input (or still shows the original)")
	}

	// Another session of the account cannot edit this session's request.
	rec := call(t, r.h, "POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/app"}`, sameOriginFetch)
	var other struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &other)
	if other.ID == "" {
		t.Fatalf("second session: %d %s", rec.Code, rec.Body)
	}
	if c := edit(other.ID, cur, "edited from another session"); c/100 == 2 {
		t.Errorf("another session edited it: %d", c)
	}
	if got := inputOf(cur); got != "Edited review input" {
		t.Errorf("input after cross-session edit = %q", got)
	}

	// A run proposal not made by Grimm cannot be edited through a session.
	b, _ := json.Marshal(map[string]any{"operation": "run", "workflowId": r.wf, "input": "plain proposal", "summary": "not Grimm's"})
	pr := call(t, anon(r.h), "POST", "/api/workflow-proposals", string(b), map[string]string{"Authorization": "Bearer " + r.key})
	var plain struct{ ID string }
	_ = json.Unmarshal(pr.Body.Bytes(), &plain)
	if pr.Code != 201 {
		t.Fatalf("plain proposal: %d %s", pr.Code, pr.Body)
	}
	if c := edit(r.session, plain.ID, "hijacked"); c/100 == 2 || inputOf(plain.ID) != "plain proposal" {
		t.Errorf("edited a proposal not linked to the session: %d", c)
	}

	// Rejected, expired and withdrawn requests cannot be edited.
	rej := request("to reject")
	if rr := call(t, r.h, "POST", "/api/workflow-proposals/"+rej+"/reject", `{}`, sameOriginFetch); rr.Code/100 != 2 {
		t.Fatalf("reject: %d", rr.Code)
	}
	if c := edit(r.session, rej, "after reject"); c/100 == 2 || inputOf(rej) != "to reject" {
		t.Errorf("edited a rejected request: %d", c)
	}
	exp := request("to expire")
	if _, err := r.d.Owner.Exec(t.Context(), `UPDATE workflow_proposals SET expires_at = now() - interval '1 hour' WHERE id = $1`, exp); err != nil {
		t.Fatal(err)
	}
	if c := edit(r.session, exp, "after expiry"); c/100 == 2 || inputOf(exp) != "to expire" {
		t.Errorf("edited an expired request: %d", c)
	}
	if c := edit(r.session, "00000000-0000-4000-8000-000000000009", "nobody"); c/100 == 2 {
		t.Errorf("edited a missing request: %d", c)
	}

	// Another account can neither edit nor approve it.
	ts := r.h.(*testServer)
	acct, err := ts.store.Create(t.Context(), "otheraccount", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	cookie := sessionFor(t, ts.store, acct)
	if c := call(t, r.h, "POST", verifyEditPath(r.session, cur), `{"input":"other account"}`, sameOriginFetch, cookie).Code; c/100 == 2 || inputOf(cur) != "Edited review input" {
		t.Errorf("another account edited it: %d", c)
	}
	if c := call(t, r.h, "POST", "/api/workflow-proposals/"+cur+"/approve", `{}`, sameOriginFetch, cookie).Code; c/100 == 2 {
		t.Errorf("another account approved it: %d", c)
	}
	if r.runs(t) != 0 {
		t.Fatal("a refused edit or approval started a run")
	}
	// Approval runs the edited input.
	if rr := call(t, r.h, "POST", "/api/workflow-proposals/"+cur+"/approve", `{}`, sameOriginFetch); rr.Code/100 != 2 {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body)
	}
	var input string
	if err := r.d.Owner.QueryRow(t.Context(), `SELECT r.input FROM workflow_proposals p JOIN workflow_runs r ON r.id = p.started_run_id WHERE p.id = $1`, cur).Scan(&input); err != nil || input != "Edited review input" {
		t.Fatalf("run input %q %v", input, err)
	}
}

// grimm_workflow_runs and grimm_workflow_run_status are account-isolated through /api/sql.
func TestVerifyGrimmWorkflowRunsAreAccountIsolated(t *testing.T) {
	t.Parallel()
	r := newWfReqRig(t)
	if code, body := r.request(t, "mgr-1", "Run it", "Isolated thing"); code/100 != 2 {
		t.Fatalf("request: %d %s", code, body)
	}
	ts := r.h.(*testServer)
	acct, err := ts.store.Create(t.Context(), "isolatedother", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	cookie := sessionFor(t, ts.store, acct)
	for _, rel := range []string{"grimm_workflow_runs", "grimm_workflow_run_status"} {
		code, res := sqlPost(t, r.h, q("SELECT count(*) FROM "+rel))
		if code != 200 || len(res.Rows) != 1 || res.Rows[0][0] != float64(1) {
			t.Errorf("owner reads %s: %d %+v", rel, code, res)
		}
		rec := call(t, r.h, "POST", "/api/sql", q("SELECT count(*) FROM "+rel), nil, cookie)
		var other sqlResult
		_ = json.Unmarshal(rec.Body.Bytes(), &other)
		if rec.Code != 200 || len(other.Rows) != 1 || other.Rows[0][0] != float64(0) {
			t.Errorf("another account reads %s: %d %s", rel, rec.Code, rec.Body)
		}
		if n := r.count(t, `SELECT count(*) FROM pg_views WHERE schemaname = 'public' AND viewname = $1`, rel); n == 1 {
			var opts []string
			_ = r.d.Owner.QueryRow(t.Context(), `SELECT coalesce(reloptions, '{}') FROM pg_class WHERE relname = $1`, rel).Scan(&opts)
			if !strings.Contains(strings.Join(opts, ","), "security_invoker=true") {
				t.Errorf("%s is a view without security_invoker: %v", rel, opts)
			}
		}
	}
	if err := db.CheckContract(t.Context(), r.d.Owner, r.d.Readonly); err != nil {
		t.Fatal(err)
	}
}
