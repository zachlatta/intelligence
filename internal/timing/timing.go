// Package timing records how long the server takes to answer each HTTP request (http_requests)
// and how long each page takes to load in the browser (page_loads), and prunes both after
// Retention.
//
// Requests are recorded without slowing them: Request queues a row without blocking, dropping it
// (and counting it, Dropped) when the queue is full, and Run writes the queue in batches.
package timing

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Retention is how long timing rows are kept.
const Retention = 30 * 24 * time.Hour

// PruneEvery is how often Run prunes rows older than Retention (and once when it starts).
const PruneEvery = time.Hour

// FlushEvery is how often the server writes queued requests.
const FlushEvery = time.Second

// QueueSize is how many requests the server queues between flushes before dropping them.
const QueueSize = 10000

// Request is one answered HTTP request. Route is the mux pattern that served it, not the path.
type Request struct {
	At        time.Time
	AccountID int64 // 0: no one signed in
	Method    string
	Route     string
	Status    int
	Duration  time.Duration
}

// PageLoad is one page's navigation timing, as its browser reported it.
type PageLoad struct {
	At                 time.Time
	AccountID          int64
	Route              string
	NavigationType     string
	TTFBMS             float64
	DOMContentLoadedMS float64
	LoadMS             float64
	DurationMS         float64
}

// Recorder queues requests and writes them to http_requests.
type Recorder struct {
	db      *pgxpool.Pool
	queue   chan Request
	dropped atomic.Int64
}

// New is a recorder writing to db that queues up to size requests between flushes.
func New(db *pgxpool.Pool, size int) *Recorder {
	return &Recorder{db: db, queue: make(chan Request, size)}
}

// Request queues req for the next flush, or drops it when the queue is full. It never blocks.
func (r *Recorder) Request(req Request) {
	select {
	case r.queue <- req:
	default:
		r.dropped.Add(1)
	}
}

// Dropped is how many requests were dropped because the queue was full.
func (r *Recorder) Dropped() int64 { return r.dropped.Load() }

// Flush writes what is queued, at most one queue's worth, in one COPY.
func (r *Recorder) Flush(ctx context.Context) error {
	var rows [][]any
	for len(rows) < cap(r.queue) { // at most one queue's worth, so a busy server still returns
		select {
		case req := <-r.queue:
			var acct any
			if req.AccountID != 0 {
				acct = req.AccountID
			}
			rows = append(rows, []any{req.At, acct, req.Method, req.Route, req.Status, ms(req.Duration)})
			continue
		default:
		}
		break
	}
	if len(rows) == 0 {
		return nil
	}
	_, err := r.db.CopyFrom(ctx, pgx.Identifier{"http_requests"},
		[]string{"at", "account_id", "method", "route", "status", "duration_ms"}, pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("record %d requests: %w", len(rows), err)
	}
	return nil
}

// Run flushes every flushEvery and prunes rows older than Retention when it starts and every
// PruneEvery, until ctx ends; then it writes what is still queued and returns.
func (r *Recorder) Run(ctx context.Context, flushEvery time.Duration) {
	if _, err := Prune(ctx, r.db, time.Now()); err != nil && ctx.Err() == nil {
		log.Printf("timing: %v", err)
	}
	flush := time.NewTicker(flushEvery)
	defer flush.Stop()
	prune := time.NewTicker(PruneEvery)
	defer prune.Stop()
	var reported int64
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := r.Flush(final); err != nil {
				log.Printf("timing: %v", err)
			}
			return
		case <-flush.C:
			if err := r.Flush(ctx); err != nil && ctx.Err() == nil {
				log.Printf("timing: %v", err)
			}
			if d := r.Dropped(); d != reported {
				log.Printf("timing: %d requests dropped so far (queue full)", d)
				reported = d
			}
		case <-prune.C:
			if _, err := Prune(ctx, r.db, time.Now()); err != nil && ctx.Err() == nil {
				log.Printf("timing: %v", err)
			}
		}
	}
}

// RecordPageLoad writes p synchronously: page loads are few, and the beacon hears if it failed.
func RecordPageLoad(ctx context.Context, db *pgxpool.Pool, p PageLoad) error {
	_, err := db.Exec(ctx, `INSERT INTO page_loads (at, account_id, route, navigation_type, ttfb_ms, dom_content_loaded_ms, load_ms, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		p.At, p.AccountID, p.Route, p.NavigationType, p.TTFBMS, p.DOMContentLoadedMS, p.LoadMS, p.DurationMS)
	if err != nil {
		return fmt.Errorf("record page load: %w", err)
	}
	return nil
}

// Prune deletes timing rows from before now minus Retention, and returns how many.
func Prune(ctx context.Context, db *pgxpool.Pool, now time.Time) (int64, error) {
	before := now.Add(-Retention)
	var n int64
	for _, table := range []string{"http_requests", "page_loads"} {
		tag, err := db.Exec(ctx, `DELETE FROM `+table+` WHERE at < $1`, before)
		if err != nil {
			return n, fmt.Errorf("prune %s: %w", table, err)
		}
		n += tag.RowsAffected()
	}
	return n, nil
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
