package web

import (
	_ "embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/whatsnew"
)

//go:embed whatsnew.html
var whatsNewHTML string

//go:embed whatsnew.js
var whatsNewJS []byte

var (
	whatsNewTmpl   = template.Must(template.New("whatsnew").Funcs(template.FuncMap{"short": short}).Parse(whatsNewHTML + layoutHTML))
	whatsNewScript = asset("whatsnew.js", "text/javascript; charset=utf-8", whatsNewJS)
)

// whatsNewEntry is one release note as the What's New page shows it.
type whatsNewEntry struct {
	ID, Title, Summary, MediaURL, MediaAlt string
	Released                               time.Time
	Unseen                                 bool
}

func (e whatsNewEntry) Video() bool { return strings.HasSuffix(strings.ToLower(e.MediaURL), ".mp4") }

// whatsNewSQL is the release notes, newest first, and whether the reading account has seen each.
const whatsNewSQL = `SELECT w.id, w.title, w.summary, w.media_url, w.media_alt, w.released_on,
	NOT EXISTS (SELECT 1 FROM whats_new_seen s WHERE s.entry_id = w.id AND s.account_id = current_account_id())
	FROM whats_new w ORDER BY w.released_on DESC, w.position`

// whatsNewPage lists the release notes, marking those the account had not seen. It writes
// nothing: whatsnew.js marks them seen through the API once they are shown.
func whatsNewPage(cfg Config, v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := v.pool.Query(r.Context(), whatsNewSQL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (whatsNewEntry, error) {
			var e whatsNewEntry
			return e, row.Scan(&e.ID, &e.Title, &e.Summary, &e.MediaURL, &e.MediaAlt, &e.Released, &e.Unseen)
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := whatsNewTmpl.ExecuteTemplate(w, "whatsnew", map[string]any{"Version": cfg.Version, "Entries": entries}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// markWhatsNewSeen is POST /api/whats-new/seen, {"ids": [...]}: the account has seen those
// entries. Each must exist; marking one again keeps when it was first seen.
func (a *app) markWhatsNewSeen(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IDs []string `json:"ids"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil || len(body.IDs) == 0 || len(body.IDs) > 200 {
			http.Error(w, `send {"ids": ["<entry id>", ...]}: 1 to 200 ids from whats_new`, http.StatusBadRequest)
			return
		}
		// Validate and mark in one statement: an unknown id rejects the entire request.
		var known bool
		err := a.cfg.DB.QueryRow(r.Context(), `WITH requested AS (
			SELECT DISTINCT unnest($2::text[]) AS id
		), known AS (
			SELECT id FROM whats_new WHERE id IN (SELECT id FROM requested)
		), valid AS (
			SELECT (SELECT count(*) FROM known) = (SELECT count(*) FROM requested) AS ok
		), marked AS (
			INSERT INTO whats_new_seen (account_id, entry_id)
			SELECT $1, id FROM known WHERE (SELECT ok FROM valid) ON CONFLICT DO NOTHING
		)
		SELECT ok FROM valid`, v.acct.ID, body.IDs).Scan(&known)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !known {
			http.Error(w, "no such What's New entry", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// whatsNewMedia serves the release notes' demos, built into the binary.
func whatsNewMedia() http.HandlerFunc {
	files := map[string]http.HandlerFunc{}
	names, _ := fs.Glob(whatsnew.Media, "media/*")
	for _, path := range names {
		name := strings.TrimPrefix(path, "media/")
		body, err := fs.ReadFile(whatsnew.Media, path)
		if ct := whatsnew.MediaType(name); ct != "" && err == nil {
			files[name] = asset(name, ct, body)
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		serve, ok := files[r.PathValue("name")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		serve(w, r)
	}
}
