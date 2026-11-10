package web

import (
	"encoding/json"
	"mime"
	"net/http"

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
	var body struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := a.wf.Decide(r.Context(), v.acct.ID, by, r.PathValue("id"), "withdraw", body.Reason); err != nil {
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
