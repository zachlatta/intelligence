package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestModelGroupsStableTokensAndIsolation(t *testing.T) {
	h, _ := dbServer(t)
	rec := call(t, h, http.MethodPost, "/api/model-groups", `{"name":"Frontier Models","members":[]}`, sameOriginFetch)
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var g struct {
		ID       string
		Token    string
		Revision int
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
		t.Fatal(err)
	}
	if g.Token != "frontier_models_model" {
		t.Fatalf("token: %+v", g)
	}
	rec = call(t, h, http.MethodPut, "/api/model-groups/"+g.ID, `{"name":"Best","revision":1,"members":[]}`, sameOriginFetch)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), g.Token) {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body)
	}
	rec = call(t, h, http.MethodPut, "/api/model-groups/"+g.ID, `{"name":"Stale","revision":1,"members":[]}`, sameOriginFetch)
	if rec.Code != 409 {
		t.Fatalf("stale edit: %d %s", rec.Code, rec.Body)
	}
	code, r := sqlPost(t, h, q(`SELECT token, name FROM model_groups`))
	if code != 200 || len(r.Rows) != 1 || r.Rows[0][1] != "Best" {
		t.Fatalf("SQL: %d %+v", code, r)
	}
	rec = call(t, anon(h), http.MethodPost, "/api/model-groups", `{"name":"Other"}`, sameOriginFetch)
	if rec.Code != 401 {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	for _, path := range []string{"/usage", "/workflows"} {
		page := get(t, h, path, nil)
		if !strings.Contains(page.Body.String(), "/model-groups.js") {
			t.Fatalf("missing editor on %s", path)
		}
	}
	rec = call(t, h, "POST", "/api/model-groups", `{"name":"Frontier Models","members":[]}`, sameOriginFetch)
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), "frontier_models_2_model") {
		t.Fatalf("token collision: %d %s", rec.Code, rec.Body)
	}
	code, _ = sqlPost(t, h, q(`UPDATE model_groups SET name='write'`))
	if code != 400 {
		t.Fatalf("SQL could write groups: %d", code)
	}
}

func TestModelGroupPageMarkup(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	wf := createWorkflow(t, h, workflow.Starting)
	for _, path := range []string{"/usage", "/workflows", "/workflows/" + wf} {
		page := get(t, h, path, nil).Body.String()
		for _, markup := range []string{`id="model-groups"`, `id="model-group-editor"`, `id="model-group-member"`} {
			if !strings.Contains(page, markup) {
				t.Errorf("%s lacks server-rendered %s", path, markup)
			}
		}
		if !strings.Contains(page, "data-new disabled") {
			t.Errorf("%s allows creating a group before loading", path)
		}
	}
}

func TestModelGroupInitialLoadGate(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	out, err := exec.Command(node, "-e", `
const assert=require('assert'),vm=require('vm'),fs=require('fs');
const script=fs.readFileSync(process.argv[1],'utf8');
async function scenario(sheet, fail) {
 const create={disabled:false}, host={replaceChildren(){}}, cards={replaceChildren(){}}, pending=[];
 const manager=sheet?{open:false,showModal(){this.open=true;}}:null;
 const root={querySelector:s=>s==='[data-new]'?create:s==='[data-host]'?host:cards,querySelectorAll:()=>[create],scrollIntoView(){}};
 const document={querySelector:s=>s==='#model-groups'?root:s==='#model-group-manager'?manager:{},querySelectorAll:()=>[],dispatchEvent(){}};
 const window={};
 const context=vm.createContext({document,window,fetch:()=>new Promise(resolve=>pending.push(resolve)),Event:class {}});
 vm.runInContext(script,context);
 assert(create.disabled,'create must stay disabled while initial SQL is pending');
 const opened=window.modelGroups.open();
 assert.equal(pending.length,4,'opening the manager must share the in-flight initial load');
 if(manager)assert(!manager.open,'sheet must wait for the catalog');
 pending.forEach(resolve=>resolve({ok:!fail,json:async()=>fail?{error:'unavailable'}:{columns:[],rows:[]}}));
 await opened;await new Promise(resolve=>setImmediate(resolve));
 assert.equal(create.disabled,fail,'only a successful complete load enables creation');
 if(manager)assert.equal(manager.open,!fail);
}
(async()=>{await scenario(false,false);await scenario(true,false);await scenario(false,true);await scenario(true,true);})().catch(e=>{console.error(e);process.exitCode=1;});
`, "model-groups.js").CombinedOutput()
	if err != nil {
		t.Fatalf("initial model group load: %v\n%s", err, out)
	}
}

