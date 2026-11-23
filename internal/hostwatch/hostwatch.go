// Package hostwatch notices a host that stopped reporting while workflow runs depend on it, and
// tells the account holder: one phone notification when it has been silent for After (10 minutes),
// one when it reports again, never a repeat while it stays down, and nothing for a host with no
// active runs. Its state is in Postgres (host_silence_alerts), so every server instance and a
// restarted server agree on what was sent, and the red banner reads it through the SQL API.
package hostwatch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/webpush"
)

// After is how long a host with active runs may be silent before it alerts.
const After = 10 * time.Minute

// Every is how often the watcher checks.
const Every = time.Minute

// maxAttempts bounds sends of one notice that PDW refused.
const maxAttempts = 10

// activeStatuses are the run statuses a silent host holds up.
const activeStatuses = `('queued','finding_project','starting','running','waiting','stalled')`

// lockKey serializes Check across server instances (pg_advisory_xact_lock).
const lockKey = 0x686f737477617463 // "hostwatc"

// Notice is one phone notification.
type Notice struct {
	Title, Body string
	ThreadID    string // groups the alerts in Notification Center
	Level       string // interruption level: passive, active, time-sensitive or critical
}

// Notifier delivers a notice to the phone.
type Notifier interface {
	Notify(ctx context.Context, n Notice) error
}

// Watcher checks the hosts. Notify nil records alerts (the banner) but sends nothing.
type Watcher struct {
	DB      *pgxpool.Pool
	Notify  Notifier
	Push    webpush.Sender
	Account string // the username whose hosts' alerts go to the phone; "" none
	// Location is the zone the notices give times in; default UTC.
	Location *time.Location
	// Started is when this server started: a host is not silent for longer than the server has
	// been listening for it, so a server that was down itself does not alert on every host.
	Started time.Time
	After   time.Duration // default After
	Now     func() time.Time
}

