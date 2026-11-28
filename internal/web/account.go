package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

func loginPage(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var next string
		if u := localPageURL(r.URL.Query().Get("next")); u != nil {
			next = u.String()
		}
		render(w, "login", map[string]any{"Version": cfg.Version, "Next": next})
	}
}

// localPageURL accepts only a root-relative URL on this origin.
func localPageURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || !strings.HasPrefix(u.Path, "/") ||
		strings.HasPrefix(raw, "//") || strings.HasPrefix(u.Path, "//") || strings.ContainsRune(u.Path, '\\') ||
		strings.IndexFunc(u.Path, unicode.IsControl) >= 0 {
		return nil
	}
	return u
}

var sessionPageID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// sessionSourcePath records a registered page path with record UUIDs. Keep
// other route parameters as placeholders, since they can carry arbitrary text.
func (a *app) sessionSourcePath(next string) string {
	u := localPageURL(next)
	if u == nil {
		return "/login"
	}
	_, pattern := a.routes.Handler(&http.Request{Method: http.MethodGet, URL: u})
	route, ok := strings.CutPrefix(pattern, "GET ")
	if !ok || strings.HasPrefix(route, "/api/") || strings.HasSuffix(route, "...}") {
		return "/login"
	}
	if route == "/{$}" {
		return "/"
	}
	parts, pathParts := strings.Split(route, "/"), strings.Split(u.Path, "/")
	if len(parts) == len(pathParts) {
		for i, part := range parts {
			if part == "{id}" && sessionPageID.MatchString(pathParts[i]) {
				parts[i] = pathParts[i]
			}
		}
	}
	return strings.Join(parts, "/")
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

type accountMCPToken struct {
	ID               string
	Created, Expires time.Time
	Active           bool
}

const accountMCPTokensSQL = `SELECT id, created_at, expires_at, expires_at > now() AS active
	FROM mcp_tokens ORDER BY created_at DESC, id`

type accountPushSub struct {
	ID           string
	EndpointHost string
	UserAgent    string
	Created      time.Time
	LastSent     *time.Time
	LastError    string
}

const accountPushSubsSQL = `SELECT id::text, endpoint_host, user_agent, created_at, last_sent_at, last_error
	FROM push_subscriptions ORDER BY created_at DESC, id`

// accountPage shows the account's hosts, how to add one, notifications, MCP connections and password form.
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
		rows, err = v.pool.Query(r.Context(), accountMCPTokensSQL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		tokens, err := pgx.CollectRows(rows, pgx.RowToStructByPos[accountMCPToken])
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rows, err = v.pool.Query(r.Context(), accountPushSubsSQL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		pushSubs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[accountPushSub])
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		vapidPub := ""
		if cfg.VAPID != nil {
			vapidPub = cfg.VAPID.PublicKey
		}
		w.Header().Set("Cache-Control", "no-store")
		render(w, "account", map[string]any{
			"Version": cfg.Version, "Username": v.acct.Username, "Hosts": hosts,
			"MCPTokens": tokens, "MCPTokensSQL": accountMCPTokensSQL,
			"PushSubs": pushSubs, "PushSubsSQL": accountPushSubsSQL, "VAPIDPublicKey": vapidPub,
			"Now": cfg.Now(), "Server": requestOrigin(cfg, r),
		})
	}
}

// requestOrigin is the server's origin as the browser reaches it: PUBLIC_URL, else the request's.
func requestOrigin(cfg Config, r *http.Request) string {
	if cfg.PublicURL != "" {
		return strings.TrimRight(cfg.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
		scheme = p
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}
