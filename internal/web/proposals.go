package web

import (
	"encoding/json"
	"net/http"

	"github.com/zachlatta/intelligence/internal/accounts"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func (a *app) proposalURL(r *http.Request, id string) string {
	return a.baseURL(r) + "/workflow-proposals/" + id
}

func (a *app) proposeWorkflow(acct accounts.Account, host string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req workflow.ProposalRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		id, err := a.wf.Propose(r.Context(), acct.ID, "host:"+host, req)
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
}

func (a *app) withdrawProposal(acct accounts.Account, host string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Reason string `json:"reason"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if err := a.wf.Withdraw(r.Context(), acct.ID, "host:"+host, r.PathValue("id"), body.Reason); err != nil {
			workflowError(w, err)
			return
		}
		writeJSON(w, map[string]string{"status": "withdrawn"})
	}
}

func (a *app) decideProposal(action string) func(viewer) http.HandlerFunc {
	return func(v viewer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct{}
			if !decodeJSON(w, r, &body) {
				return
			}
			if err := a.wf.Decide(r.Context(), v.acct.ID, r.PathValue("id"), action); err != nil {
				workflowError(w, err)
				return
			}
			writeJSON(w, map[string]string{"decision": action})
		}
	}
}