// Run checks every minute until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	tick := time.NewTicker(Every)
	defer tick.Stop()
	for {
		bounded, cancel := context.WithTimeout(ctx, 45*time.Second)
		err := w.Check(bounded)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("host watch: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watcher) loc() *time.Location {
	if w.Location != nil {
		return w.Location
	}
	return time.UTC
}

// Check opens an alert for each newly silent host with active runs, closes the alerts of hosts
// heard from again, and sends what is still to send. One instance at a time does it.
func (w *Watcher) Check(ctx context.Context) error {
	after := w.After
	if after == 0 {
		after = After
	}
	now := w.now()
	type pushItem struct {
		accountID int64
		msg       webpush.Message
	}
	var pushes []pushItem
	err := pgx.BeginFunc(ctx, w.DB, func(tx pgx.Tx) error {
		var got bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, int64(lockKey)).Scan(&got); err != nil || !got {
			return err // another instance is checking
		}
		// hosts heard from again
		if w.Push != nil {
			rrows, err := tx.Query(ctx, `UPDATE host_silence_alerts a SET recovered_at = h.last_seen
				FROM hosts h
				LEFT JOIN host_keys k ON k.host = h.name
				WHERE h.name = a.host AND a.recovered_at IS NULL AND h.last_seen > a.last_seen
				RETURNING coalesce(k.account_id, 0), a.host, a.last_seen, a.recovered_at`)
			if err != nil {
				return err
			}
			for rrows.Next() {
				var acctID int64
				var host string
				var lastSeen, recovered time.Time
				if err := rrows.Scan(&acctID, &host, &lastSeen, &recovered); err != nil {
					rrows.Close()
					return err
				}
				if acctID > 0 {
					n := w.recoveryNotice(alert{host: host, lastSeen: lastSeen, recovered: &recovered})
					pushes = append(pushes, pushItem{acctID, webpush.Message{
						Title: n.Title, Body: n.Body, URL: "/resources", Tag: "host-silent-" + host,
					}})
				}
			}
			rrows.Close()
			if err := rrows.Err(); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `UPDATE host_silence_alerts a SET recovered_at = h.last_seen
				FROM hosts h WHERE h.name = a.host AND a.recovered_at IS NULL AND h.last_seen > a.last_seen`); err != nil {
				return err
			}
		}
		// newly silent hosts with active runs
		if w.Push != nil {
			arows, err := tx.Query(ctx, `WITH inserted AS (
				INSERT INTO host_silence_alerts (host, last_seen, opened_at, active_runs, running_runs, waiting_runs,
					queued_runs, oom_kills, last_oom_at, oom_comms, notify)
				SELECT h.name, h.last_seen, $1::timestamptz, r.active, r.running, r.waiting, r.queued, coalesce(o.kills, 0), o.last_at, coalesce(o.comms, ''),
				       EXISTS (SELECT 1 FROM host_keys k JOIN accounts acc ON acc.id = k.account_id WHERE k.host = h.name AND acc.username = $3::text AND $3::text <> '')
				FROM hosts h
				CROSS JOIN LATERAL (SELECT count(*)::int AS active,
				         count(*) FILTER (WHERE status IN ('finding_project','starting','running','stalled'))::int AS running,
				         count(*) FILTER (WHERE status = 'waiting')::int AS waiting,
				         count(*) FILTER (WHERE status = 'queued')::int AS queued
				       FROM workflow_runs WHERE host = h.name AND status IN `+activeStatuses+`) r
				LEFT JOIN LATERAL (SELECT count(*)::int AS kills, max(at) AS last_at,
				         string_agg(DISTINCT nullif(comm, ''), ', ') AS comms
				       FROM oom_kills WHERE host = h.name
				         AND at > (SELECT max(at) FROM oom_kills WHERE host = h.name) - interval '1 hour') o ON true
				WHERE greatest(h.last_seen, $4::timestamptz) < $1::timestamptz - make_interval(secs => $2::float8) AND r.active > 0
				  AND NOT EXISTS (SELECT 1 FROM host_silence_alerts a WHERE a.host = h.name AND a.recovered_at IS NULL)
				ON CONFLICT DO NOTHING
				RETURNING host, last_seen, opened_at, active_runs, running_runs, waiting_runs, queued_runs, oom_kills, last_oom_at, oom_comms
			)
			SELECT coalesce(k.account_id, 0), i.host, i.last_seen, i.opened_at, i.active_runs, i.running_runs, i.waiting_runs, i.queued_runs, i.oom_kills, i.last_oom_at, i.oom_comms
			FROM inserted i LEFT JOIN host_keys k ON k.host = i.host`, now, after.Seconds(), w.Account, w.Started)
			if err != nil {
				return err
			}
			for arows.Next() {
				var acctID int64
				var a alert
				if err := arows.Scan(&acctID, &a.host, &a.lastSeen, &a.opened, &a.active, &a.running, &a.waiting, &a.queued, &a.oom, &a.lastOOM, &a.comms); err != nil {
					arows.Close()
					return err
				}
				if acctID > 0 {
					n := w.alertNotice(a, now)
					pushes = append(pushes, pushItem{acctID, webpush.Message{
						Title: n.Title, Body: n.Body, URL: "/resources", Tag: "host-silent-" + a.host,
					}})
				}
			}
			arows.Close()
			if err := arows.Err(); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `INSERT INTO host_silence_alerts (host, last_seen, opened_at, active_runs, running_runs, waiting_runs,
					queued_runs, oom_kills, last_oom_at, oom_comms, notify)
				SELECT h.name, h.last_seen, $1::timestamptz, r.active, r.running, r.waiting, r.queued, coalesce(o.kills, 0), o.last_at, coalesce(o.comms, ''),
				       EXISTS (SELECT 1 FROM host_keys k JOIN accounts acc ON acc.id = k.account_id WHERE k.host = h.name AND acc.username = $3::text AND $3::text <> '')
				FROM hosts h
				CROSS JOIN LATERAL (SELECT count(*)::int AS active,
				         count(*) FILTER (WHERE status IN ('finding_project','starting','running','stalled'))::int AS running,
				         count(*) FILTER (WHERE status = 'waiting')::int AS waiting,
				         count(*) FILTER (WHERE status = 'queued')::int AS queued
				       FROM workflow_runs WHERE host = h.name AND status IN `+activeStatuses+`) r
				LEFT JOIN LATERAL (SELECT count(*)::int AS kills, max(at) AS last_at,
				         string_agg(DISTINCT nullif(comm, ''), ', ') AS comms
				       FROM oom_kills WHERE host = h.name
				         AND at > (SELECT max(at) FROM oom_kills WHERE host = h.name) - interval '1 hour') o ON true
				WHERE greatest(h.last_seen, $4::timestamptz) < $1::timestamptz - make_interval(secs => $2::float8) AND r.active > 0
				  AND NOT EXISTS (SELECT 1 FROM host_silence_alerts a WHERE a.host = h.name AND a.recovered_at IS NULL)
				ON CONFLICT DO NOTHING`, now, after.Seconds(), w.Account, w.Started); err != nil {
				return err
			}
		}
		if w.Notify == nil {
			return nil
		}
		return w.send(ctx, tx, now)
	})
	if err == nil && w.Push != nil && len(pushes) > 0 {
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		for _, p := range pushes {
			if _, _, perr := w.Push.SendAccount(bounded, p.accountID, p.msg); perr != nil && ctx.Err() == nil {
				log.Printf("host watch push: %v", perr)
			}
		}
	}
	return err
}

