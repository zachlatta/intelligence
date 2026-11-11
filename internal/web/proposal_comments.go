package web

import (
	"net/http"

	"github.com/zachlatta/intelligence/internal/accounts"
)

func (a *app) commentProposal(account int64, author string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if err := a.wf.Comment(r.Context(), account, r.PathValue("id"), author, body.Text); err != nil {
			workflowError(w, err)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}
}

func (a *app) reviewerComment(v viewer) http.HandlerFunc {
	return a.commentProposal(v.acct.ID, "reviewer")
}
func (a *app) agentComment(acct accounts.Account, host string) http.HandlerFunc {
	return a.commentProposal(acct.ID, "host:"+host)
}
