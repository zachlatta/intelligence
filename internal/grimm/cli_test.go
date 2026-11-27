package grimm

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

func TestCLISendsEachCommandAsTheCallingAgent(t *testing.T) {
	t.Parallel()
	var got []Call
	var agents, auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != AgentPath || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var c Call
		_ = json.NewDecoder(r.Body).Decode(&c)
		got = append(got, c)
		agents = append(agents, r.Header.Get(AgentHeader))
		auth = append(auth, r.Header.Get("Authorization"))
		if c.Verb == "stop" {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "hermes is not running"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "ok " + c.Verb})
	}))
	defer srv.Close()
	dir := t.TempDir()
	key := filepath.Join(dir, "host.key")
	if err := os.WriteFile(key, []byte("hostkey\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": dir, "HOST_SERVER": srv.URL, "HOST_KEY_FILE": key, "PASEO_AGENT_ID": "agent-7"}
	run := func(stdin string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := CLI(t.Context(), args, func(k string) string { return env[k] }, strings.NewReader(stdin), &out, &errb, srv.Client())
		return code, out.String(), errb.String()
	}
	if code, out, _ := run("", "ctl", "quota"); code != 0 || out != "ok quota\n" {
		t.Fatalf("quota: %d %q", code, out)
	}
	if code, _, e := run("Build the parser.\nWith tests.\n", "ctl", "hire", "--role", "builder", "--name", "ash", "--provider", "codex", "-"); code != 0 {
		t.Fatalf("hire: %d %s", code, e)
	}
	if code, _, e := run("", "ctl", "say", "ash", "use", "tabs"); code != 0 {
		t.Fatalf("say: %d %s", code, e)
	}
	if code, _, e := run("", "ctl", "--state-dir", "/x", "peek", "ash", "--lines", "5"); code != 0 {
		t.Fatalf("peek: %d %s", code, e)
	}
	if code, _, e := run("", "ctl", "wait", "ash", "iris", "--timeout", "30"); code != 0 {
		t.Fatalf("wait: %d %s", code, e)
	}
	if code, _, e := run("", "ctl", "directive", "add", "--from", "ash", "Always", "run", "go", "vet"); code != 0 {
		t.Fatalf("directive: %d %s", code, e)
	}
	if code, _, e := run("Parser done.\n", "bus", "post", "--from", "ash", "--to", "grimm", "--kind", "result", "-"); code != 0 {
		t.Fatalf("post: %d %s", code, e)
	}
	if code, _, e := run("", "bus", "read", "--limit", "10"); code != 0 {
		t.Fatalf("read: %d %s", code, e)
	}
	if code, _, e := run("", "ctl", "stop", "hermes"); code != 1 || !strings.Contains(e, "hermes is not running") {
		t.Fatalf("a refusal: %d %q", code, e)
	}
	if code, _, _ := run("", "ctl", "frobnicate"); code != 2 {
		t.Fatalf("unknown command: %d", code)
	}
	want := []Call{
		{Verb: "quota"},
		{Verb: "hire", Role: "builder", Name: "ash", Provider: "codex", Text: "Build the parser.\nWith tests."},
		{Verb: "say", Args: []string{"ash"}, Text: "use tabs"},
		{Verb: "peek", Args: []string{"ash"}, Lines: 5},
		{Verb: "wait", Args: []string{"ash", "iris"}, Timeout: 30},
		{Verb: "directive", Args: []string{"add"}, Text: "Always run go vet"},
		{Verb: "bus-post", To: "grimm", Kind: "result", Text: "Parser done."},
		{Verb: "bus-read", Lines: 10},
		{Verb: "stop", Args: []string{"hermes"}},
	}
	if len(got) != len(want) {
		t.Fatalf("calls = %+v", got)
	}
	for i := range want {
		g, _ := json.Marshal(got[i])
		w, _ := json.Marshal(want[i])
		if string(g) != string(w) {
			t.Errorf("call %d = %s, want %s", i, g, w)
		}
		if agents[i] != "agent-7" || auth[i] != "Bearer hostkey" {
			t.Errorf("call %d as %q with %q", i, agents[i], auth[i])
		}
	}
	env["PASEO_AGENT_ID"] = ""
	if code, _, e := run("", "ctl", "roster"); code != 1 || !strings.Contains(e, "PASEO_AGENT_ID") {
		t.Fatalf("outside an agent: %d %q", code, e)
	}
}