type alert struct {
	id                                    int64
	host                                  string
	lastSeen, opened                      time.Time
	active, running, waiting, queued, oom int
	lastOOM                               *time.Time
	comms                                 string
	recovered                             *time.Time
}

func (w *Watcher) send(ctx context.Context, tx pgx.Tx, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT id, host, last_seen, opened_at, active_runs, running_runs, waiting_runs, queued_runs,
		oom_kills, last_oom_at, oom_comms, recovered_at
		FROM host_silence_alerts
		WHERE notify AND ((recovered_at IS NULL AND alert_sent_at IS NULL AND alert_attempts < $1)
		   OR (recovered_at IS NOT NULL AND alert_sent_at IS NOT NULL AND recovery_sent_at IS NULL AND recovery_attempts < $1))
		ORDER BY id`, maxAttempts)
	if err != nil {
		return err
	}
	alerts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (alert, error) {
		var a alert
		err := r.Scan(&a.id, &a.host, &a.lastSeen, &a.opened, &a.active, &a.running, &a.waiting, &a.queued, &a.oom, &a.lastOOM, &a.comms, &a.recovered)
		return a, err
	})
	if err != nil {
		return err
	}
	for _, a := range alerts {
		recovery := a.recovered != nil
		n := w.alertNotice(a, now)
		if recovery {
			n = w.recoveryNotice(a)
		}
		sendErr := w.Notify.Notify(ctx, n)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		msg, sent := "", any(now)
		if sendErr != nil {
			msg, sent = Clip(sendErr.Error(), 500), nil
		}
		text := n.Title + "\n" + n.Body
		q := `UPDATE host_silence_alerts SET alert_text = $2, alert_sent_at = $3, alert_error = $4, alert_attempts = alert_attempts + 1 WHERE id = $1`
		if recovery {
			q = `UPDATE host_silence_alerts SET recovery_text = $2, recovery_sent_at = $3, recovery_error = $4, recovery_attempts = recovery_attempts + 1 WHERE id = $1`
		}
		if _, err := tx.Exec(ctx, q, a.id, text, sent, msg); err != nil {
			return err
		}
		if sendErr != nil {
			log.Printf("host watch: notify %s: %v", a.host, sendErr)
		}
	}
	return nil
}

func (w *Watcher) clock(t time.Time) string {
	return t.In(w.loc()).Format("15:04 MST")
}

func (w *Watcher) alertNotice(a alert, now time.Time) Notice {
	var b strings.Builder
	fmt.Fprintf(&b, "Silent %s (last heard %s). %s affected:", Span(now.Sub(a.lastSeen)), w.clock(a.lastSeen), plural(a.active, "run", "runs"))
	var parts []string
	for _, p := range []struct {
		n    int
		what string
	}{{a.running, "running"}, {a.waiting, "waiting on you"}, {a.queued, "queued"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.what))
		}
	}
	b.WriteString(" " + strings.Join(parts, ", ") + ".")
	switch {
	case a.oom > 0 && a.lastOOM != nil:
		at := w.clock(*a.lastOOM)
		if a.lastSeen.Sub(*a.lastOOM) > time.Hour {
			at = a.lastOOM.In(w.loc()).Format("Jan 2 15:04 MST")
		}
		fmt.Fprintf(&b, " Last recorded: %s up to %s", plural(a.oom, "OOM kill", "OOM kills"), at)
		if a.comms != "" {
			b.WriteString(" (" + Clip(a.comms, 120) + ")")
		}
		b.WriteString(".")
	default:
		b.WriteString(" No OOM kills recorded.")
	}
	return Notice{Title: Clip(a.host+" stopped reporting", 60), Body: b.String(), ThreadID: "intelligence-hosts", Level: "time-sensitive"}
}

func (w *Watcher) recoveryNotice(a alert) Notice {
	body := fmt.Sprintf("Back at %s after %s silent. Its runs can move again.", w.clock(*a.recovered), Span(a.recovered.Sub(a.lastSeen)))
	return Notice{Title: Clip(a.host+" is reporting again", 60), Body: body, ThreadID: "intelligence-hosts", Level: "active"}
}

// Span is a duration in whole minutes: "10 min", "3 h 5 min".
func Span(d time.Duration) string {
	m := int(d / time.Minute)
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	if m%60 == 0 {
		return fmt.Sprintf("%d h", m/60)
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Clip shortens s to at most n runes, ending in an ellipsis when cut.
func Clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
