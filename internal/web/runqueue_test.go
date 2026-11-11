package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/resources"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// hostMinute sends one finished minute of host-1's resources up b's stream: cpu is its average
// busy %, mem its share of memory used.
func hostMinute(t *testing.T, b *rawHost, minute time.Time, cpu, mem float64) {
	t.Helper()
	const total = 100 << 30
	rep := resources.Report{Minutes: []resources.Minute{{Minute: minute, Samples: 12, CPUs: 8, CPUAvgPct: &cpu, CPUMaxPct: &cpu, Load1: 1,
		MemTotal: total, MemAvailableMin: uint64(total * (1 - mem))}}}
	if ack := b.call(hostclient.Frame{Type: hostclient.HostFrame, Host: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
}

// runField reads one column of a run through the account's SQL API.
func runField(t *testing.T, h http.Handler, run, col string) any {
	t.Helper()
	code, r := sqlPost(t, h, q("SELECT "+col+" FROM workflow_runs WHERE id = '"+run+"'"))
	if code != http.StatusOK || len(r.Rows) != 1 {
		t.Fatalf("run %s %s: %d %+v", run, col, code, r)
	}
	return r.Rows[0][0]
}

func fixedWorkflow(t *testing.T, h http.Handler) string {
	t.Helper()
	d := workflow.Starting
	d.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	return createWorkflow(t, h, d)
}

func runHex(run string) string { return strings.ReplaceAll(run, "-", "")[:12] }

// TestABusyHostQueuesNewRunsAndStartsThemInOrder: while the host's newest minute shows it near
// capacity, a new run waits in a queue (readable with SQL, with why), and the queue starts in the
// order the runs came, one per fresh measurement, once the host has room.
func TestABusyHostQueuesNewRunsAndStartsThemInOrder(t *testing.T) {
	d := dbtest.New(t)
	clock := &fakeClock{t: time.Now().UTC().Truncate(time.Minute)}
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.Now = clock.Now; c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	b := openStream(t, h, "host-1")
	wf := fixedWorkflow(t, h)
	start := clock.Now()
	hostMinute(t, b, start.Add(-time.Minute), 97, 0.5)

	first := startRun(t, h, wf, "Fix the login page")
	clock.Add(time.Second)
	second := startRun(t, h, wf, "Fix the signup page")
	for _, run := range []string{first, second} {
		if s := runField(t, h, run, "status"); s != "queued" {
			t.Fatalf("a run on a busy host should be queued, is %v", s)
		}
	}
	if reason := runField(t, h, first, "queue_reason").(string); !strings.Contains(reason, "CPU") || !strings.Contains(reason, "97%") {
		t.Errorf("the reason should say the CPU is busy: %q", reason)
	}
	if reason := runField(t, h, second, "queue_reason").(string); reason != "1 run queued on host-1 ahead of it" {
		t.Errorf("the reason should say what is queued ahead: %q", reason)
	}
	code, r := sqlPost(t, h, q("SELECT run_id::text, position, host FROM workflow_run_queue ORDER BY position"))
	if code != http.StatusOK || len(r.Rows) != 2 || r.Rows[0][0] != first || r.Rows[1][0] != second || r.Rows[0][2] != "host-1" {
		t.Fatalf("queue = %d %+v", code, r)
	}
	page := get(t, h, "/workflow-runs/"+first, nil).Body.String()
	if !strings.Contains(page, "queued") || !strings.Contains(page, "97%") || !strings.Contains(page, `id="start-now"`) {
		t.Errorf("the run page should show the run queued, why, and a way to start it now")
	}
	if list := get(t, h, "/workflows", nil).Body.String(); !strings.Contains(list, "queued") || !strings.Contains(list, "2nd in line for host-1") {
		t.Errorf("the runs list should show the runs queued, and where in line")
	}
	if !strings.Contains(page, "1st in line for host-1") || !strings.Contains(page, "<span>queued ") || strings.Contains(page, ">Started<") {
		t.Errorf("the run page should say where in line it is, and that it was queued, not started")
	}

	// a minute at 70%: room for one more run (about 80% with it), not two (about 90%)
	clock.Add(time.Minute)
	hostMinute(t, b, start, 70, 0.3) // (its command may come before the ack, which call skips: read the runs)
	if s := runField(t, h, first, "status"); s != "starting" {
		t.Fatalf("the first queued run should start first: %v", s)
	}
	if ws := runField(t, h, first, "workspace_title").(string); !strings.Contains(ws, runHex(first)) {
		t.Errorf("workspace = %q", ws)
	}
	if s := runField(t, h, second, "status"); s != "queued" {
		t.Errorf("the second run should wait: the first's load is not measured yet: %v", s)
	}
	want := "host-1 is near capacity: CPU 70% now, about 90% with 1 run starting and this one (queues at 85%)"
	if reason := runField(t, h, second, "queue_reason").(string); reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
	// the first's workspace is made (as the host would report), then a minute measured since, still
	// at 70%: the second starts
	clock.Add(time.Minute)
	if _, err := d.Owner.Exec(t.Context(), "UPDATE commands SET status = 'succeeded', finished_at = $1 WHERE kind = 'create_workspace'", clock.Now()); err != nil {
		t.Fatal(err)
	}
	clock.Add(2 * time.Minute)
	hostMinute(t, b, start.Add(3*time.Minute), 70, 0.3)
	if s := runField(t, h, second, "status"); s != "starting" {
		t.Fatalf("then the second: %v", s)
	}
	code, r = sqlPost(t, h, q("SELECT count(*) FROM workflow_run_queue"))
	if code != http.StatusOK || r.Rows[0][0] != float64(0) {
		t.Errorf("the queue should be empty: %+v", r)
	}
	code, r = sqlPost(t, h, q("SELECT kind FROM workflow_run_events WHERE run_id = '"+first+"' ORDER BY id"))
	var kinds []string
	for _, row := range r.Rows {
		kinds = append(kinds, row[0].(string))
	}
	if strings.Join(kinds, ",") != "started,queued,dequeued,project" {
		t.Errorf("events = %v", kinds)
	}
}

// TestAQuietHostStartsARunAtOnce: below the thresholds nothing is queued.
func TestAQuietHostStartsARunAtOnce(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	b := openStream(t, h, "host-1")
	hostMinute(t, b, time.Now().UTC().Truncate(time.Minute).Add(-time.Minute), 40, 0.5)
	run := startRun(t, h, fixedWorkflow(t, h), "anything")
	if c := b.command(); c.Kind != hostclient.CreateWorkspaceKind {
		t.Fatalf("command = %s", c.Kind)
	}
	if s := runField(t, h, run, "status"); s != "starting" {
		t.Errorf("status = %v", s)
	}
}

// TestAQueuedRunCanBeStartedNowOrCanceled: the escape hatch starts a queued run whatever the
// host's load; a queued run cancels like any other.
func TestAQueuedRunCanBeStartedNowOrCanceled(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	b := openStream(t, h, "host-1")
	hostMinute(t, b, time.Now().UTC().Truncate(time.Minute).Add(-time.Minute), 20, 0.97)
	wf := fixedWorkflow(t, h)
	first := startRun(t, h, wf, "first")
	second := startRun(t, h, wf, "second")
	if reason := runField(t, h, first, "queue_reason").(string); !strings.Contains(reason, "memory 97% used now, over 100% with this one") {
		t.Errorf("reason = %q", reason)
	}
	if code, msg := move(t, h, key, first, map[string]string{"move": "next"}); code != http.StatusConflict || !strings.Contains(msg, "queued") {
		t.Errorf("a queued run moved: %d %s", code, msg)
	}
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+second+"/start", `{}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("start now = %d %s", rec.Code, rec.Body)
	}
	var wa hostclient.CreateWorkspaceArgs
	json.Unmarshal(b.command().Args, &wa)
	if !strings.Contains(wa.Title, runHex(second)) {
		t.Fatalf("start now should start that run: %q", wa.Title)
	}
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+second+"/start", `{}`, sameOriginFetch); rec.Code != http.StatusConflict {
		t.Errorf("starting a run that is not queued = %d", rec.Code)
	}
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+first+"/cancel", `{}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body)
	}
	if s := runField(t, h, first, "status"); s != "canceled" {
		t.Errorf("status = %v", s)
	}
}

