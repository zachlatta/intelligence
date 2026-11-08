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

// accountHost is one of an account's hosts, as the account page lists it.
type accountHost struct {
	Host   string
	Joined time.Time
	Used   *time.Time // when its key was last used; nil if never
	Online bool
}

// accountHostsSQL is the account's hosts, read through its own role (so only its own).
const accountHostsSQL = `SELECT k.host, k.created_at, k.last_used_at, coalesce(s.online, false)
	FROM host_keys k LEFT JOIN host_status s ON s.name = k.host ORDER BY k.host`

// accountPage shows the account's hosts, how to add one, and its password form.
func accountPage(cfg Config, v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := v.pool.Query(r.Context(), accountHostsSQL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		hosts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (accountHost, error) {
			var b accountHost
			return b, row.Scan(&b.Host, &b.Joined, &b.Used, &b.Online)
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		render(w, "account", map[string]any{"Version": cfg.Version, "Username": v.acct.Username, "Hosts": hosts,
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
