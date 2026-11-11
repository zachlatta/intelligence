package web

import (
	"fmt"
	"html"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// Optional real-browser regression: install Playwright in scratch, then set
// MODEL_GROUP_PLAYWRIGHT to its module directory and PLAYWRIGHT_BROWSERS_PATH to its browsers.
func TestModelGroupsBrowser(t *testing.T) {
	module := os.Getenv("MODEL_GROUP_PLAYWRIGHT")
	if module == "" {
		t.Skip("set MODEL_GROUP_PLAYWRIGHT for the headless layout regression")
	}
	h, d := dbServer(t)
	report := wfReport()
	report.Paseo.Providers = []hostclient.Provider{{Name: "codex", Available: true, Models: []hostclient.Model{{ID: "gpt-6.1-sol", Label: "GPT 6.1 Sol", Thinking: []string{"high"}}}}}
	sendReport(t, h, report)
	for _, query := range []string{
		`INSERT INTO usage_accounts(provider,account,label,first_seen,last_seen) VALUES('codex','one','Test plan',now(),now())`,
		`INSERT INTO usage_sources(host,provider,account,ok,received_at) VALUES('host-1','codex','one',true,now())`,
		`INSERT INTO usage_active(host,provider,account,since,until) VALUES('host-1','codex','one',now(),now())`,
		`INSERT INTO usage_readings(provider,account,window_id,window_seconds,used_pct,resets_at,first_observed,last_observed) VALUES('codex','one','7d',604800,30,now()+interval '1 day',now(),now())`,
	} {
		if _, err := d.Owner.Exec(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"Frontier", "A deliberately long frontier model group name"} {
		rec := call(t, h, http.MethodPost, "/api/model-groups", `{"name":"`+name+`","members":[{"provider":"codex","model":"gpt-6.1-sol","thinking":"high","login":"one","longWindow":"7d","enabled":true}]}`, sameOriginFetch)
		if rec.Code != 201 {
			t.Fatal(rec.Body)
		}
	}
	definition := workflow.Starting
	definition.Steps = append([]workflow.Step(nil), definition.Steps...)
	definition.Project = workflow.Project{Mode: "fixed", Directory: "/w/app"}
	definition.Steps[0].Instructions = "Launch the builder with {{frontier_model}} and a second opinion with {{frontier_model}}."
	wf := createWorkflow(t, h, definition)
	run := startRun(t, h, wf, "Test the layout")
	var prompt string
	if err := d.Owner.QueryRow(t.Context(), `SELECT args->'agent'->>'prompt' FROM commands WHERE id=(SELECT pending_command FROM workflow_runs WHERE id=$1)`, run).Scan(&prompt); err != nil {
		t.Fatal(err)
	}
	_, instructions, ok := strings.Cut(prompt, "Step 1 of 6: Do the work\n\n")
	if !ok {
		t.Fatal("missing step prompt")
	}
	instructions, _, _ = strings.Cut(instructions, "\n\n"+workflow.UnattendedShellRule)
	inline, notes, ok := strings.Cut(instructions, "\n\nModel suggestions:\n")
	if !ok {
		t.Fatal("missing appended notes")
	}
	// The test fixture injects its test-only session. No production credential is needed.
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/test/model-instructions" {
			// Render the real queued command's resolved instructions separately from its notes.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/ui.css"><main class="container page narrow"><h1>Resolved step instructions</h1><p id="model-inline">%s</p><section class="card"><div class="card-head"><h2>Model suggestions</h2></div><pre class="card-body" id="model-notes" style="white-space:pre-wrap;overflow-wrap:anywhere">%s</pre></section></main>`, html.EscapeString(inline), html.EscapeString(notes))
			return
		}
		signedIn(h, r)
		h.ServeHTTP(w, r)
	}))
	driver, err := filepath.Abs("../../scripts/test-model-groups-ui.cjs")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "node", driver)
	cmd.Env = append(os.Environ(), "MODEL_GROUP_TEST_URL="+srv.URL, "MODEL_GROUP_TEST_WORKFLOW="+wf, "MODEL_GROUP_TEST_RUN="+run)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("browser regression: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}
