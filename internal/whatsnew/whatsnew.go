// Package whatsnew holds Intelligence's release notes: what each user-visible feature is, when it
// shipped and a demo of it. They are written in entries.json (newest first) with their demos in
// media/, both built into the binary, and the server syncs them into the whats_new table when it
// starts, so they read with SQL like all other state. Each account's seen entries are in
// whats_new_seen, written through POST /api/whats-new/seen.
//
// Every change that ships a feature people can see adds an entry to entries.json.
package whatsnew

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Entry is one release note.
type Entry struct {
	ID       string `json:"id"`       // a lowercase slug, never changed once released: seen state keys on it
	Released string `json:"released"` // YYYY-MM-DD
	Title    string `json:"title"`
	Summary  string `json:"summary"`            // a few plain sentences: what it is and where to find it
	Media    string `json:"media,omitempty"`    // a file in media/: a GIF, image or short MP4 demo
	MediaAlt string `json:"mediaAlt,omitempty"` // what the demo shows, for those who cannot see it
}

//go:embed entries.json
var entriesJSON []byte

// Media holds each entry's demo, under media/.
//
//go:embed media
var Media embed.FS

// MediaURL is where the server serves the demo called name.
const MediaURL = "/whats-new/media/"

// Entries are the built-in release notes, newest first.
func Entries() ([]Entry, error) {
	var es []Entry
	dec := json.NewDecoder(strings.NewReader(string(entriesJSON)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&es); err != nil {
		return nil, fmt.Errorf("entries.json: %w", err)
	}
	return es, nil
}

// MediaType is the content type of a demo file, or "" for a kind a page cannot show.
func MediaType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".gif":
		return "image/gif"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".mp4":
		return "video/mp4"
	}
	return ""
}

// Sync makes the whats_new table hold exactly entries: it adds the new, changes the edited and
// removes the gone (and with them who had seen them), in one transaction.
func Sync(ctx context.Context, p *pgxpool.Pool, entries []Entry) error {
	return pgx.BeginFunc(ctx, p, func(tx pgx.Tx) error {
		ids := make([]string, len(entries))
		for i, e := range entries {
			if _, err := time.Parse(time.DateOnly, e.Released); err != nil {
				return fmt.Errorf("entry %s: released %q is not YYYY-MM-DD", e.ID, e.Released)
			}
			media := ""
			if e.Media != "" {
				media = MediaURL + e.Media
			}
			ids[i] = e.ID
			if _, err := tx.Exec(ctx, `INSERT INTO whats_new (id, released_on, title, summary, media_url, media_alt, position)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (id) DO UPDATE SET released_on = excluded.released_on, title = excluded.title,
					summary = excluded.summary, media_url = excluded.media_url, media_alt = excluded.media_alt,
					position = excluded.position`,
				e.ID, e.Released, e.Title, e.Summary, media, e.MediaAlt, i); err != nil {
				return fmt.Errorf("entry %s: %w", e.ID, err)
			}
		}
		_, err := tx.Exec(ctx, "DELETE FROM whats_new WHERE NOT (id = ANY ($1))", ids)
		return err
	})
}
