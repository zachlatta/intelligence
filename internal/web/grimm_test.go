package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/hostclient"
)

// grimmRig streams as host-1 with Claude Opus 5.5 in a model group and headroom on it.
func grimmRig(t *testing.T, h http.Handler) *rawHost {
	t.Helper()
	d := h.(*testServer).d
	rep := wfReport()
	rep.Paseo.Providers = []hostclient.Provider{{Name: "claude", Available: true, Models: []hostclient.Model{{ID: "claude-opus-5-5", Thinking: []string{"medium"}}}}}
	stream := openStream(t, h, "host-1")
	if ack := stream.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	for _, q := range []string{
		`INSERT INTO usage_accounts(provider,account,first_seen,last_seen) VALUES('claude','one',now(),now())`,
		`INSERT INTO usage_sources(host,provider,account,ok,received_at) VALUES('host-1','claude','one',true,now())`,
		`INSERT INTO usage_readings(provider,account,window_id,window_seconds,used_pct,resets_at,first_observed,last_observed) VALUES('claude','one','7d',604800,30,now()+interval '1 day',now(),now())`,
		`INSERT INTO usage_active(host,provider,account,since,until) VALUES('host-1','claude','one',now(),now())`,
	} {
		if _, err := d.Owner.Exec(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"name":"Opus","members":[{"provider":"claude","model":"claude-opus-5-5","thinking":"medium","login":"one","longWindow":"7d","enabled":true}]}`
	if rec := call(t, h, "POST", "/api/model-groups", body, sameOriginFetch); rec.Code != 201 {
		t.Fatalf("model group: %d %s", rec.Code, rec.Body)
	}
	return stream
}

func TestGrimmIsTheHomePageAndHostsMoveToHosts(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	grimmRig(t, h)
	home := get(t, h, "/", nil)
	body := home.Body.String()
	if home.Code != 200 || !strings.Contains(body, "Start a Grimm session") || !strings.Contains(body, `<option value="host-1"`) ||
		!strings.Contains(body, `href="/" aria-current="page">Grimm</a>`) || !strings.Contains(body, `/grimm.js`) {
		t.Fatalf("home page: %d\n%s", home.Code, body)
	}
	if !strings.Contains(body, `value="20"`) {
		t.Error("the new-session form does not default the cap to 20")
	}
	hosts := get(t, h, "/hosts", nil).Body.String()
	if !strings.Contains(hosts, "Login fix") || !strings.Contains(hosts, `href="/hosts" aria-current="page">Hosts</a>`) {
		t.Fatalf("hosts page:\n%s", hosts)
	}
}

func TestGrimmSessionStreamsItsThreadAndState(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	stream := grimmRig(t, h)
	rec := call(t, h, "POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/app","maxWorkers":12}`, sameOriginFetch)
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct{ ID, URL string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	ws := stream.command()
	if ws.Kind != hostclient.CreateWorkspaceKind {
		t.Fatalf("first command = %+v", ws)
	}
	succeed(t, stream, ws, `{"workspaceId":"w-app"}`)

	page := get(t, h, created.URL, nil).Body.String()
	if !strings.Contains(page, `data-session="`+created.ID+`"`) || !strings.Contains(page, `id="g-cap"`) {
		t.Fatalf("session page:\n%s", page)
	}
	if home := get(t, h, "/", nil).Body.String(); !strings.Contains(home, `data-session="`+created.ID+`"`) {
		t.Error("the home page does not open the newest session")
	}

	srv := serve(t, h)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/grimm/sessions/"+created.ID+"/stream", nil)
	req.AddCookie(h.(*testServer).cookie)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header)
	}
	events := make(chan [2]string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				ev = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				events <- [2]string{ev, v}
			}
		}
		close(events)
	}()
	wait := func(what string, ok func(ev, data string) bool) {
		t.Helper()
		for {
			select {
			case e, open := <-events:
				if !open {
					t.Fatalf("stream ended waiting for %s", what)
				}
				if ok(e[0], e[1]) {
					return
				}
			case <-ctx.Done():
				t.Fatalf("no %s", what)
			}
		}
	}
	wait("the first state", func(ev, data string) bool {
		return ev == "state" && strings.Contains(data, `"max_workers":12`) && strings.Contains(data, `"manager_state":"offline"`)
	})
	if rec := call(t, h, "POST", "/api/grimm/sessions/"+created.ID+"/messages", `{"text":"Build the CSV tool"}`, sameOriginFetch); rec.Code != 202 {
		t.Fatalf("message: %d %s", rec.Code, rec.Body)
	}
	wait("the message, pushed", func(ev, data string) bool { return ev == "thread" && strings.Contains(data, "Build the CSV tool") })
	mgr := stream.command() // the supervisor starts the manager once the worktree is ready
	var args hostclient.CreateAgentArgs
	_ = json.Unmarshal(mgr.Args, &args)
	if mgr.Kind != hostclient.CreateAgentKind || args.Model != "claude-opus-5-5" || args.Labels[hostclient.LiveLabel] != "1" {
		t.Fatalf("manager = %s %s", mgr.Kind, mgr.Args)
	}
	succeed(t, stream, mgr, `{"agentId":"mgr-1"}`)
	rep := wfReport()
	rep.Paseo.Providers = []hostclient.Provider{{Name: "claude", Available: true, Models: []hostclient.Model{{ID: "claude-opus-5-5", Thinking: []string{"medium"}}}}}
	rep.Paseo.Agents = append(rep.Paseo.Agents, hostclient.Agent{ID: "mgr-1", WorkspaceID: "w-app", Provider: "claude", Status: "running", CreatedAt: time.Now()})
	if ack := stream.call(hostclient.Frame{Type: hostclient.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	wait("the manager working", func(ev, data string) bool {
		return ev == "state" && strings.Contains(data, `"manager_state":"working"`)
	})
	// the manager's words stream into the thread as its transcript grows
	batch := hostclient.TranscriptBatch{Host: "host-1", AgentID: "mgr-1", Epoch: "e1", Replace: true, Entries: []hostclient.TranscriptEntry{
		{SeqStart: 1, SeqEnd: 1, At: time.Now(), Item: json.RawMessage(`{"type":"user_message","text":"first prompt"}`)},
		{SeqStart: 2, SeqEnd: 2, At: time.Now(), Item: json.RawMessage(`{"type":"assistant_message","text":"Hiring a builder"}`)}}}
	if ack := stream.call(hostclient.Frame{Type: hostclient.TranscriptFrame, Transcript: &batch}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	wait("the manager's words", func(ev, data string) bool {
		return ev == "thread" && strings.Contains(data, "Hiring a builder") && !strings.Contains(data, "first prompt")
	})
	batch = hostclient.TranscriptBatch{Host: "host-1", AgentID: "mgr-1", Epoch: "e1", Entries: []hostclient.TranscriptEntry{
		{SeqStart: 2, SeqEnd: 3, At: time.Now(), Item: json.RawMessage(`{"type":"assistant_message","text":"Hiring a builder and a verifier"}`)}}}
	if ack := stream.call(hostclient.Frame{Type: hostclient.TranscriptFrame, Transcript: &batch}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	wait("the words growing in place", func(ev, data string) bool { return ev == "thread" && strings.Contains(data, "and a verifier") })
}

func TestGrimmRoutesKeepToTheAccountAndTheCallingAgent(t *testing.T) {
	t.Parallel()
	h, _ := dbServer(t)
	grimmRig(t, h)
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/api/grimm/sessions", `{"host":"host-9","repo":"/w/app"}`, 400},
		{"POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/app","maxWorkers":500}`, 400},
		{"POST", "/api/grimm/sessions/00000000-0000-4000-8000-000000000009/messages", `{"text":"hi"}`, 404},
		{"GET", "/grimm/00000000-0000-4000-8000-000000000009", "", 404},
	} {
		if rec := call(t, h, tc.method, tc.path, tc.body, sameOriginFetch); rec.Code != tc.want {
			t.Errorf("%s %s: %d %s, want %d", tc.method, tc.path, rec.Code, rec.Body, tc.want)
		}
	}
	if rec := call(t, h, "POST", "/api/grimm/sessions", `{"host":"host-1","repo":"/w/app"}`, map[string]string{"Sec-Fetch-Site": "cross-site"}); rec.Code != 403 {
		t.Errorf("a cross-site create: %d", rec.Code)
	}
	if rec := call(t, anon(h), "POST", "/api/grimm/agent", `{"verb":"roster"}`, nil); rec.Code != 401 {
		t.Errorf("no host key: %d", rec.Code)
	}
	key := keyOf(t, h, "host-1")
	rec := call(t, anon(h), "POST", "/api/grimm/agent", `{"verb":"roster"}`, map[string]string{"Authorization": "Bearer " + key, "X-Paseo-Agent-Id": "someone-else"})
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "not part of an active Grimm session") {
		t.Errorf("an agent outside Grimm: %d %s", rec.Code, rec.Body)
	}
}
