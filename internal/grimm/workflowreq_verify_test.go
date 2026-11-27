package grimm

// sage's spec tests for `intelligence grimm ctl workflow request`: the command line only. The
// server side (manager-only, a run proposal, browser approval) is tested in internal/web.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyWorkflowRequestParses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "task.md")
	if err := os.WriteFile(file, []byte("Task from a file\nline two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		args  []string
		stdin string
		want  string // the input text the call carries
	}{
		{"positional", []string{"ctl", "workflow", "request", "--workflow", "wf-1", "--summary", "why", "Do", "the", "task"}, "", "Do the task"},
		{"stdin", []string{"ctl", "workflow", "request", "--workflow", "wf-1", "-"}, "Task from stdin\n", "Task from stdin"},
		{"file", []string{"ctl", "workflow", "request", "--workflow=wf-1", "--file", file}, "", "Task from a file\nline two"},
	} {
		c, err := ParseCLI(tc.args, strings.NewReader(tc.stdin))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		b, _ := json.Marshal(c)
		if !strings.Contains(string(b), "wf-1") {
			t.Errorf("%s: the call lost the workflow: %s", tc.name, b)
		}
		if !strings.Contains(c.Text, tc.want) {
			t.Errorf("%s: input = %q, want %q", tc.name, c.Text, tc.want)
		}
		if tc.name == "positional" && !strings.Contains(string(b), "why") {
			t.Errorf("the call lost the summary: %s", b)
		}
	}
	if _, err := ParseCLI([]string{"ctl", "workflow", "request", "Do the task"}, strings.NewReader("")); err == nil {
		t.Error("a request with no workflow parsed")
	}
	if _, err := ParseCLI([]string{"ctl", "workflow", "request", "--workflow", "wf-1"}, strings.NewReader("")); err == nil {
		t.Error("a request with no input parsed")
	}
	if !strings.Contains(CLIUsage, "workflow request") {
		t.Error("the CLI usage does not list workflow request")
	}
}

// The CLI sends the request as the calling agent with the host's key and prints the server's
// answer (the proposal's link), as every other command does.
func TestVerifyWorkflowRequestGoesToTheServerAsTheAgent(t *testing.T) {
	t.Parallel()
	var got Call
	var agent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		agent = r.Header.Get(AgentHeader)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "requested: /workflow-proposals/p-1"})
	}))
	defer srv.Close()
	dir := t.TempDir()
	key := filepath.Join(dir, "host.key")
	_ = os.WriteFile(key, []byte("hostkey\n"), 0o600)
	env := map[string]string{"HOME": dir, "HOST_SERVER": srv.URL, "HOST_KEY_FILE": key, "PASEO_AGENT_ID": "mgr-7"}
	var out, errb bytes.Buffer
	code := CLI(t.Context(), []string{"ctl", "workflow", "request", "--workflow", "wf-1", "Do it"}, func(k string) string { return env[k] }, strings.NewReader(""), &out, &errb, srv.Client())
	if code != 0 || !strings.Contains(out.String(), "/workflow-proposals/p-1") {
		t.Fatalf("exit %d out %q err %q", code, out.String(), errb.String())
	}
	if agent != "mgr-7" || got.Verb == "" {
		t.Fatalf("sent %+v as %q", got, agent)
	}
}