func TestModelGroupClientHelpers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	out, err := exec.Command(node, "-e", `
const assert=require('assert'),vm=require('vm'),fs=require('fs');
global.document={querySelector:()=>null};
vm.runInThisContext(fs.readFileSync(process.argv[1],'utf8'));
const windows=[
 {provider:'p',account:'one',window_id:'capacity/cycle',window_seconds:1000},
 {provider:'p',account:'one',window_id:'capacity/burst',window_seconds:50},
 {provider:'p',account:'one',window_id:'other/cycle',window_seconds:800},
 {provider:'p',account:'two',window_id:'private/cycle',window_seconds:9000}];
assert.deepEqual(modelGroupPools(windows,'p','one'),['capacity/cycle','other/cycle']);
assert.deepEqual(modelGroupPools([], 'p','one'),[]);
assert.deepEqual(modelGroupPools([{provider:'p',account:'one',window_id:'unknown',window_seconds:null}],'p','one'),[]);
`, "model-groups.js").CombinedOutput()
	if err != nil {
		t.Fatalf("model group pools: %v\n%s", err, out)
	}
	// Exercise the inspector's own renderer using the same plain-node approach as other UI tests.
	_, script, ok := strings.Cut(workflowsHTML, "function modelTokenChips(")
	if !ok {
		t.Fatal("missing inspector renderer")
	}
	script, _, ok = strings.Cut(script, "\n}")
	if !ok {
		t.Fatal("missing renderer end")
	}
	out, err = exec.Command(node, "-e", `const assert=require('assert');const esc=s=>String(s).replaceAll('&','&amp;').replaceAll('"','&quot;').replaceAll('<','&lt;');const V=s=>'{{'+s+'}}';`+"function modelTokenChips("+script+"\n}\n"+`
const html=modelTokenChips([{name:'Frontier',token:'frontier_model'},{name:'Fast <checks>',token:'fast_checks_model'}]);
assert(html.includes('data-var="{{frontier_model}}"'));
assert(html.includes('data-var="{{fast_checks_model}}"'));
assert(html.includes('Fast &lt;checks>'));
assert(modelTokenChips([]).includes('Edit groups'));
`).CombinedOutput()
	if err != nil {
		t.Fatalf("model token chips: %v\n%s", err, out)
	}
}

func TestWorkflowHostAPIErrorsAreJSON(t *testing.T) {
	h, _ := dbServer(t)
	for _, action := range []string{"moves", "suggestions"} {
		for _, body := range []string{`{}`, `not-json`} {
			rec := call(t, anon(h), "POST", "/api/workflow-runs/00000000-0000-4000-8000-000000000000/"+action, body, nil)
			var out struct{ Message string }
			if rec.Code != 401 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Message == "" {
				t.Errorf("%s auth: %d %s", action, rec.Code, rec.Body)
			}
			key := keyOf(t, h, "host-1")
			rec = call(t, anon(h), "POST", "/api/workflow-runs/00000000-0000-4000-8000-000000000000/"+action, body, map[string]string{"Authorization": "Bearer " + key})
			out.Message = ""
			want := 404
			if body == "not-json" {
				want = 400
			}
			if rec.Code != want || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Message == "" {
				t.Errorf("%s request: %d %s", action, rec.Code, rec.Body)
			}
		}
	}
}