func TestQueuePositionsReadAsOrdinals(t *testing.T) {
	for n, want := range map[int]string{1: "1st", 2: "2nd", 3: "3rd", 4: "4th", 11: "11th", 12: "12th", 13: "13th", 21: "21st", 22: "22nd", 111: "111th", 123: "123rd"} {
		if got := ordinal(n); got != want {
			t.Errorf("ordinal(%d) = %q, want %q", n, got, want)
		}
	}
}

// burst starts n runs of wf at once and returns how many of them are queued.
func burst(t *testing.T, h http.Handler, wf string, n int) (queued int) {
	t.Helper()
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, _ := json.Marshal(map[string]string{"workflowId": wf, "input": fmt.Sprintf("task %d", i)})
			codes[i] = call(t, h, http.MethodPost, "/api/workflow-runs", string(body), sameOriginFetch).Code
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusCreated {
			t.Fatalf("run %d = %d", i, c)
		}
	}
	code, r := sqlPost(t, h, q("SELECT count(*) FILTER (WHERE status = 'queued'), count(*) FROM workflow_runs"))
	if code != http.StatusOK || r.Rows[0][1] != float64(n) {
		t.Fatalf("runs = %d %+v", code, r)
	}
	return int(r.Rows[0][0].(float64))
}

// TestABurstOnAnIdleHostStartsWhatFits: a quiet minute says nothing of runs started since, so each
// run started since counts at its estimated footprint (10% CPU a run with no history to learn
// from): at 10% CPU, 7 of 10 runs started at once fit under 85%, and 3 queue.
func TestABurstOnAnIdleHostStartsWhatFits(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	b := openStream(t, h, "host-1")
	hostMinute(t, b, time.Now().UTC().Truncate(time.Minute).Add(-time.Minute), 10, 0.3)
	if queued := burst(t, h, fixedWorkflow(t, h), 10); queued != 3 {
		t.Fatalf("%d of 10 queued, want 3", queued)
	}
	code, r := sqlPost(t, h, q("SELECT queue_reason FROM workflow_run_queue WHERE position = 1"))
	if want := "host-1 is near capacity: CPU 10% now, about 90% with 7 runs starting and this one (queues at 85%)"; code != http.StatusOK || r.Rows[0][0] != want {
		t.Errorf("reason = %+v, want %q", r, want)
	}
}

