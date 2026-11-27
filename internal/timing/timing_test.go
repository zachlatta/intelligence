package timing

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

// TestMain migrates one template database and clones it for each test database.
func TestMain(m *testing.M) {
	os.Exit(dbtest.RunWithTemplate(m))
}

// A recorded request reaches http_requests on the next flush, with its route pattern, method,
// status, duration and time, and nothing else about the request.
func TestRecorderFlushesRequests(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	at := time.Date(2026, 11, 27, 7, 40, 0, 0, time.UTC)
	r := New(d.Owner, 16)
	r.Request(Request{At: at, Method: "GET", Route: "/workflows/{id}", Status: 200, Duration: 1500 * time.Microsecond})
	r.Request(Request{At: at, Method: "POST", Route: "/api/sql", Status: 400, Duration: 3 * time.Millisecond})
	if err := r.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Owner.Query(t.Context(), `SELECT method, route, status, duration_ms, at, account_id FROM http_requests ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		method, route string
		status        int
		ms            float64
		at            time.Time
		account       *int64
	}
	var got []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.method, &x.route, &x.status, &x.ms, &x.at, &x.account); err != nil {
			t.Fatal(err)
		}
		got = append(got, x)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if got[0].method != "GET" || got[0].route != "/workflows/{id}" || got[0].status != 200 || got[0].ms != 1.5 || !got[0].at.Equal(at) || got[0].account != nil {
		t.Errorf("first row = %+v", got[0])
	}
	if got[1].method != "POST" || got[1].status != 400 || got[1].ms != 3 {
		t.Errorf("second row = %+v", got[1])
	}
	// a second flush with nothing queued writes nothing
	if err := r.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM http_requests`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("after empty flush: %d rows, %v", n, err)
	}
}

// Recording never blocks a request: past the queue's size, requests are dropped and counted.
func TestRecorderDropsWhenFull(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	r := New(d.Owner, 2)
	for range 5 {
		r.Request(Request{At: time.Now(), Method: "GET", Route: "/", Status: 200})
	}
	if got := r.Dropped(); got != 3 {
		t.Errorf("dropped %d, want 3", got)
	}
	if err := r.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM http_requests`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d rows, %v; want 2", n, err)
	}
}

// Requests may be recorded from many goroutines while Run flushes; when Run's context ends it
// writes what is still queued before returning.
func TestRunFlushesOnStop(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	r := New(d.Owner, 1000)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { r.Run(ctx, time.Hour); close(done) }()
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			for range 20 {
				r.Request(Request{At: time.Now(), Method: "GET", Route: "/", Status: 200, Duration: time.Millisecond})
			}
		})
	}
	wg.Wait()
	cancel()
	<-done
	var n int
	if err := d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM http_requests`).Scan(&n); err != nil || n != 200 {
		t.Fatalf("%d rows, %v; want 200", n, err)
	}
}

// Rows older than Retention are pruned from both tables; newer ones are kept.
func TestPrune(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	now := time.Date(2026, 11, 27, 8, 0, 0, 0, time.UTC)
	old, recent := now.Add(-Retention-time.Hour), now.Add(-Retention+time.Hour)
	var acct int64
	if err := d.Owner.QueryRow(t.Context(), `INSERT INTO accounts (username, password_hash, role_name) VALUES ('pruner', 'x', 'pruner_role') RETURNING id`).Scan(&acct); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{old, recent} {
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO http_requests (at, method, route, status, duration_ms) VALUES ($1, 'GET', '/', 200, 1)`, at); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO page_loads (at, account_id, route, navigation_type, ttfb_ms, dom_content_loaded_ms, load_ms, duration_ms) VALUES ($1, $2, '/', 'navigate', 1, 2, 3, 3)`, at, acct); err != nil {
			t.Fatal(err)
		}
	}
	n, err := Prune(t.Context(), d.Owner, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("pruned %d rows, want 2", n)
	}
	for _, table := range []string{"http_requests", "page_loads"} {
		var left time.Time
		var count int
		if err := d.Owner.QueryRow(t.Context(), `SELECT count(*), max(at) FROM `+table).Scan(&count, &left); err != nil {
			t.Fatal(err)
		}
		if count != 1 || !left.Equal(recent) {
			t.Errorf("%s kept %d rows (newest %v), want the recent one", table, count, left)
		}
	}
}

// Both tables keep the read-only SQL contract, and each account reads only its own rows (and the
// requests no one was signed in for).
func TestTablesKeepTheContract(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	if err := db.CheckContract(t.Context(), d.Owner, d.Readonly); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Readonly.Exec(t.Context(), `INSERT INTO http_requests (at, method, route, status, duration_ms) VALUES (now(), 'GET', '/', 200, 1)`); err == nil {
		t.Error("the read-only role wrote http_requests")
	}
	if _, err := d.Readonly.Exec(t.Context(), `SELECT count(*) FROM http_requests, page_loads`); err != nil {
		t.Errorf("the read-only role cannot read: %v", err)
	}
}
