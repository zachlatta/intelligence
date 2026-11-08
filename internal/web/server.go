// Package web serves Intelligence's HTTP interface.
package web

import (
	"cmp"
	_ "embed"
	"encoding/json"
	"html/template"
	"net/http"
)

// Config is what the server needs to know about the running binary.
type Config struct {
	Version string // the deployed commit, shown on every page and by /healthz
}

//go:embed index.html
var indexHTML string

var indexTmpl = template.Must(template.New("index").Parse(indexHTML))

// New returns the app's HTTP handler.
func New(cfg Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": cfg.Version})
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = indexTmpl.Execute(w, map[string]string{
			"Version": cfg.Version,
			"Short":   short(cfg.Version),
			// set by tailscale serve for requests from a tailnet user
			"User": cmp.Or(r.Header.Get("Tailscale-User-Name"), r.Header.Get("Tailscale-User-Login")),
		})
	})
	return mux
}

// short is a commit id cut to 12 characters for display; anything shorter is kept whole.
func short(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}
