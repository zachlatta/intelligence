package web

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// The top of /workflows: what needs a reply, then what is in progress. Both read through the
// account's role, like the rest of the page, so they show what the same SQL would.

// liveStatuses are the runs In progress lists: every run not ended and not waiting on a reply.
var liveStatuses = []string{workflow.FindingProject, workflow.StartingOrchestrator, workflow.Running, workflow.Queued, workflow.Stalled}

// activeRunsLimit bounds In progress; the history table below still lists every run.
const activeRunsLimit = 200

// waitingRun is a run waiting on a reply, with how long it has waited and the start of its question.
type waitingRun struct {
	runRow
	Waited, Question string
}

// readWaitingRuns reads the runs waiting on a reply, oldest ask first, each with the start of its
// note and the time since its latest ask (its last update when it has no ask event).
func readWaitingRuns(ctx context.Context, p *pgxpool.Pool, now time.Time) ([]waitingRun, error) {
	rows, err := p.Query(ctx, runRowSelect+"WHERE r.status = $1 ORDER BY r.created_at, r.id", workflow.Waiting)
	if err != nil {
		return nil, err
	}
	runs, err := pgx.CollectRows(rows, scanRunRow)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.ID
	}
	asked := map[string]time.Time{}
	rows, err = p.Query(ctx, `SELECT run_id::text, max(at) FROM workflow_run_events
		WHERE kind = 'ask' AND run_id = ANY($1::uuid[]) GROUP BY run_id`, ids)
	if err != nil {
		return nil, err
	}
	var id string
	var at time.Time
	if _, err := pgx.ForEachRow(rows, []any{&id, &at}, func() error { asked[id] = at; return nil }); err != nil {
		return nil, err
	}
	out := make([]waitingRun, len(runs))
	for i, r := range runs {
		since, ok := asked[r.ID]
		if !ok {
			since = r.UpdatedAt
		}
		out[i] = waitingRun{runRow: r, Waited: waitedFor(now.Sub(since)), Question: noteExcerpt(r.Note, 280)}
	}
	return out, nil
}

// readActiveRuns caps In progress at activeRunsLimit runs, newest first, so a backlog cannot make
// the page unbounded; the history table's status tabs still reach every live run.
func readActiveRuns(ctx context.Context, p *pgxpool.Pool) ([]runRow, error) {
	rows, err := p.Query(ctx, runRowSelect+"WHERE r.status = ANY($1) ORDER BY r.created_at DESC, r.id DESC LIMIT $2", liveStatuses, activeRunsLimit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanRunRow)
}

// stepCount is how many live runs are at one place: a step's title, or a stage before or outside
// the steps (queued, finding the project, starting, stalled).
type stepCount struct {
	Label string
	Count int
}

// stepCounts counts runs by where they are: queued, finding the project and starting first,
// then the steps in the order they come in their workflows, and stalled runs last. Steps of the
// same title in different workflows count together.
func stepCounts(runs []runRow) []stepCount {
	type place struct {
		label      string
		rank, step int
	}
	where := func(r runRow) place {
		switch r.Status {
		case workflow.Queued:
			return place{"Queued", 0, 0}
		case workflow.FindingProject:
			return place{"Finding the project", 1, 0}
		case workflow.StartingOrchestrator:
			return place{"Starting", 2, 0}
		case workflow.Stalled:
			return place{"Stalled", 4, 0}
		}
		if r.Step < len(r.Steps) {
			return place{r.Steps[r.Step].Title, 3, r.Step}
		}
		return place{"Finishing", 3, len(r.Steps)}
	}
	var places []place
	counts := map[string]int{}
	for _, r := range runs {
		p := where(r)
		if counts[p.label] == 0 {
			places = append(places, p)
		}
		counts[p.label]++
	}
	// a stable insertion sort: few places, and ties keep the order runs first showed them
	for i := 1; i < len(places); i++ {
		for j := i; j > 0 && (places[j].rank < places[j-1].rank ||
			places[j].rank == places[j-1].rank && places[j].step < places[j-1].step); j-- {
			places[j], places[j-1] = places[j-1], places[j]
		}
	}
	out := make([]stepCount, len(places))
	for i, p := range places {
		out[i] = stepCount{p.label, counts[p.label]}
	}
	return out
}

// stepMark is one step of a run's progress bar.
type stepMark struct {
	Title, State string // State: "done", "now" or ""
}

// StepMarks are r's steps for its progress bar: those before its step done, its step now.
// Runs not yet at a step (queued, finding the project, starting) have none done.
func (r runRow) StepMarks() []stepMark {
	at := -1
	switch r.Status {
	case workflow.Running, workflow.Waiting, workflow.Stalled:
		at = r.Step
	}
	out := make([]stepMark, len(r.Steps))
	for i, s := range r.Steps {
		out[i].Title = s.Title
		switch {
		case i < at:
			out[i].State = "done"
		case i == at:
			out[i].State = "now"
		}
	}
	return out
}

// StepOf says which step r is at, “Step 2 of 7 · Meets the request”, or where it is before its
// steps (“Finding the project”, “2nd in line for host-1”).
func (r runRow) StepOf() string {
	switch r.Status {
	case workflow.Running, workflow.Waiting, workflow.Stalled:
		if r.Step < len(r.Steps) {
			return fmt.Sprintf("Step %d of %d · %s", r.Step+1, len(r.Steps), r.Steps[r.Step].Title)
		}
	}
	w := r.Where()
	if w == "" {
		return ""
	}
	return strings.ToUpper(w[:1]) + w[1:]
}

// RepoName is the last part of r's project directory (“app” for /home/someone/dev/app).
func (r runRow) RepoName() string {
	if r.Repo == "" {
		return ""
	}
	return path.Base(strings.TrimRight(r.Repo, "/"))
}

var (
	mdLink   = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	mdMarker = regexp.MustCompile(`(?m)^\s*(#{1,6}|[-*+]|>)\s+`)
	mdInline = strings.NewReplacer("**", "", "__", "", "`", "")
)

// noteExcerpt is the start of a Markdown note as one line of plain text, at most n runes: link
// text without its address, no heading, bullet or emphasis marks, and an ellipsis when cut.
func noteExcerpt(note string, n int) string {
	s := mdLink.ReplaceAllString(note, "$1")
	s = mdMarker.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(mdInline.Replace(s)), " ")
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	cut := n - 1
	if runes[cut] != ' ' {
		if i := strings.LastIndex(string(runes[:cut]), " "); i > 0 {
			return strings.TrimSpace(string(runes[:cut])[:i]) + "…"
		}
	}
	return strings.TrimSpace(string(runes[:cut])) + "…"
}

// waitedFor is a wait in words: “under a minute”, “5m”, “1h 35m”, “1d 2h”.
func waitedFor(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%dd %dh", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
}
