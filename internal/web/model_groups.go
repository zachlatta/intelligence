package web

import (
	_ "embed"
	"errors"
	"log"
	"net/http"

	"github.com/zachlatta/intelligence/internal/workflow"
)

//go:embed model-groups.js
var modelGroupsJS string

func modelGroupError(w http.ResponseWriter, err error) {
	var refusal *workflow.Refusal
	if errors.As(err, &refusal) || errors.Is(err, workflow.ErrNotFound) {
		workflowError(w, err)
		return
	}
	log.Printf("model group request: %v", err)
	http.Error(w, "Could not complete the model group request. Please try again.", http.StatusInternalServerError)
}

func (a *app) saveModelGroup(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var g workflow.ModelGroup
		if !decodeJSON(w, r, &g) {
			return
		}
		saved, err := a.wf.SaveModelGroup(r.Context(), v.acct.ID, r.PathValue("id"), g)
		if err != nil {
			modelGroupError(w, err)
			return
		}
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
		}
		writeJSON(w, saved)
	}
}

func (a *app) deleteModelGroup(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := a.wf.DeleteModelGroup(r.Context(), v.acct.ID, r.PathValue("id")); err != nil {
			modelGroupError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *app) suggestModel(w http.ResponseWriter, r *http.Request) {
	key, _ := bearerOf(r)
	host, err := a.store.HostOfKey(r.Context(), key)
	if err != nil {
		modelGroupError(w, err)
		return
	}
	if host == "" {
		http.Error(w, "bad or missing host key", 401)
		return
	}
	var req struct {
		Token   string   `json:"token"`
		Exclude []string `json:"exclude"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	msg, err := a.wf.SuggestModel(r.Context(), host, r.PathValue("id"), req.Token, req.Exclude)
	if err != nil {
		modelGroupError(w, err)
		return
	}
	writeJSON(w, map[string]string{"message": msg})
}
