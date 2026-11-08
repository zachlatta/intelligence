package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/zachlatta/intelligence/internal/accounts"
	"github.com/zachlatta/intelligence/internal/fleet"
)

// maxEnrollmentWait bounds ?wait= on a box collecting its key.
const maxEnrollmentWait = time.Minute

// enroll is POST /api/devbox-enrollments {"box"}: a dev box asks to join an account. It gets a
// code to show its person, and a secret to collect its key with once the code is entered.
func (a *app) enroll(w http.ResponseWriter, r *http.Request) {
	var req struct{ Box string }
	if !decode(w, r, &req) {
		return
	}
	e, err := a.store.Enroll(r.Context(), req.Box)
	if err != nil {
		accountError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(e)
}

// collectKey is POST /api/devbox-enrollments/{id}/key?wait=30s, with the enrollment's secret as
// the bearer token: 200 {"key", "box"} once a signed-in account has entered the box's code, 202
// while none has (after waiting up to ?wait for one to), 404 once it expired.
func (a *app) collectKey(w http.ResponseWriter, r *http.Request) {
	var wait time.Duration
	if v := r.URL.Query().Get("wait"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 || d > maxEnrollmentWait {
			http.Error(w, fmt.Sprintf("bad wait %q: want a duration up to %s", v, maxEnrollmentWait), http.StatusBadRequest)
			return
		}
		wait = d
	}
	secret, _ := bearerOf(r)
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	claimed, stop := a.changes.Subscribe(fleet.EnrollmentTopic(id)) // before trying: no claim is missed
	defer stop()
	for {
		key, box, err := a.store.Collect(r.Context(), id, secret)
		switch {
		case err == nil:
			writeJSON(w, map[string]string{"key": key, "box": box})
			return
		case !errors.Is(err, accounts.ErrNotClaimed):
			accountError(w, err)
			return
		case ctx.Err() != nil:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "waiting for its code to be entered"})
			return
		}
		select {
		case <-ctx.Done():
		case <-claimed:
		}
	}
}

// claimBox is POST /api/devbox-enrollments/claim {"code"}: the box showing code joins the account.
func (a *app) claimBox(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Code string }
		if !decode(w, r, &req) {
			return
		}
		box, err := a.store.Claim(r.Context(), v.acct, req.Code)
		if err != nil {
			accountError(w, err)
			return
		}
		writeJSON(w, map[string]string{"box": box})
	}
}

// removeBox is DELETE /api/devboxes/{box}: the box leaves the account, with all it reported.
func (a *app) removeBox(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := a.store.RemoveBox(r.Context(), v.acct.ID, r.PathValue("box")); err != nil {
			accountError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// owned answers the request itself (404) unless box is v's.
func (a *app) owned(v viewer, w http.ResponseWriter, r *http.Request, box string) bool {
	ok, err := a.store.Owns(r.Context(), v.acct.ID, box)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return false
	}
	if !ok {
		http.Error(w, "no such dev box in this account", http.StatusNotFound)
	}
	return ok
}
