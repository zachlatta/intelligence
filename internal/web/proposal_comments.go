package web

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/zachlatta/intelligence/internal/accounts"
)

func (a *app) commentProposal(account int64, author string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text       string `json:"text"`
			QuestionID int64  `json:"questionId,omitempty"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		var err error
		var id int64
		switch {
		case body.QuestionID < 0 || (author == "reviewer" && body.QuestionID != 0):
			http.Error(w, "invalid questionId", 400)
			return
		case author == "reviewer":
			id, err = a.wf.AskProposal(r.Context(), account, r.PathValue("id"), body.Text)
		case body.QuestionID > 0:
			err = a.wf.AnswerProposal(r.Context(), account, r.PathValue("id"), body.QuestionID, author, body.Text)
		default:
			err = a.wf.Comment(r.Context(), account, r.PathValue("id"), author, body.Text)
		}
		if err != nil {
			workflowError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "status": "posted"})
	}
}

func (a *app) retryProposalAnswer(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		if !decodeJSON(w, r, &body) {
			return
		}
		question, err := strconv.ParseInt(r.PathValue("question"), 10, 64)
		if err != nil || question <= 0 {
			http.NotFound(w, r)
			return
		}
		if err = a.wf.RetryProposalAnswer(r.Context(), v.acct.ID, r.PathValue("id"), question); err != nil {
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
