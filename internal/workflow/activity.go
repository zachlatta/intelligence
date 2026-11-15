package workflow

import (
	"context"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Remove old agent-authored clock annotations; the status command owns the timestamp.
const activityTime = `(?:[01]?\d|2[0-3]):[0-5]\d(?::[0-5]\d)?`
const activityZone = `(?:UTC|GMT|[ECMP][DS]?T|[ECMP]T|JST|CET|CEST|BST|Z|[+-]\d\d:\d\d)`
const activityDate = `\d{4}-\d\d-\d\d[T ]+`
const activityMeridiem = `(?:AM|PM)`

// Only a complete trailing annotation is removed. Bare colon-separated numbers
// can be durations or versions, so they require a date, zone or meridiem.
var activityClock = regexp.MustCompile(`(?i)\s+(?:[·—(]\s*)?(?:` +
	`(?:since|at)\s+(?:` + activityDate + `)?` + activityTime + `(?:\s*` + activityMeridiem + `)?(?:\s*` + activityZone + `)?|` +
	activityDate + activityTime + `(?:\s*` + activityZone + `)?|` +
	activityTime + `\s*(?:` + activityMeridiem + `(?:\s*` + activityZone + `)?|` + activityZone + `)` +
	`)\s*\)?\.?$`)

func CleanActivity(line string) string {
	return strings.TrimSpace(activityClock.ReplaceAllString(line, ""))
}

func activityValues(m Move) (string, *time.Time, error) {
	line := strings.TrimSpace(m.Now)
	if line == "" || utf8.RuneCountInString(line) > 280 || strings.ContainsFunc(m.Now, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || unicode.Is(unicode.Cf, r)
	}) {
		return "", nil, bad("current activity must be one visible line of 1–280 characters")
	}
	var eta *time.Time
	if m.ETA != "" {
		at, err := time.Parse(time.RFC3339, m.ETA)
		if err != nil {
			return "", nil, bad("eta must be RFC3339")
		}
		eta = &at
	}
	line = CleanActivity(line)
	if line == "" {
		return "", nil, bad("current activity needs text before its clock annotation")
	}
	if len(m.WaitingOn) > 80 || strings.ContainsFunc(m.WaitingOn, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
	}) {
		return "", nil, bad("waitingOn must be one visible name of at most 80 bytes")
	}
	return line, eta, nil
}

// Sum the median completed duration of each remaining machine step. Prefer this
// repository's samples per step, falling back to the same workflow's other repos.
// Missing history for any step leaves the ETA unknown rather than inventing time.
func historyETA(ctx context.Context, tx pgx.Tx, r run, at time.Time) (*time.Time, error) {
	var total float64
	for i := r.Step; i < len(r.Def.Steps); i++ {
		step := r.Def.Steps[i]
		if step.Kind == KindSignoff {
			continue
		}
		var seconds *float64
		err := tx.QueryRow(ctx, `WITH samples AS (
 SELECT extract(epoch from t.time_spent)::double precision AS seconds, h.repo=$3 AS same_repo
 FROM workflow_run_step_times t JOIN workflow_runs h ON h.id=t.run_id
 WHERE h.workflow_id=$1 AND h.account_id=$5 AND h.id<>$6 AND h.status='succeeded'
 AND h.finished_at >= $4::timestamptz-interval '30 days' AND h.finished_at <= $4
 AND t.step_id=$2 AND NOT t.current
), chosen AS (SELECT * FROM samples WHERE same_repo OR NOT EXISTS(SELECT 1 FROM samples WHERE same_repo))
 SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY seconds) FROM chosen`, r.WorkflowID, step.ID, r.Repo, at, r.Account, r.ID).Scan(&seconds)
		if err != nil {
			return nil, err
		}
		if seconds == nil {
			return nil, nil
		}
		remaining := *seconds
		if i == r.Step {
			var elapsed float64
			if err := tx.QueryRow(ctx, `SELECT greatest(extract(epoch from ($3::timestamptz-started_at)),0)::double precision
 FROM workflow_run_step_visits WHERE run_id=$1 AND step=$2 AND ended_at IS NULL
 ORDER BY visit DESC LIMIT 1`, r.ID, i, at).Scan(&elapsed); err != nil && err != pgx.ErrNoRows {
				return nil, err
			}
			remaining -= elapsed
			if remaining < 0 {
				remaining = 0
			}
		}
		total += remaining
	}
	if total <= 0 {
		return nil, nil
	}
	eta := at.Add(time.Duration(total * float64(time.Second)))
	return &eta, nil
}

// refreshActivity follows lifecycle changes without pretending the orchestrator
// reported a new activity line. Agent ETAs survive transitions and human waits;
// the read projection hides them while waiting and restores them after a reply.
func (e *Engine) refreshActivity(ctx context.Context, tx pgx.Tx, r run) error {
	var eta *time.Time
	var estimated bool
	var line string
	if err := tx.QueryRow(ctx, `SELECT activity_eta,activity_eta_estimated,current_activity FROM workflow_runs WHERE id=$1`, r.ID).Scan(&eta, &estimated, &line); err != nil {
		return err
	}
	live := r.Status == Running || r.Status == Waiting || r.Status == Stalled
	waiting := ""
	if live && (r.Status == Waiting || (r.Step >= 0 && r.Step < len(r.Def.Steps) && r.Def.Steps[r.Step].Kind == KindSignoff)) {
		waiting = "Zach"
	}
	if waiting != "" || !live {
		if estimated {
			eta = nil
			estimated = false
		}
	} else if line != "" && (eta == nil || estimated) {
		var err error
		eta, err = historyETA(ctx, tx, r, e.Now())
		if err != nil {
			return err
		}
		estimated = eta != nil
	}
	_, err := tx.Exec(ctx, `UPDATE workflow_runs SET activity_eta=$2,activity_eta_estimated=$3,activity_waiting_on=$4 WHERE id=$1`, r.ID, eta, estimated, waiting)
	return err
}