func TestModelGroupUsesObservedQuotaPools(t *testing.T) {
	h, d := dbServer(t)
	rep := wfReport()
	rep.Paseo.Providers = []hostclient.Provider{{Name: "antigravity", Available: true, Models: []hostclient.Model{{ID: "new-model", Thinking: []string{"high"}}}}}
	sendReport(t, h, rep)
	for _, query := range []string{
		`INSERT INTO usage_accounts(provider,account,first_seen,last_seen) VALUES('antigravity','default',now(),now())`,
		`INSERT INTO usage_sources(host,provider,account,ok,received_at) VALUES('host-1','antigravity','default',true,now())`,
		`INSERT INTO usage_readings(provider,account,window_id,window_seconds,used_pct,first_observed,last_observed) VALUES('antigravity','default','new-pool/month',2592000,0,now(),now()),('antigravity','default','new-pool/day',86400,0,now(),now()),('antigravity','default','other/month',2592000,0,now(),now())`,
	} {
		if _, err := d.Owner.Exec(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	request := func(long string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]any{"name": long, "members": []any{map[string]any{"provider": "antigravity", "model": "new-model", "thinking": "high", "login": "default", "longWindow": long, "enabled": true}}})
		return call(t, h, "POST", "/api/model-groups", string(b), sameOriginFetch)
	}
	if rec := request("new-pool/month"); rec.Code != 201 {
		t.Fatalf("observed pool rejected: %d %s", rec.Code, rec.Body)
	}
	for _, long := range []string{"new-pool/day", "invented/month"} {
		if rec := request(long); rec.Code != 400 {
			t.Errorf("invalid pool %s: %d %s", long, rec.Code, rec.Body)
		}
	}
}

