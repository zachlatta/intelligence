package whatsnew

import (
	"context"
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/dbtest"
)

// Every release that ships something people can see adds an entry to entries.json: this keeps the
// file one a page can show.
func TestEntriesAreValid(t *testing.T) {
	entries, err := Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no entries")
	}
	slug := regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	seen := map[string]bool{}
	var last time.Time
	for i, e := range entries {
		if !slug.MatchString(e.ID) {
			t.Errorf("entry %d: id %q is not a lowercase slug", i, e.ID)
		}
		if seen[e.ID] {
			t.Errorf("entry %d: id %q repeats", i, e.ID)
		}
		seen[e.ID] = true
		if strings.TrimSpace(e.Title) == "" || strings.TrimSpace(e.Summary) == "" {
			t.Errorf("entry %s: needs a title and a summary", e.ID)
		}
		if len(e.Summary) > 600 {
			t.Errorf("entry %s: summary is %d bytes; keep it short (600 at most)", e.ID, len(e.Summary))
		}
		d, err := time.Parse(time.DateOnly, e.Released)
		if err != nil {
			t.Errorf("entry %s: released %q is not YYYY-MM-DD", e.ID, e.Released)
			continue
		}
		if i > 0 && d.After(last) {
			t.Errorf("entry %s: entries go newest first, but it is newer than the one before it", e.ID)
		}
		last = d
		// every entry shows the feature: a GIF of it in use, or a screenshot of where it shows
		if e.Media == "" {
			t.Errorf("entry %s: has no demo: add a GIF (or a screenshot) to media/ and name it in media", e.ID)
			continue
		}
		info, err := fs.Stat(Media, "media/"+e.Media)
		if err != nil {
			t.Errorf("entry %s: media %q is not in media/: %v", e.ID, e.Media, err)
		} else if info.Size() > maxMedia {
			t.Errorf("entry %s: media %q is %d bytes; keep a demo under %d (fewer frames, smaller, fewer colours)", e.ID, e.Media, info.Size(), maxMedia)
		}
		if MediaType(e.Media) == "" {
			t.Errorf("entry %s: media %q is not a .gif, .png, .jpg, .webp or .mp4", e.ID, e.Media)
		}
		if strings.TrimSpace(e.MediaAlt) == "" {
			t.Errorf("entry %s: media needs alt text", e.ID)
		}
	}
	if !seen["whats-new"] {
		t.Error("the What's New feature itself has no entry")
	}
}

// maxMedia is the most a demo may weigh: every one is built into the binary.
const maxMedia = 1536 << 10

// Every file in media/ is shown by some entry: an unused one is dead weight in the binary.
func TestMediaIsUsed(t *testing.T) {
	entries, err := Entries()
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	for _, e := range entries {
		used["media/"+e.Media] = true
	}
	files, err := fs.Glob(Media, "media/*")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !used[f] {
			t.Errorf("%s is in no entry", f)
		}
	}
}

func TestSync(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	v1 := []Entry{
		{ID: "b", Released: "2026-11-02", Title: "B", Summary: "second"},
		{ID: "a", Released: "2026-11-01", Title: "A", Summary: "first", Media: "a.gif", MediaAlt: "a demo"},
	}
	if err := Sync(ctx, d.Owner, v1); err != nil {
		t.Fatal(err)
	}
	if got := rows(t, d.Readonly); got != "b 2026-11-02 B second  |a 2026-11-01 A first /whats-new/media/a.gif a demo|" {
		t.Fatalf("after first sync: %q", got)
	}
	// an edited entry changes in place, a removed one goes, a new one comes, and syncing twice is a no-op
	v2 := []Entry{
		{ID: "c", Released: "2026-11-03", Title: "C", Summary: "third"},
		{ID: "b", Released: "2026-11-02", Title: "B2", Summary: "second, edited"},
	}
	for range 2 {
		if err := Sync(ctx, d.Owner, v2); err != nil {
			t.Fatal(err)
		}
	}
	if got := rows(t, d.Readonly); got != "c 2026-11-03 C third  |b 2026-11-02 B2 second, edited  |" {
		t.Fatalf("after second sync: %q", got)
	}
	if err := Sync(ctx, d.Owner, []Entry{{ID: "x", Released: "nope", Title: "X", Summary: "x"}}); err == nil {
		t.Fatal("a bad date synced")
	}
}

// The embedded entries sync into a fresh database.
func TestSyncEmbedded(t *testing.T) {
	d := dbtest.New(t)
	entries, err := Entries()
	if err != nil {
		t.Fatal(err)
	}
	if err := Sync(context.Background(), d.Owner, entries); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.Readonly.QueryRow(context.Background(), "SELECT count(*) FROM whats_new").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(entries) {
		t.Fatalf("%d rows for %d entries", n, len(entries))
	}
}

func rows(t *testing.T, p interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) string {
	t.Helper()
	r, err := p.Query(context.Background(), "SELECT id, released_on::text, title, summary, media_url, media_alt FROM whats_new ORDER BY released_on DESC, position")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for r.Next() {
		var id, rel, title, summary, media, alt string
		if err := r.Scan(&id, &rel, &title, &summary, &media, &alt); err != nil {
			t.Fatal(err)
		}
		b.WriteString(strings.Join([]string{id, rel, title, summary, media, alt}, " ") + "|")
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
