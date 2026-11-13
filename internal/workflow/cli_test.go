package workflow

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProposalAnswerCLIValidation(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	path, body, err := parseAnswerProposalCLI([]string{"--proposal", id, "--question", "42", "--note", "-"}, strings.NewReader("## Answer\nBecause."))
	if err != nil || path != "/api/workflow-proposals/"+id+"/comments/agent" {
		t.Fatalf("path: %s %v", path, err)
	}
	var got struct {
		Text       string
		QuestionID int64
	}
	if err = json.Unmarshal(body, &got); err != nil || got.QuestionID != 42 || got.Text != "## Answer\nBecause." {
		t.Fatalf("body: %s %v", body, err)
	}
	for _, args := range [][]string{{"--proposal", id, "--question", "0", "--note", "-"}, {"--proposal", "../other", "--question", "1", "--note", "-"}, {"--proposal", id, "--question", "1"}} {
		if _, _, err := parseAnswerProposalCLI(args, strings.NewReader("text")); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if _, _, err := parseAnswerProposalCLI([]string{"--proposal", id, "--question", "42", "--note", "-"}, strings.NewReader(strings.Repeat("a", 20001))); err == nil {
		t.Fatal("accepted oversized answer")
	}
}

func TestProposalCLIUsesHostConfiguration(t *testing.T) {
	var paths []string
	var bodies []map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer host-key" {
			t.Error("missing host key")
		}
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"demo","approvalURL":"https://example.com/review/demo"}`))
	}))
	defer s.Close()
	home := boxHome(t, s.URL)
	file := filepath.Join(t.TempDir(), "proposal.json")
	os.WriteFile(file, []byte(`{"operation":"delete","summary":"Remove unused","workflowId":"demo","baseVersion":1}`), 0600)
	for _, args := range [][]string{{"propose", file}, {"withdraw", "demo", "--reason", "Replaced by a better plan"}} {
		code, out, errOut := runCLI(t, home, "", args...)
		if code != 0 || !strings.Contains(out, "demo") {
			t.Fatalf("%v: %d %s %s", args, code, out, errOut)
		}
	}
	if paths[0] != "/api/workflow-proposals" || paths[1] != "/api/workflow-proposals/demo/withdraw" || bodies[1]["reason"] != "Replaced by a better plan" {
		t.Fatalf("requests: %v %+v", paths, bodies)
	}
}

func TestCLISuggestDoesNotAdvanceRun(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workflow-runs/run-1/suggestions" {
			t.Errorf("path: %s", r.URL.Path)
		}
		var body struct {
			Token   string
			Exclude []string
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Token != "frontier_model" || len(body.Exclude) != 1 || body.Exclude[0] != "claude/opus" {
			t.Errorf("request: %+v", body)
		}
		json.NewEncoder(w).Encode(map[string]string{"message": "use codex"})
	}))
	defer s.Close()
	code, out, err := runCLI(t, boxHome(t, s.URL), "", "suggest", "--run", "run-1", "frontier_model", "--exclude", "claude/opus")
	if code != 0 || !strings.Contains(out, "use codex") {
		t.Fatalf("suggest: %d %s %s", code, out, err)
	}
}

// fakeServer answers moves with what it was sent, and records each request.
func fakeServer(t *testing.T, code int) (*httptest.Server, *[]*http.Request, *[]Move) {
	t.Helper()
	var reqs []*http.Request
	var moves []Move
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m Move
		json.NewDecoder(r.Body).Decode(&m)
		reqs, moves = append(reqs, r), append(moves, m)
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"message": "moved: " + m.Move})
	}))
	t.Cleanup(s.Close)
	return s, &reqs, &moves
}

// boxHome is a home directory set up as `deploy/install-host` leaves it: the env file and the key.
func boxHome(t *testing.T, server string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "intelligence")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "host.env"), []byte("# the server\nHOST_SERVER="+server+"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "host.key"), []byte("host-key\n"), 0o600)
	return home
}

func runCLI(t *testing.T, home, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	env := map[string]string{"HOME": home}
	code := CLI(t.Context(), args, func(k string) string { return env[k] }, strings.NewReader(stdin), &out, &errb, http.DefaultClient)
	return code, out.String(), errb.String()
}

func TestTheCLISendsEachMoveWithTheBoxsKey(t *testing.T) {
	s, reqs, moves := fakeServer(t, http.StatusOK)
	home := boxHome(t, s.URL)
	note := filepath.Join(t.TempDir(), "note.md")
	os.WriteFile(note, []byte("## A question"), 0o600)
	for _, c := range []struct {
		args []string
		want Move
	}{
		{[]string{"project", "--run", "r1", "/srv/dev/app"}, Move{Move: "project", Directory: "/srv/dev/app"}},
		{[]string{"project", "--run", "r1", "none"}, Move{Move: "project", Directory: "none"}},
		{[]string{"next", "--run", "r1", "--summary", "built it"}, Move{Move: "next", Summary: "built it"}},
		{[]string{"summarize", "--run", "r1", "--summary", "Summarize workflow run titles"}, Move{Move: "summarize", Summary: "Summarize workflow run titles"}},
		{[]string{"back", "--run", "r1", "build", "--why", "no test"}, Move{Move: "back", Step: "build", Why: "no test"}},
		{[]string{"back", "--why", "no test", "--run", "r1", "build"}, Move{Move: "back", Step: "build", Why: "no test"}},
		{[]string{"skip", "--run", "r1", "--why", "only what he asked"}, Move{Move: "skip", Why: "only what he asked"}},
		{[]string{"ask", "--run", "r1", "--note", note}, Move{Move: "ask", Note: "## A question"}},
		{[]string{"done", "--run", "r1"}, Move{Move: "done"}},
		{[]string{"fail", "--run", "r1", "--why", "no such repo"}, Move{Move: "fail", Why: "no such repo"}},
	} {
		code, out, errOut := runCLI(t, home, "", c.args...)
		if code != 0 || out != "moved: "+c.want.Move+"\n" {
			t.Errorf("%v: %d %q %q", c.args, code, out, errOut)
			continue
		}
		r, m := (*reqs)[len(*reqs)-1], (*moves)[len(*moves)-1]
		if r.URL.Path != "/api/workflow-runs/r1/moves" || r.Header.Get("Authorization") != "Bearer host-key" || m != c.want {
			t.Errorf("%v: sent %s %s %+v, want %+v", c.args, r.URL.Path, r.Header.Get("Authorization"), m, c.want)
		}
	}
	if code, _, _ := runCLI(t, home, "## From stdin", "ask", "--run", "r1", "--note", "-"); code != 0 || (*moves)[len(*moves)-1].Note != "## From stdin" {
		t.Errorf("--note - should read stdin: %+v", (*moves)[len(*moves)-1])
	}
}

func TestTheCLIRefusesWhatItCannotSend(t *testing.T) {
	s, reqs, _ := fakeServer(t, http.StatusOK)
	home := boxHome(t, s.URL)
	for _, args := range [][]string{
		{},
		{"next"},                              // no run
		{"jump", "--run", "r1"},               // no such move
		{"back", "--run", "r1", "--why", "x"}, // no step
		{"project", "--run", "r1"},            // no directory
		{"ask", "--run", "r1"},                // no note
	} {
		if code, _, errOut := runCLI(t, home, "", args...); code == 0 || errOut == "" {
			t.Errorf("%v: %d, want a usage error", args, code)
		}
	}
	if len(*reqs) != 0 {
		t.Errorf("sent %d requests", len(*reqs))
	}
	if code, _, errOut := runCLI(t, t.TempDir(), "", "next", "--run", "r1"); code == 0 || !strings.Contains(errOut, "HOST_SERVER") {
		t.Errorf("no configuration: %d %q", code, errOut)
	}
}

func TestTheCLIShowsTheServersRefusal(t *testing.T) {
	s, _, _ := fakeServer(t, http.StatusConflict)
	code, out, errOut := runCLI(t, boxHome(t, s.URL), "", "next", "--run", "r1")
	if code == 0 || out != "" || !strings.Contains(errOut, "moved: next") {
		t.Errorf("%d %q %q", code, out, errOut)
	}
}

// TestTheCLIReadsAHostInstalledAsADevBox: a host installed before hosts were called dev boxes has
// devbox.env and devbox.key.
func TestTheCLIReadsAHostInstalledAsADevBox(t *testing.T) {
	s, reqs, _ := fakeServer(t, http.StatusOK)
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "intelligence")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "devbox.env"), []byte("DEVBOX_SERVER="+s.URL+"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "devbox.key"), []byte("old-key"), 0o600)
	if code, _, errOut := runCLI(t, home, "", "next", "--run", "r1"); code != 0 || (*reqs)[0].Header.Get("Authorization") != "Bearer old-key" {
		t.Errorf("%d %q", code, errOut)
	}
}

func TestRunAnswerCLI(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	args := []string{"--run", id, "--channel", "email", "--message-id", "sent-demo", "--thread-id", "thread-demo", "--sent-at", "2026-11-11T20:00:00Z", "--checked"}
	path, body, err := parseAnswerCLI(args)
	if err != nil || path != "/api/workflow-runs/"+id+"/answers" {
		t.Fatalf("%s %v", path, err)
	}
	var a Answer
	if json.Unmarshal(body, &a) != nil || !a.Checked || a.MessageID != "sent-demo" {
		t.Fatalf("%s", body)
	}
	for _, args := range [][]string{nil, {"--run", "../x"}, {"--run", id, "--sent-at", "bad"}} {
		if _, _, err := parseAnswerCLI(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	msg := StepMessage(Starting, 0, Vars{Run: id})
	if !strings.Contains(msg, "workflow answer --run "+id) {
		t.Fatal("missing answer instruction")
	}
}

func TestAnswerCLIRecordsSteeredAndNotNeeded(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	to := "22222222-2222-4222-8222-222222222222"
	path, body, err := parseAnswerCLI([]string{"--run", id, "--steered-to", to, "--sent-at", "Thu, 12 Nov 2026 18:24:00 -0500", "--reason", "Folded into the deck run"})
	if err != nil || path != "/api/workflow-runs/"+id+"/answer-resolution" {
		t.Fatalf("steered: %s %v", path, err)
	}
	var r AnswerResolution
	if json.Unmarshal(body, &r) != nil || r.Kind != "steered" || r.SteeredTo != to || r.Reason != "Folded into the deck run" || r.At.UTC().Format(time.RFC3339) != "2026-11-12T23:24:00Z" {
		t.Fatalf("steered body: %s", body)
	}
	path, body, err = parseAnswerCLI([]string{"--run", id, "--not-needed", "A recorded meeting, not a request"})
	if err != nil || path != "/api/workflow-runs/"+id+"/answer-resolution" {
		t.Fatalf("not needed: %s %v", path, err)
	}
	r = AnswerResolution{}
	if json.Unmarshal(body, &r) != nil || r.Kind != "not_needed" || r.Reason != "A recorded meeting, not a request" || !r.At.IsZero() {
		t.Fatalf("not needed body: %s", body)
	}
	for _, args := range [][]string{
		{"--run", id, "--steered-to", "not-a-run"},
		{"--run", id, "--steered-to", id},
		{"--run", id, "--not-needed", "  "},
		{"--run", id, "--steered-to", to, "--not-needed", "both"},
		{"--run", id, "--steered-to", to, "--channel", "email", "--message-id", "sent"},
		{"--run", id, "--not-needed", "x", "--checked"},
		{"--run", id, "--channel", "email", "--message-id", "sent", "--reason", "only with a resolution"},
	} {
		if _, _, err := parseAnswerCLI(args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	for _, want := range []string{"--steered-to", "--not-needed"} {
		if !strings.Contains(RunAnswerRule, want) || !strings.Contains(CLIUsage, want) {
			t.Errorf("instructions or usage omit %s", want)
		}
	}
}

func TestRunAnswerCLIAcceptsMailDatesAndOmittedTime(t *testing.T) {
	for _, date := range []string{"", "Thu, 12 Nov 2026 14:13:23 -0500", "Thu, 12 Nov 2026 14:13:23 -0500 (EST)"} {
		args := []string{"--run", "11111111-1111-4111-8111-111111111111", "--channel", "email", "--message-id", "sent-example"}
		if date != "" {
			args = append(args, "--sent-at", date)
		}
		_, body, err := parseAnswerCLI(args)
		if err != nil {
			t.Errorf("date %q: %v", date, err)
			continue
		}
		var a Answer
		if err := json.Unmarshal(body, &a); err != nil {
			t.Fatal(err)
		}
		if date != "" && a.SentAt.UTC().Format("2006-01-02T15:04:05Z") != "2026-11-12T19:13:23Z" {
			t.Fatalf("wrong mail date: %s", body)
		}
	}
}

func TestAnswerInstructionsCoverRequestersAndEarlyExits(t *testing.T) {
	for _, want := range []string{"requester", "fail", "skip", "--sent-at", "RFC 5322", "server"} {
		if !strings.Contains(RunAnswerRule, want) {
			t.Errorf("answer instructions miss %q", want)
		}
	}
	if !strings.Contains(DeliverableEmailRule, "intelligence workflow answer") {
		t.Error("email delivery instructions omit recording")
	}
	prompt := OrchestratorPrompt("Example", Starting, Vars{Run: "run-example"})
	commandList, _, _ := strings.Cut(prompt, "next after the last step")
	if !strings.Contains(commandList, "intelligence workflow answer --run run-example") {
		t.Error("command list omits answer")
	}
}