// TestAnIdleHostStartsRunsBackToBack: with room, a run starts at once, however recently the last did.
func TestAnIdleHostStartsRunsBackToBack(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	b := openStream(t, h, "host-1")
	hostMinute(t, b, time.Now().UTC().Truncate(time.Minute).Add(-time.Minute), 5, 0.2)
	wf := fixedWorkflow(t, h)
	for _, task := range []string{"first", "second"} {
		run := startRun(t, h, wf, task)
		if s := runField(t, h, run, "status"); s != "starting" {
			t.Fatalf("%s = %v", task, s)
		}
	}
}

// TestAnUnmonitoredHostStartsEveryRun: a host that never sent its resources gives no sign it is
// near capacity, and there is no count of runs to hold them to, so they all start.
func TestAnUnmonitoredHostStartsEveryRun(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	if queued := burst(t, h, fixedWorkflow(t, h), 10); queued != 0 {
		t.Fatalf("%d of 10 queued, want none", queued)
	}
}

// TestEachHostLearnsARunsFootprint: host_run_footprint fits the host's history (its load against
// how many runs were going) to say what one run adds, which then projects its load.
func TestEachHostLearnsARunsFootprint(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	b := openStream(t, h, "host-1")
	wf := fixedWorkflow(t, h)
	code, r := sqlPost(t, h, q("SELECT cpu_learned, cpu_pct_per_run, mem_pct_per_run, load_per_cpu_per_run FROM host_run_footprint WHERE host = 'host-1'"))
	if code != http.StatusOK || len(r.Rows) != 1 || r.Rows[0][0] != false || r.Rows[0][1] != float64(10) || r.Rows[0][2] != float64(5) || r.Rows[0][3] != 0.15 {
		t.Fatalf("with no history, the defaults: %d %+v", code, r)
	}
	// two hours: the first with no runs at 10% CPU, the second with two runs at 50%
	now := time.Now().UTC().Truncate(time.Minute)
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO host_minutes (host, minute, samples, cpus, cpu_avg_pct, load1, mem_total, mem_available_min)
		SELECT 'host-1', $1::timestamptz - i * interval '1 minute', 12, 8, CASE WHEN i <= 60 THEN 50 ELSE 10 END, 1, 107374182400, 75161927680
		FROM generate_series(1, 120) i`, now); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_runs (account_id, workflow_id, version, host, title, input, status,
			created_at, updated_at, launched_at, finished_at)
			SELECT account_id, id, 1, 'host-1', 'past', 'past', 'succeeded', $1, $1, $1, $2 FROM workflows WHERE id = $3`,
			now.Add(-60*time.Minute-30*time.Second), now.Add(-30*time.Second), wf); err != nil {
			t.Fatal(err)
		}
	}
	code, r = sqlPost(t, h, q("SELECT cpu_learned, cpu_pct_per_run, mem_learned, mem_pct_per_run FROM host_run_footprint WHERE host = 'host-1'"))
	if code != http.StatusOK || r.Rows[0][0] != true || r.Rows[0][1] != float64(20) || r.Rows[0][2] != false || r.Rows[0][3] != float64(5) {
		t.Fatalf("learned = %+v, want 20%% CPU a run learned, and the default for memory, which did not move", r)
	}
	first := startRun(t, h, wf, "first") // 50% now, about 70% with it
	b.command()
	second := startRun(t, h, wf, "second")
	if s := runField(t, h, first, "status"); s != "starting" {
		t.Errorf("first = %v", s)
	}
	want := "host-1 is near capacity: CPU 50% now, about 90% with 1 run starting and this one (queues at 85%)"
	if reason := runField(t, h, second, "queue_reason").(string); reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
}