// Members keep only what the editor chooses: the provider is the plan's provider, and shorter
// windows in a pool are read live from usage, so neither is stored a second time.
func TestModelGroupSchemaStoresNoDerivedCopies(t *testing.T) {
	_, d := dbServer(t)
	var cols []string
	rows, err := d.Owner.Query(t.Context(), `SELECT table_name||'.'||column_name FROM information_schema.columns
		WHERE table_schema='public' AND column_name IN ('usage_provider','gate_windows','suggestion_id') ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	if strings.Join(cols, " ") != "workflow_run_events.suggestion_id" {
		t.Errorf("columns: %v", cols)
	}
}

func TestModelGroupNames(t *testing.T) {
	h, _ := dbServer(t)
	body := func(name string, revision int) string {
		b, _ := json.Marshal(map[string]any{"name": name, "revision": revision, "members": []any{}})
		return string(b)
	}
	for _, name := range []string{"", strings.Repeat("a", 300)} {
		rec := call(t, h, "POST", "/api/model-groups", body(name, 0), sameOriginFetch)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "a model group needs a name") || strings.Contains(rec.Body.String(), "workflow") {
			t.Errorf("group name validation: %d %s", rec.Code, rec.Body)
		}
	}
	rec := call(t, h, "POST", "/api/model-groups", body("Frontier", 0), sameOriginFetch)
	if rec.Code != 201 {
		t.Fatal(rec.Body)
	}
	var group workflow.ModelGroup
	json.Unmarshal(rec.Body.Bytes(), &group)
	for _, name := range []string{"Frontier", "frontier", " FRONTIER "} {
		rec = call(t, h, "POST", "/api/model-groups", body(name, 0), sameOriginFetch)
		if rec.Code != 409 || !strings.Contains(rec.Body.String(), "already exists") {
			t.Errorf("duplicate %q: %d %s", name, rec.Code, rec.Body)
		}
	}
	rec = call(t, h, "POST", "/api/model-groups", body("Fast", 0), sameOriginFetch)
	if rec.Code != 201 {
		t.Fatal(rec.Body)
	}
	var fast workflow.ModelGroup
	json.Unmarshal(rec.Body.Bytes(), &fast)
	rec = call(t, h, "PUT", "/api/model-groups/"+fast.ID, body("FRONTIER", 1), sameOriginFetch)
	if rec.Code != 409 {
		t.Errorf("duplicate rename: %d %s", rec.Code, rec.Body)
	}
	rec = call(t, h, "PUT", "/api/model-groups/"+group.ID, body("frontier", 1), sameOriginFetch)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"token":"frontier_model"`) {
		t.Errorf("self rename: %d %s", rec.Code, rec.Body)
	}
	// Names are scoped to the app account, and a deleted name may be used again.
	ts := h.(*testServer)
	other, err := ts.store.Create(t.Context(), "group-names", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	rec = call(t, h, "POST", "/api/model-groups", body("Frontier", 0), sameOriginFetch, sessionFor(t, ts.store, other))
	if rec.Code != 201 {
		t.Errorf("other account: %d %s", rec.Code, rec.Body)
	}
	rec = call(t, h, "DELETE", "/api/model-groups/"+group.ID, "", sameOriginFetch)
	if rec.Code != 204 {
		t.Fatal(rec.Body)
	}
	rec = call(t, h, "POST", "/api/model-groups", body("Frontier", 0), sameOriginFetch)
	if rec.Code != 201 {
		t.Errorf("reuse deleted name: %d %s", rec.Code, rec.Body)
	}
}

func TestModelGroupCanConfigureUnknownMeters(t *testing.T) {
	h, d := dbServer(t)
	rep := wfReport()
	rep.Paseo.Providers = []hostclient.Provider{{Name: "muse", Available: true, Models: []hostclient.Model{{ID: "wonder", Thinking: []string{"high"}}}}}
	sendReport(t, h, rep)
	for _, q := range []string{
		`INSERT INTO usage_accounts(provider,account,first_seen,last_seen) VALUES('muse','default',now(),now())`,
		`INSERT INTO usage_sources(host,provider,account,ok,received_at) VALUES('host-1','muse','default',true,now())`,
		`INSERT INTO usage_active(host,provider,account,since,until) VALUES('host-1','muse','default',now(),now())`,
	} {
		if _, err := d.Owner.Exec(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	rec := call(t, h, "POST", "/api/model-groups", `{"name":"Unknown capacity","members":[{"provider":"muse","model":"wonder","thinking":"high","login":"default","longWindow":"","enabled":true}]}`, sameOriginFetch)
	if rec.Code != 400 {
		t.Fatalf("unobserved quota enabled: %d %s", rec.Code, rec.Body)
	}
	rec = call(t, h, "POST", "/api/model-groups", `{"name":"Unknown capacity","members":[{"provider":"muse","model":"wonder","thinking":"high","login":"default","longWindow":"","enabled":false}]}`, sameOriginFetch)
	if rec.Code != 201 {
		t.Fatalf("configure unknown meter: %d %s", rec.Code, rec.Body)
	}
	code, r := sqlPost(t, h, q(`SELECT eligible,reason FROM model_group_candidates`))
	if code != 200 || len(r.Rows) != 1 || r.Rows[0][0] != false || r.Rows[0][1] != "Disabled" {
		t.Fatalf("unknown was capacity: %+v", r)
	}
	// Older saved entries could name guessed windows. Disabling them must remain possible.
	rec = call(t, h, "POST", "/api/model-groups", `{"name":"Legacy unknown","members":[{"provider":"muse","model":"wonder","thinking":"high","login":"default","longWindow":"old-pool/week","enabled":false}]}`, sameOriginFetch)
	var legacy workflow.ModelGroup
	if rec.Code != 201 || json.Unmarshal(rec.Body.Bytes(), &legacy) != nil || len(legacy.Members) != 1 || legacy.Members[0].LongWindow != "" {
		t.Fatalf("disabled unknown mapping: %d %s", rec.Code, rec.Body)
	}
	rec = call(t, h, "POST", "/api/model-groups", `{"name":"Missing effort","members":[{"provider":"muse","model":"wonder","thinking":"","login":"default","longWindow":"7d","enabled":true}]}`, sameOriginFetch)
	if rec.Code != 400 {
		t.Fatalf("implicit thinking default accepted: %d %s", rec.Code, rec.Body)
	}
}

func TestModelGroupRejectsBlankLoginAndHidesDatabaseErrors(t *testing.T) {
	h, d := dbServer(t)
	rep := wfReport()
	rep.Paseo.Providers = []hostclient.Provider{{Name: "codex", Available: true, Models: []hostclient.Model{{ID: "sol", Thinking: []string{"high"}}}}}
	sendReport(t, h, rep)
	// Provider-level source rows deliberately use an empty account; they are not plan logins.
	for _, query := range []string{
		`INSERT INTO usage_sources(host,provider,account,ok,received_at) VALUES('host-1','codex','',true,now())`,
		`INSERT INTO usage_accounts(provider,account,first_seen,last_seen) VALUES('codex','one',now(),now())`,
		`INSERT INTO usage_sources(host,provider,account,ok,received_at) VALUES('host-1','codex','one',true,now())`,
		`INSERT INTO usage_readings(provider,account,window_id,window_seconds,used_pct,first_observed,last_observed) VALUES('codex','one','7d',604800,0,now(),now())`,
	} {
		if _, err := d.Owner.Exec(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	body := func(login string) string {
		b, _ := json.Marshal(map[string]any{"name": "Frontier", "members": []any{map[string]any{"provider": "codex", "model": "sol", "thinking": "high", "login": login, "longWindow": "7d", "enabled": true}}})
		return string(b)
	}
	for _, login := range []string{"", "   "} {
		rec := call(t, h, "POST", "/api/model-groups", body(login), sameOriginFetch)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Choose a plan/login") {
			t.Fatalf("blank login: %d %s", rec.Code, rec.Body)
		}
	}
	// A broken writer must return a safe error, not a relation name or SQLSTATE.
	if _, err := d.Owner.Exec(t.Context(), `DROP TABLE model_group_members CASCADE`); err != nil {
		t.Fatal(err)
	}
	rec := call(t, h, "POST", "/api/model-groups", body("one"), sameOriginFetch)
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "SQLSTATE") || strings.Contains(rec.Body.String(), "model_group_members") {
		t.Fatalf("database details leaked: %d %s", rec.Code, rec.Body)
	}
}

func TestModelGroupRankingAndWorkflowExpansion(t *testing.T) {
	h, d := dbServer(t)
	rep := wfReport()
	rep.Paseo.Providers = []hostclient.Provider{
		{Name: "claude", Available: true, Models: []hostclient.Model{{ID: "opus", Label: "Claude Opus 5.5", Thinking: []string{"medium"}}}},
		{Name: "codex", Available: true, Models: []hostclient.Model{{ID: "sol", Thinking: []string{"high"}}}},
	}
	sendReport(t, h, rep)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Owner.Exec(t.Context(), q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"claude", "codex"} {
		for _, login := range []string{"one", "two"} {
			exec(`INSERT INTO usage_accounts(provider,account,first_seen,last_seen) VALUES($1,$2,now(),now())`, p, login)
			exec(`INSERT INTO usage_sources(host,provider,account,ok,received_at) VALUES('host-1',$1,$2,true,now())`, p, login)
			exec(`INSERT INTO usage_readings(provider,account,window_id,window_seconds,used_pct,resets_at,first_observed,last_observed) VALUES($1,$2,'7d',604800,30,now()+interval '1 day',now(),now())`, p, login)
		}
		exec(`INSERT INTO usage_active(host,provider,account,since,until) VALUES('host-1',$1,'one',now(),now())`, p)
	}
	member := func(p, model, think, login string) map[string]any {
		return map[string]any{"provider": p, "model": model, "thinking": think, "login": login, "longWindow": "7d", "enabled": true}
	}
	body, _ := json.Marshal(map[string]any{"name": "Frontier", "members": []any{member("claude", "opus", "medium", "one"), member("claude", "opus", "medium", "two"), member("codex", "sol", "high", "one")}})
	rec := call(t, h, "POST", "/api/model-groups", string(body), sameOriginFetch)
	if rec.Code != 201 {
		t.Fatalf("create %d %s", rec.Code, rec.Body)
	}
	var g workflow.ModelGroup
	json.Unmarshal(rec.Body.Bytes(), &g)
	code, r := sqlPost(t, h, q(`SELECT login,reason FROM model_group_candidates WHERE provider='claude' ORDER BY login`))
	if code != 200 || len(r.Rows) != 2 || r.Rows[1][1] != "Inactive login" {
		t.Fatalf("separate logins: %d %+v", code, r)
	}
	def := workflow.Starting
	def.Steps = append([]workflow.Step(nil), def.Steps...)
	def.Project = workflow.Project{Mode: "fixed", Directory: "/w/app"}
	def.Steps[0].Instructions = "Launch the builder with {{frontier_model}} and a second opinion with {{frontier_model}}. Task: {{input}}"
	def.Steps[1].Instructions = "Check using {{frontier_model}}."
	wf := createWorkflow(t, h, def)
	stream := openStream(t, h, "host-1")
	run := startRun(t, h, wf, "Literal {{frontier_model}}")
	cmd := stream.command()
	var ws hostclient.CreateWorkspaceArgs
	json.Unmarshal(cmd.Args, &ws)
	if strings.Contains(ws.Agent.Prompt, "This step's model") {
		t.Fatalf("prompt retains per-step model directive: %s", ws.Agent.Prompt)
	}
	inline := "Claude Opus 5.5 (Medium) via claude/opus --thinking medium on one"
	if !strings.Contains(ws.Agent.Prompt, "Launch the builder with "+inline+" and a second opinion with "+inline+".") {
		t.Fatalf("model expansion interrupts sentence: %s", ws.Agent.Prompt)
	}
	if strings.Count(ws.Agent.Prompt, "\nModel suggestions:\n") != 1 || strings.Count(ws.Agent.Prompt, "- frontier_model:") != 1 {
		t.Fatalf("want one appended note per group: %s", ws.Agent.Prompt)
	}
	page := get(t, h, "/workflow-runs/"+run, nil).Body.String()
	if !strings.Contains(page, "Suggested Claude Opus 5.5 (Medium) on one for {{frontier_model}}") || strings.Contains(page, "model_suggestion ") || strings.Contains(page, "Before another worker") || strings.Contains(page, "Launch settings:") {
		t.Fatalf("run page exposes worker instructions: %s", page)
	}
	if !strings.Contains(ws.Agent.Prompt, "--provider claude/opus --mode bypassPermissions --thinking medium") || !strings.Contains(ws.Agent.Prompt, "Literal {{frontier_model}}") {
		t.Fatalf("prompt: %s", ws.Agent.Prompt)
	}
	code, r = sqlPost(t, h, q(`SELECT count(*) FROM workflow_model_suggestions`))
	if code != 200 || r.Rows[0][0] != float64(1) {
		t.Fatalf("duplicate token allocations: %+v", r)
	}
	// The event links its suggestion by id, so its text need not carry it.
	code, r = sqlPost(t, h, q(`SELECT count(*) FROM workflow_run_events e JOIN workflow_model_suggestions s ON s.id=e.suggestion_id
		WHERE e.kind='model_suggestion' AND e.text NOT LIKE '%Suggestion %'`))
	if code != 200 || r.Rows[0][0] != float64(1) {
		t.Fatalf("suggestion events not linked by id: %d %+v", code, r)
	}
	// The first allocation lowers the shared Claude pool score, so the next goes to Codex.
	succeed(t, stream, cmd, `{"workspaceId":"work","agentId":"orch"}`)
	key := keyOf(t, h, "host-1")
	status, msg := move(t, h, key, run, map[string]string{"move": "next", "summary": "built"})
	if status != 200 || !strings.Contains(msg, "--provider codex/sol --mode full-access --thinking high") {
		t.Fatalf("next: %d %s", status, msg)
	}
	rec = call(t, h, "DELETE", "/api/model-groups/"+g.ID, "", sameOriginFetch)
	if rec.Code != 409 {
		t.Fatalf("delete referenced group: %d %s", rec.Code, rec.Body)
	}
	// Every model in the same quota pool sees the allocation, across groups too.
	body, _ = json.Marshal(map[string]any{"name": "Other", "members": []any{member("claude", "opus", "medium", "one")}})
	rec = call(t, h, "POST", "/api/model-groups", string(body), sameOriginFetch)
	if rec.Code != 201 {
		t.Fatal(rec.Body)
	}
	code, r = sqlPost(t, h, q(`SELECT count(DISTINCT score) FROM model_group_candidates WHERE provider='claude' AND login='one'`))
	if code != 200 || r.Rows[0][0] != float64(1) {
		t.Fatalf("pool duplication: %+v", r)
	}
	// A shared short window is mandatory even if the editor omitted it.
	exec(`INSERT INTO usage_readings(provider,account,window_id,window_seconds,used_pct,resets_at,first_observed,last_observed)
	 VALUES('claude','one','5h',18000,100,now()+interval '1 hour',now(),now())`)
	code, r = sqlPost(t, h, q(`SELECT count(*) FROM model_group_candidates WHERE provider='claude' AND login='one' AND eligible`))
	if code != 200 || r.Rows[0][0] != float64(0) {
		t.Fatalf("short window bypassed: %+v", r)
	}
	// All capacity unknown: reset without another reading does not imply a full plan.
	exec(`UPDATE usage_readings SET resets_at=now()-interval '1 minute'`)
	code, r = sqlPost(t, h, q(`SELECT count(*) FROM model_group_candidates WHERE eligible`))
	if code != 200 || r.Rows[0][0] != float64(0) {
		t.Fatalf("expired windows eligible: %+v", r)
	}
	rec = call(t, anon(h), "POST", "/api/workflow-runs/"+run+"/suggestions", `{"token":"frontier_model"}`, map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "No eligible model") {
		t.Fatalf("wait: %d %s", rec.Code, rec.Body)
	}
	// Another account cannot read or mutate group state or use its token.
	ts := h.(*testServer)
	bob, err := ts.store.Create(t.Context(), "group-reader", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	cookie := sessionFor(t, ts.store, bob)
	rec = call(t, h, "GET", "/api/sql?q=SELECT+token+FROM+model_groups", "", nil, cookie)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "frontier_model") {
		t.Fatalf("group leak: %d %s", rec.Code, rec.Body)
	}
	rec = call(t, h, "PUT", "/api/model-groups/"+g.ID, `{"name":"Stolen","revision":1,"members":[]}`, sameOriginFetch, cookie)
	if rec.Code != 404 {
		t.Fatalf("cross-account write: %d %s", rec.Code, rec.Body)
	}
	for _, tc := range []struct{ change, reason string }{
		{`UPDATE usage_readings SET resets_at=now()+interval '1 day',last_observed=now()-interval '16 minutes'`, "Meter unknown or stale"},
		{`UPDATE usage_readings SET last_observed=now(),used_pct=100`, "Quota reserve"},
		{`UPDATE model_group_members SET eligible_until=now()-interval '1 minute'`, "Plan ended"},
		{`UPDATE model_group_members SET enabled=false`, "Disabled"},
	} {
		exec(tc.change)
		code, r = sqlPost(t, h, q(`SELECT eligible,reason FROM model_group_candidates WHERE provider='codex'`))
		if code != 200 || len(r.Rows) != 1 || r.Rows[0][0] != false || r.Rows[0][1] != tc.reason {
			t.Fatalf("eligibility %s: %+v", tc.reason, r)
		}
	}
}

// model-groups.js is served like the other embedded scripts: revalidated by an ETag of its content.
func TestModelGroupsScriptIsServed(t *testing.T) {
	rec := get(t, dictationServer(t, "", ""), "/model-groups.js", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") ||
		!strings.Contains(rec.Body.String(), "model-groups") || rec.Header().Get("ETag") == "" || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("/model-groups.js: %d %q, etag %q", rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("ETag"))
	}
}

func TestWorkflowAPIRejectsStepModelFields(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	def := workflow.Starting
	def.Host = "host-1"
	body, _ := json.Marshal(map[string]any{"name": "Removed model", "definition": def})
	for _, field := range []string{`"model":"{{frontier_model}}"`, `"thinking":"high"`} {
		raw := strings.Replace(string(body), `"steps":[{`, `"steps":[{`+field+",", 1)
		rec := call(t, h, "POST", "/api/workflows", raw, sameOriginFetch)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "unknown field") {
			t.Fatalf("removed field %s: %d %s", field, rec.Code, rec.Body)
		}
	}
	for _, removed := range []string{"data-step-model", "data-step-thinking", "stepModelLabel"} {
		if strings.Contains(workflowsHTML, removed) {
			t.Errorf("editor retains %s", removed)
		}
	}
}
