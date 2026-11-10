package web

import (
	"encoding/json"
	"errors"
	"html/template"
	"mime"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/zachlatta/intelligence/internal/accounts"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// Agents authenticate with a host key. Browser decisions use write, which accepts
// only a session cookie. A bearer token is never converted into a browser session.
func (a *app) proposalAgent(w http.ResponseWriter, r *http.Request) (viewer, string, bool) {
	key, _ := bearerOf(r)
	host, err := a.store.HostOfKey(r.Context(), key)
	if err != nil {
		accountError(w, err)
		return viewer{}, "", false
	}
	if host == "" {
		http.Error(w, "bad or missing host key", 401)
		return viewer{}, "", false
	}
	var acct accounts.Account
	err = a.cfg.DB.QueryRow(r.Context(), `SELECT a.id,a.role_name FROM accounts a JOIN host_keys k ON k.account_id=a.id WHERE k.host=$1`, host).Scan(&acct.ID, &acct.Role)
	if err != nil {
		accountError(w, err)
		return viewer{}, "", false
	}
	pool, err := a.store.Reader(r.Context(), acct)
	if err != nil {
		accountError(w, err)
		return viewer{}, "", false
	}
	return viewer{acct: acct, pool: pool}, "host:" + host, true
}

func (a *app) proposalURL(r *http.Request, id string) string {
	return a.baseURL(r) + "/workflow-proposals/" + id
}

func (a *app) proposeWorkflow(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", 403)
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "send JSON", http.StatusUnsupportedMediaType)
		return
	}
	v, by, ok := a.proposalAgent(w, r)
	if !ok {
		return
	}
	var req workflow.ProposalRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id, err := a.wf.Propose(r.Context(), v.acct.ID, by, req)
	if err != nil {
		workflowError(w, err)
		return
	}
	url := a.proposalURL(r, id)
	w.Header().Set("Location", url)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "approvalURL": url})
}

func (a *app) withdrawProposal(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", 403)
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "send JSON", http.StatusUnsupportedMediaType)
		return
	}
	v, by, ok := a.proposalAgent(w, r)
	if !ok {
		return
	}
	var body struct{}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := a.wf.Decide(r.Context(), v.acct.ID, by, r.PathValue("id"), "withdraw"); err != nil {
		workflowError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "withdrawn"})
}

func (a *app) decideProposal(action string) func(viewer) http.HandlerFunc {
	return func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct{}
			if !decodeJSON(w, r, &body) {
				return
			}
			if err := a.wf.Decide(r.Context(), v.acct.ID, "", r.PathValue("id"), action); err != nil {
				workflowError(w, err)
				return
			}
			writeJSON(w, map[string]string{"decision": action})
		}
	}
}

type proposalView struct {
	ID, Summary, Operation, Proposer, Status, Before, After, Expiry string
	Pending, Applied                                                bool
}

func proposalPage(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !workflowIDRe.MatchString(id) {
			http.NotFound(w, r)
			return
		}
		var p proposalView
		var before, after []byte
		err := v.pool.QueryRow(r.Context(), `SELECT id::text,summary,operation,proposer,
		 effective_status,
 before_state,proposed_state,expires_at::text FROM workflow_proposal_status WHERE id=$1`, id).Scan(&p.ID, &p.Summary, &p.Operation, &p.Proposer, &p.Status, &before, &after, &p.Expiry)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		pretty := func(b []byte) string {
			var x any
			_ = json.Unmarshal(b, &x)
			formatted, _ := json.MarshalIndent(x, "", "  ")
			return string(formatted)
		}
		// Show canonical snapshots; deletion keeps a tombstone and undo restores the prior state.
		p.Before = pretty(before)
		p.After = pretty(after)
		p.Pending = p.Status == "pending"
		p.Applied = p.Status == "applied"
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = proposalTmpl.Execute(w, p)
	}
}

var proposalTmpl = template.Must(template.New("proposal").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Review workflow proposal</title><link rel="stylesheet" href="/ui.css">
<main class="container"><p><a href="/workflow-proposals">All proposals</a> · <a href="/workflows">Workflows</a></p>
<h1>{{.Summary}}</h1><p>{{.Operation}} · {{.Status}} · proposed by {{.Proposer}}</p><p>Expires {{.Expiry}}</p>
<p>Review the complete workflow below. Red lines are removed; green lines are added. Approval applies this exact change. Later edits prevent approval or undo.</p>
<div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(280px,1fr));gap:1rem"><section><h2>Before</h2><pre style="white-space:pre-wrap;overflow-wrap:anywhere">{{.Before}}</pre></section><section><h2>After</h2><pre style="white-space:pre-wrap;overflow-wrap:anywhere">{{.After}}</pre></section></div>
<h2>Changes</h2><pre id="diff" style="white-space:pre-wrap;overflow-wrap:anywhere"></pre>
{{if .Pending}}<button data-action="approve">Approve change</button> <button data-action="reject">Reject</button>{{end}}
{{if .Applied}}<button data-action="undo">Undo change</button>{{end}}<p id="error" role="alert"></p></main>
<script>
const blocks=document.querySelectorAll('section pre'), a=blocks[0].textContent.split('\n'),b=blocks[1].textContent.split('\n');
// Match equal lines in order. The complete snapshots above remain the review authority.
const diff=document.querySelector('#diff');function line(s,color){const span=document.createElement('span');span.textContent=s+'\n';span.style.color=color;diff.append(span);}
let start=0,end=0;while(start<a.length&&start<b.length&&a[start]===b[start])start++;while(end<a.length-start&&end<b.length-start&&a[a.length-1-end]===b[b.length-1-end])end++;
for(let i=0;i<start;i++)line('  '+a[i],'inherit');for(let i=start;i<a.length-end;i++)line('- '+a[i],'#b42318');for(let i=start;i<b.length-end;i++)line('+ '+b[i],'#067647');for(let i=a.length-end;i<a.length;i++)line('  '+a[i],'inherit');
document.querySelectorAll('[data-action]').forEach(button=>button.onclick=async()=>{
document.querySelectorAll('button').forEach(b=>b.disabled=true);
try{const res=await fetch('/api/workflow-proposals/{{.ID}}/'+button.dataset.action,{method:'POST',headers:{'Content-Type':'application/json'},body:'{}'});if(!res.ok)throw new Error(await res.text());location.reload();}catch(e){document.querySelector('#error').textContent=e.message;document.querySelectorAll('button').forEach(b=>b.disabled=false);}});
</script></html>`))

func proposalsPage(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := v.pool.Query(r.Context(), `SELECT id::text,summary,operation,proposer,effective_status FROM workflow_proposal_status ORDER BY created_at DESC LIMIT 200`)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		ps, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (proposalView, error) {
			var p proposalView
			err := row.Scan(&p.ID, &p.Summary, &p.Operation, &p.Proposer, &p.Status)
			return p, err
		})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = proposalsTmpl.Execute(w, ps)
	}
}

var proposalsTmpl = template.Must(template.New("proposals").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Workflow proposals</title><link rel="stylesheet" href="/ui.css"><main class="container"><p><a href="/workflows">Workflows</a></p><h1>Workflow proposals</h1>{{range .}}<p><a href="/workflow-proposals/{{.ID}}">{{.Summary}}</a> · {{.Operation}} · {{.Status}} · {{.Proposer}}</p>{{else}}<p>No proposals yet.</p>{{end}}</main></html>`))
