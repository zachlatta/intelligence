package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// TestWorkflowsPageLeadsWithWhatNeedsYouAndWhatIsRunning: /workflows puts what changes every
// minute first. After the task box come the runs waiting on a reply (with the question itself),
// then every live run with its step, then the history table, and the workflow definitions last.
func TestWorkflowsPageLeadsWithWhatNeedsYouAndWhatIsRunning(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	wf := createWorkflow(t, h, workflow.Starting)
	set := func(id, sql string, args ...any) {
		t.Helper()
		if _, err := d.Owner.Exec(t.Context(), "UPDATE workflow_runs SET "+sql+" WHERE id=$1", append([]any{id}, args...)...); err != nil {
			t.Fatal(err)
		}
	}
	asking := startRun(t, h, wf, "Add dark mode to settings")
	set(asking, "status='waiting', step=4, note=$2", "Two choices before landing:\n\n1. **Follow the system theme**, or\n2. Add a toggle.\n\nWhich do you want?")
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_run_events (run_id, account_id, at, kind, step, text)
		SELECT id, account_id, now() - interval '95 minutes', 'ask', 4, note FROM workflow_runs WHERE id=$1`, asking); err != nil {
		t.Fatal(err)
	}
	working := startRun(t, h, wf, "Speed up the search page")
	set(working, "status='running', step=0, current_activity='Profiling the slow query.', activity_updated_at=now()")
	checking := startRun(t, h, wf, "Fix the flaky login test")
	set(checking, "status='running', step=1")
	checking2 := startRun(t, h, wf, "Export invoices as CSV")
	set(checking2, "status='running', step=1")
	stuck := startRun(t, h, wf, "Clean up old feature flags")
	set(stuck, "status='stalled', step=2")
	done := startRun(t, h, wf, "Update the README")
	set(done, "status='succeeded', finished_at=now()")

	rec := get(t, h, "/workflows", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /workflows = %d %s", rec.Code, rec.Body)
	}
	page := rec.Body.String()
	order := []string{`id="task-dictation"`, `id="waiting-runs"`, `id="active-runs"`, `id="runs"`, `id="workflow-defs"`}
	last := -1
	for _, marker := range order {
		i := strings.Index(page, marker)
		if i < 0 {
			t.Fatalf("missing %s", marker)
		}
		if i < last {
			t.Errorf("%s is out of order: want %v", marker, order)
		}
		last = i
	}
	section := func(id string) string {
		t.Helper()
		_, after, found := strings.Cut(page, `id="`+id+`"`)
		if !found {
			t.Fatalf("missing section %s", id)
		}
		before, _, _ := strings.Cut(after, "</section>")
		return before
	}

	needs := section("waiting-runs")
	for _, want := range []string{
		"is asking you: “Add dark mode to settings”",
		"Two choices before landing: 1. Follow the system theme, or 2. Add a toggle. Which do you want?",
		`href="/workflow-runs/` + asking + `#reply">Read the note and reply`,
		"waiting 1h 35m",
	} {
		if !strings.Contains(needs, want) {
			t.Errorf("Needs you lacks %q:\n%s", want, needs)
		}
	}

	active := section("active-runs")
	for _, id := range []string{working, checking, checking2, stuck} {
		if !strings.Contains(active, `href="/workflow-runs/`+id+`"`) {
			t.Errorf("In progress lacks live run %s", id)
		}
	}
	for _, id := range []string{done, asking} {
		if strings.Contains(active, `href="/workflow-runs/`+id+`"`) {
			t.Errorf("In progress shows %s, which is not in progress", id)
		}
	}
	for _, want := range []string{
		`data-step-count="Do the work">Do the work <b>1</b>`,
		`data-step-count="Meets the request">Meets the request <b>2</b>`,
		`data-step-count="Stalled">Stalled <b>1</b>`,
		"Step 2 of 7 · Meets the request",
		"Profiling the slow query.",
		`data-resume-run="` + stuck + `"`,
	} {
		if !strings.Contains(active, want) {
			t.Errorf("In progress lacks %q:\n%s", want, active)
		}
	}

	bob, err := h.store.Create(t.Context(), "bob", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	other := call(t, h, http.MethodGet, "/workflows", "", nil, sessionFor(t, h.store, bob)).Body.String()
	if strings.Contains(other, working) || strings.Contains(other, "is asking you") {
		t.Fatal("another account sees this account's live runs")
	}
}

// TestRunsTableFoldsHostAndProjectIntoWhat: the history table keeps What readable: the host and
// project are a line under the summary, not two columns of their own.
func TestRunsTableFoldsHostAndProjectIntoWhat(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	run := startRun(t, h, createWorkflow(t, h, workflow.Starting), "Update the README")
	if _, err := d.Owner.Exec(t.Context(), "UPDATE workflow_runs SET status='succeeded', repo='/home/someone/dev/example' WHERE id=$1", run); err != nil {
		t.Fatal(err)
	}
	page := get(t, h, "/workflows", nil).Body.String()
	if strings.Contains(page, "<th>Host</th>") || strings.Contains(page, "<th>Project</th>") {
		t.Error("the runs table still has Host and Project columns")
	}
	_, row, _ := strings.Cut(page, `href="/workflow-runs/`+run+`"`)
	row, _, _ = strings.Cut(row, "</td>")
	if !strings.Contains(row, `<span class="mono">host-1</span>`) || !strings.Contains(row, `<span class="mono" title="/home/someone/dev/example">example</span>`) {
		t.Errorf("What lacks the host and project: %s", row)
	}
}

func TestNoteExcerpt(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"## Which one?\n\n- **A** or `B`", "Which one? A or B"},
		{"[the doc](https://example.com/x) is ready", "the doc is ready"},
		{strings.Repeat("word ", 100), strings.TrimSpace(strings.Repeat("word ", 56)) + "…"}, // 279 runes and the ellipsis
		{strings.Repeat("x", 300), strings.Repeat("x", 279) + "…"},
	} {
		if got := noteExcerpt(c.in, 280); got != c.want {
			t.Errorf("noteExcerpt(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWaitedFor(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second:           "under a minute",
		5 * time.Minute:            "5m",
		95 * time.Minute:           "1h 35m",
		26*time.Hour + time.Minute: "1d 2h",
	} {
		if got := waitedFor(d); got != want {
			t.Errorf("waitedFor(%v) = %q, want %q", d, got, want)
		}
	}
}
