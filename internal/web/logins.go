package web

import (
	_ "embed"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/zachlatta/intelligence/internal/fleet"
)

// logins.js warns on every page about a broken or soon-ending login (login_alerts), with how to
// fix it, and fills the Plans page's history of interrupted agents. It reads through /api/sql and
// writes only through POST /api/login-incidents/{id}/resolve.
//
//go:embed logins.js
var loginsJS []byte

var loginsScript = asset("logins.js", "text/javascript; charset=utf-8", loginsJS)

// resolveLoginIncident marks a broken login fixed (JSON {"note"}), for when the host's meter cannot
// tell: the agents waiting for it are then resumed. A login still broken opens a new incident on its
// next failure or meter read.
func (a *app) resolveLoginIncident(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "no such login incident on your hosts", http.StatusNotFound)
			return
		}
		var req struct {
			Note string `json:"note"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		req.Note = strings.TrimSpace(req.Note)
		if utf8.RuneCountInString(req.Note) > 500 || strings.ContainsFunc(req.Note, func(c rune) bool { return c < ' ' }) {
			http.Error(w, "note: one line of at most 500 characters", http.StatusBadRequest)
			return
		}
		switch err := a.reg.ResolveLoginIncident(r.Context(), v.acct.ID, id, req.Note, a.cfg.Now()); {
		case errors.Is(err, fleet.ErrNotYours):
			http.Error(w, err.Error(), http.StatusNotFound)
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}
