package web

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

func loginPage(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		render(w, "login", map[string]any{"Version": cfg.Version, "Next": localPath(r.URL.Query().Get("next"))})
	}
}

// accountBox is one of an account's dev boxes, as the account page lists it.
type accountBox struct {
	Box    string
	Joined time.Time
	Used   *time.Time // when its key was last used; nil if never
	Online bool
}

// accountBoxesSQL is the account's boxes, read through its own role (so only its own).
const accountBoxesSQL = `SELECT k.box, k.created_at, k.last_used_at, coalesce(s.online, false)
	FROM devbox_keys k LEFT JOIN devbox_status s ON s.name = k.box ORDER BY k.box`

// accountPage shows the account's dev boxes, how to add one, and its password form.
func accountPage(cfg Config, v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := v.pool.Query(r.Context(), accountBoxesSQL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		boxes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (accountBox, error) {
			var b accountBox
			return b, row.Scan(&b.Box, &b.Joined, &b.Used, &b.Online)
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		render(w, "account", map[string]any{"Version": cfg.Version, "Username": v.acct.Username, "Boxes": boxes,
			"Now": cfg.Now(), "Server": serverURL(r)})
	}
}

// serverURL is this server's base URL as the browser reached it.
func serverURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}