// TestStaleResourcesHoldTheQueue: once the host stops sending its resources, a queued run waits for
// a fresh measurement rather than starting on no evidence, and a host away for long keeps holding
// until it reports again.
func TestStaleResourcesHoldTheQueue(t *testing.T) {
	d := dbtest.New(t)
	clock := &fakeClock{t: time.Now().UTC().Truncate(time.Minute)}
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.Now = clock.Now; c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	b := openStream(t, h, "host-1")
	hostMinute(t, b, clock.Now().Add(-time.Minute), 97, 0.5)
	run := startRun(t, h, fixedWorkflow(t, h), "anything")
	clock.Add(10 * time.Minute)
	nothing := resources.Report{}
	if ack := b.call(hostclient.Frame{Type: hostclient.HostFrame, Host: &nothing}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	if s := runField(t, h, run, "status"); s != "queued" {
		t.Fatalf("a stale measurement should not start a queued run: %v", s)
	}
	if reason := runField(t, h, run, "queue_reason").(string); !strings.Contains(reason, "fresh measurement") {
		t.Errorf("reason = %q", reason)
	}
	clock.Add(time.Hour)
	if ack := b.call(hostclient.Frame{Type: hostclient.HostFrame, Host: &nothing}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	// an hour on, the host (whose clock-stamped stream was last seen an hour ago) is away: it still holds
	if s := runField(t, h, run, "status"); s != "queued" {
		t.Fatalf("a host with history that has gone quiet should keep holding: %v", s)
	}
	if reason := runField(t, h, run, "queue_reason").(string); !strings.Contains(reason, "waiting for host-1 to report again (last measured ") {
		t.Errorf("reason = %q", reason)
	}
}

// TestARunHoldsItsReservationUntilTheHostHasStartedIt: a newer minute shows nothing of runs whose
// start command the host has not carried out yet (delivery is slow, or the host is away), so they
// still count against the projection; once the host reports them done and a minute measured after
// that shows their load, they stop counting and the queue moves.
func TestARunHoldsItsReservationUntilTheHostHasStartedIt(t *testing.T) {
	d := dbtest.New(t)
	clock := &fakeClock{t: time.Now().UTC().Truncate(time.Minute)}
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.Now = clock.Now; c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	b := openStream(t, h, "host-1")
	start := clock.Now()
	hostMinute(t, b, start.Add(-time.Minute), 10, 0.3)
	if queued := burst(t, h, fixedWorkflow(t, h), 10); queued != 3 {
		t.Fatalf("%d of 10 queued, want 3", queued)
	}
	var cmds []hostclient.Command
	for range 7 {
		cmds = append(cmds, b.command())
	}

	// two minutes on, a quiet minute, but none of the 7 workspaces is made yet
	clock.Add(2 * time.Minute)
	hostMinute(t, b, start.Add(time.Minute), 10, 0.3)
	code, r := sqlPost(t, h, q("SELECT count(*) FROM workflow_run_queue"))
	if code != http.StatusOK || r.Rows[0][0] != float64(3) {
		t.Fatalf("runs the host has not started should keep their place in the projection: %+v", r)
	}
	code, r = sqlPost(t, h, q("SELECT queue_reason FROM workflow_run_queue WHERE position = 1"))
	if want := "host-1 is near capacity: CPU 10% now, about 90% with 7 runs starting and this one (queues at 85%)"; code != http.StatusOK || r.Rows[0][0] != want {
		t.Errorf("reason = %+v, want %q", r, want)
	}

	// the host makes them; a minute measured after that shows their load (here, still quiet)
	for i, c := range cmds {
		succeed(t, b, c, fmt.Sprintf(`{"workspaceId":"w-%d","agentId":"orch-%d"}`, i, i))
	}
	if code, r = sqlPost(t, h, q("SELECT count(*) FROM workflow_run_queue")); r.Rows[0][0] != float64(3) {
		t.Fatalf("made, but not measured yet: %+v", r)
	}
	clock.Add(2 * time.Minute)
	hostMinute(t, b, start.Add(3*time.Minute), 10, 0.3)
	if code, r = sqlPost(t, h, q("SELECT count(*) FROM workflow_run_queue")); code != http.StatusOK || r.Rows[0][0] != float64(0) {
		t.Fatalf("with their load measured, the queue should start: %+v", r)
	}
}

// TestContradictoryHistoryIsNotLearned: a fit that says runs lighten the host (here, two runs going
// while CPU was low) is not a footprint: the default stands for that metric.
func TestContradictoryHistoryIsNotLearned(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	wf := fixedWorkflow(t, h)
	now := time.Now().UTC().Truncate(time.Minute)
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO host_minutes (host, minute, samples, cpus, cpu_avg_pct, load1, mem_total, mem_available_min)
		SELECT 'host-1', $1::timestamptz - i * interval '1 minute', 12, 8, CASE WHEN i <= 60 THEN 10 ELSE 50 END, 1, 107374182400, 75161927680
		FROM generate_series(1, 120) i`, now); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := d.Owner.Exec(t.Context(), `INSERT INTO workflow_runs (account_id, workflow_id, version, host, title, input, status,
			created_at, updated_at, launched_at, finished_at)
			SELECT account_id, id, 1, 'host-1', 'past', 'past', 'succeeded', $1, $1, $1, $2 FROM workflows WHERE id = $3`,
			now.Add(-60*time.Minute-30*time.Second), now.Add(-30*time.Second), wf); err != nil {
			t.Fatal(err)
		}
	}
	code, r := sqlPost(t, h, q("SELECT cpu_pct_learned, cpu_learned, cpu_pct_per_run FROM host_run_footprint WHERE host = 'host-1'"))
	if code != http.StatusOK || r.Rows[0][0] != float64(-20) || r.Rows[0][1] != false || r.Rows[0][2] != float64(10) {
		t.Fatalf("footprint = %d %+v, want the -20 fit kept but not learned, and the default 10 used", code, r)
	}
}

// TestAFinderRunKeepsItsReservationThroughItsOrchestrator: a run that finds its project first
// starts twice on its host, its finder and then its workspace and orchestrator; it counts as
// starting until the host has made the second as well, not only the first.
func TestAFinderRunKeepsItsReservationThroughItsOrchestrator(t *testing.T) {
	d := dbtest.New(t)
	clock := &fakeClock{t: time.Now().UTC().Truncate(time.Minute)}
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.Now = clock.Now; c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	b := openStream(t, h, "host-1")
	start := clock.Now()
	hostMinute(t, b, start.Add(-time.Minute), 10, 0.3)
	if queued := burst(t, h, createWorkflow(t, h, workflow.Starting), 10); queued != 3 {
		t.Fatalf("%d of 10 queued, want 3", queued)
	}
	var finders []hostclient.Command
	for range 7 {
		finders = append(finders, b.command())
	}
	clock.Add(2 * time.Minute)
	for i, c := range finders {
		succeed(t, b, c, fmt.Sprintf(`{"agentId":"finder-%d"}`, i))
	}
	code, r := sqlPost(t, h, q("SELECT id::text FROM workflow_runs WHERE status = 'finding_project'"))
	if code != http.StatusOK || len(r.Rows) != 7 {
		t.Fatalf("finding = %d %+v", code, r)
	}
	for _, row := range r.Rows {
		if code, msg := move(t, h, key, row[0].(string), map[string]string{"move": "project", "directory": "/w/app"}); code != http.StatusOK {
			t.Fatalf("project: %d %s", code, msg)
		}
	}
	// a quiet minute after the finders were made, but none of the 7 workspaces is made yet
	clock.Add(2 * time.Minute)
	hostMinute(t, b, start.Add(3*time.Minute), 10, 0.3)
	if code, r = sqlPost(t, h, q("SELECT count(*) FROM workflow_run_queue")); code != http.StatusOK || r.Rows[0][0] != float64(3) {
		t.Fatalf("runs whose orchestrators the host has not started should keep their place: %+v", r)
	}
}

// TestAFinderRunIsReservedWhileItFindsItsProject: a run is starting the whole time it finds its
// project, so a newer minute after its finder was made, but before the project is chosen and the
// orchestrator asked for, does not release the queue.
func TestAFinderRunIsReservedWhileItFindsItsProject(t *testing.T) {
	d := dbtest.New(t)
	clock := &fakeClock{t: time.Now().UTC().Truncate(time.Minute)}
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.Now = clock.Now; c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	b := openStream(t, h, "host-1")
	start := clock.Now()
	hostMinute(t, b, start.Add(-time.Minute), 10, 0.3)
	if queued := burst(t, h, createWorkflow(t, h, workflow.Starting), 10); queued != 3 {
		t.Fatalf("%d of 10 queued, want 3", queued)
	}
	var finders []hostclient.Command
	for range 7 {
		finders = append(finders, b.command())
	}
	clock.Add(time.Minute)
	for i, c := range finders {
		succeed(t, b, c, fmt.Sprintf(`{"agentId":"finder-%d"}`, i))
	}
	// a quiet minute after the finders were made, before any project is chosen
	clock.Add(2 * time.Minute)
	hostMinute(t, b, start.Add(2*time.Minute), 10, 0.3)
	if code, r := sqlPost(t, h, q("SELECT count(*) FROM workflow_run_queue")); code != http.StatusOK || r.Rows[0][0] != float64(3) {
		t.Fatalf("runs still finding their project should keep their place: %+v", r)
	}
	code, r := sqlPost(t, h, q("SELECT id::text FROM workflow_runs WHERE status = 'finding_project'"))
	if code != http.StatusOK || len(r.Rows) != 7 {
		t.Fatalf("finding = %d %+v", code, r)
	}
	for _, row := range r.Rows {
		if code, msg := move(t, h, key, row[0].(string), map[string]string{"move": "project", "directory": "/w/app"}); code != http.StatusOK {
			t.Fatalf("project: %d %s", code, msg)
		}
	}
	if code, r = sqlPost(t, h, q("SELECT count(*) FROM workflow_run_queue")); code != http.StatusOK || r.Rows[0][0] != float64(3) {
		t.Fatalf("runs whose orchestrators are not made yet should keep their place: %+v", r)
	}
}

// lastMinute records host-1's only minute of resources, ago old, at cpu % busy and a load of 9 on 8 CPUs.
func lastMinute(t *testing.T, d dbtest.DB, ago time.Duration, cpu float64) time.Time {
	t.Helper()
	m := time.Now().UTC().Truncate(time.Minute).Add(-ago)
	if _, err := d.Owner.Exec(t.Context(), `INSERT INTO host_minutes (host, minute, samples, cpus, cpu_avg_pct, load1, mem_total, mem_available_min)
		VALUES ('host-1', $1, 12, 8, $2, 9, 100, 50)`, m, cpu); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestAHostAwayForLongKeepsItsQueue: a host that measured itself busy and then went away (it is
// not connected) keeps holding its runs however long it is gone: they would all land on it at once
// when it came back.
func TestAHostAwayForLongKeepsItsQueue(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	m := lastMinute(t, d, 40*time.Minute, 92)
	if _, err := d.Owner.Exec(t.Context(), "UPDATE hosts SET stream_id = NULL WHERE name = 'host-1'"); err != nil {
		t.Fatal(err)
	}
	if queued := burst(t, h, fixedWorkflow(t, h), 5); queued != 5 {
		t.Fatalf("%d of 5 queued, want all", queued)
	}
	code, r := sqlPost(t, h, q("SELECT queue_reason FROM workflow_run_queue WHERE position = 1"))
	want := "host-1 has no room yet: waiting for host-1 to report again (last measured " + m.Format("15:04 MST") + ")"
	if code != http.StatusOK || r.Rows[0][0] != want {
		t.Errorf("reason = %+v, want %q", r, want)
	}
}

// TestAConnectedHostThatSendsNoResourcesStartsRuns: a host that is connected but has sent no
// minute for long (its monitoring stopped) is no sign of being near capacity: its runs start.
func TestAConnectedHostThatSendsNoResourcesStartsRuns(t *testing.T) {
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) { c.QueueInterval = time.Hour })
	sendReport(t, h, wfReport())
	lastMinute(t, d, 40*time.Minute, 92)
	if queued := burst(t, h, fixedWorkflow(t, h), 5); queued != 0 {
		t.Fatalf("%d of 5 queued, want none", queued)
	}
}
