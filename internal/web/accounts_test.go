package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/devbox"
	"github.com/zachlatta/intelligence/internal/paseo"
	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
	"github.com/zachlatta/intelligence/internal/usage"
)

// TestEachAccountSeesOnlyItsOwnBoxes: two accounts, a box each. Every page and API, and SQL,
// shows each account its own box, its workspaces, agents, transcripts and plans, and never the other's.
func TestEachAccountSeesOnlyItsOwnBoxes(t *testing.T) {
	h, _ := dbServer(t)
	ts := h.(*testServer)
	sendReport(t, h, sampleReport) // devbox-1, someone's
	bob, err := ts.store.Create(t.Context(), "bob", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	asBob := sessionFor(t, ts.store, bob)
	bobKey := joinBox(t, ts.store, bob, "bobs-box")
	b, _, err := dialStream(t, serve(t, h), "bobs-box", bobKey)
	if err != nil {
		t.Fatal(err)
	}
	rep := devbox.Report{Name: "bobs-box", CollectedAt: t0, Version: "c1", Paseo: devbox.PaseoStatus{Connected: true,
		Workspaces: []devbox.Workspace{{ID: "w-bob", Name: "Bob's secret project", Kind: "directory", Directory: "/w/bob"}},
		Agents: []devbox.Agent{{ID: "b0000000-0000-4000-8000-000000000001", WorkspaceID: "w-bob", Title: "Bob's agent",
			Provider: "claude", Status: "idle", CreatedAt: t0}}}}
	if ack := b.call(devbox.Frame{Type: devbox.ReportFrame, Report: &rep}); ack.Error != "" {
		t.Fatal(ack.Error)
	}
	if ack := b.call(devbox.Frame{Type: devbox.UsageFrame, Usage: &usage.Report{CollectedAt: time.Now(), Sources: []usage.Source{{
		Provider: "claude", Accounts: []usage.Account{{ID: "bob@example.com", Label: "bob@example.com", Active: true, ObservedAt: time.Now(),
			Windows: []usage.Window{{ID: "7d", Seconds: 7 * 86400, UsedPct: 40}}}}}}}}); ack.Error != "" {
		t.Fatal(ack.Error)
	}

	mine := []string{"devbox-1", "Login fix"}
	bobs := []string{"bobs-box", "Bob&#39;s secret project", "bob@example.com"}
	for _, path := range []string{"/", "/api/devboxes", "/hosts", "/usage", "/api/usage", "/api/hosts",
		"/api/sql?q=" + "SELECT+name+FROM+devboxes+UNION+ALL+SELECT+account+FROM+usage_accounts+UNION+ALL+SELECT+username+FROM+accounts"} {
		someone := get(t, h, path, nil)
		other := call(t, h, http.MethodGet, path, "", nil, asBob)
		if someone.Code != http.StatusOK || other.Code != http.StatusOK {
			t.Fatalf("%s: %d / %d", path, someone.Code, other.Code)
		}
		for _, s := range bobs {
			if strings.Contains(someone.Body.String(), strings.ReplaceAll(s, "&#39;", "'")) || strings.Contains(someone.Body.String(), s) {
				t.Errorf("%s shows someone bob's %q", path, s)
			}
		}
		if strings.Contains(other.Body.String(), "devbox-1") {
			t.Errorf("%s shows bob someone's devbox-1", path)
		}
	}
	if page := get(t, h, "/", nil).Body.String(); !strings.Contains(page, mine[0]) || !strings.Contains(page, mine[1]) {
		t.Errorf("someone's home page misses their own box")
	}
	if page := call(t, h, http.MethodGet, "/", "", nil, asBob).Body.String(); !strings.Contains(page, "bobs-box") {
		t.Errorf("bob's home page misses his box")
	}
	// someone's agent's pages are not found for bob
	for _, path := range []string{"/devboxes/devbox-1/agents/" + agentA1, "/devboxes/devbox-1/workspaces/new", newAgentPath} {
		if rec := call(t, h, http.MethodGet, path, "", nil, asBob); rec.Code != http.StatusNotFound {
			t.Errorf("bob GET %s: %d, want 404", path, rec.Code)
		}
	}
}

// TestADevBoxJoinsAnAccountWithItsCode is the whole join: the box asks, shows its code, the
// account enters it, and the box collects its key (woken by the claim, not polling) and streams.
func TestADevBoxJoinsAnAccountWithItsCode(t *testing.T) {
	h, _ := dbServer(t)
	srv := serve(t, h)
	rec := call(t, anon(h), http.MethodPost, "/api/devbox-enrollments", `{"box":"new-box"}`, nil)
	var e struct{ ID, Code, Secret string }
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Code == "" || e.Secret == "" {
		t.Fatalf("enroll: %d %s", rec.Code, rec.Body)
	}
	keyPath := "/api/devbox-enrollments/" + e.ID + "/key"
	bearer := map[string]string{"Authorization": "Bearer " + e.Secret}
	if rec := call(t, anon(h), http.MethodPost, keyPath, "", bearer); rec.Code != http.StatusAccepted {
		t.Errorf("before the code is entered: %d", rec.Code)
	}
	if rec := call(t, anon(h), http.MethodPost, "/api/devbox-enrollments/claim", `{"code":"`+e.Code+`"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("claim with no one signed in: %d", rec.Code)
	}
	got := make(chan *http.Response, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+keyPath+"?wait=20s", nil)
		req.Header.Set("Authorization", "Bearer "+e.Secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			close(got)
			return
		}
		got <- resp
	}()
	time.Sleep(200 * time.Millisecond) // let the wait start
	start := time.Now()
	claim := call(t, h, http.MethodPost, "/api/devbox-enrollments/claim", `{"code":"`+strings.ToLower(e.Code)+`"}`, sameOriginFetch)
	if claim.Code != http.StatusOK || !strings.Contains(claim.Body.String(), `"new-box"`) {
		t.Fatalf("claim: %d %s", claim.Code, claim.Body)
	}
	var key struct{ Key, Box string }
	select {
	case resp := <-got:
		if resp == nil {
			t.FailNow()
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&key) != nil || key.Key == "" || key.Box != "new-box" {
			t.Fatalf("collect: %d %+v", resp.StatusCode, key)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the key never came")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %s: the wait polled", took)
	}
	if rec := call(t, anon(h), http.MethodPost, keyPath, "", bearer); rec.Code != http.StatusNotFound {
		t.Errorf("a second collect: %d, want 404", rec.Code)
	}
	if _, _, err := dialStream(t, srv, "new-box", key.Key); err != nil {
		t.Fatalf("stream with the new key: %v", err)
	}
	if page := get(t, h, "/account", nil).Body.String(); !strings.Contains(page, "new-box") {
		t.Errorf("the account page does not list the new box")
	}
}

func TestRemovingABoxRevokesItsKeyAndDeletesItsReports(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, sampleReport)
	if rec := call(t, h, http.MethodDelete, "/api/devboxes/devbox-1", "", map[string]string{"Sec-Fetch-Site": "cross-site"}); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site remove: %d", rec.Code)
	}
	if rec := call(t, h, http.MethodDelete, "/api/devboxes/devbox-1", "", sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, http.MethodDelete, "/api/devboxes/devbox-1", "", sameOriginFetch); rec.Code != http.StatusNotFound {
		t.Errorf("remove again: %d", rec.Code)
	}
	if _, resp, err := dialStream(t, serve(t, h), "devbox-1", keyOf(t, h, "devbox-1")); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a removed box streams: %v", err)
	}
	if len(boxes(t, d)) != 0 {
		t.Errorf("a removed box's reports are kept")
	}
}

func TestReadsNeedASignedInAccount(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, sampleReport)
	for _, path := range []string{"/", "/usage", "/hosts", "/account", "/devboxes/devbox-1/agents/" + agentA1} {
		if rec := get(t, anon(h), path, nil); rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login?next=") {
			t.Errorf("GET %s signed out: %d %q", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	for _, path := range []string{"/api/devboxes", "/api/sql?q=SELECT+1", "/api/hosts", "/api/usage",
		"/devboxes/devbox-1/agents/" + agentA1 + "/blocks?from=0"} {
		if rec := get(t, anon(h), path, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s signed out: %d, want 401", path, rec.Code)
		}
	}
	for _, path := range []string{"/healthz", "/login"} {
		if rec := get(t, anon(h), path, nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s signed out: %d, want 200", path, rec.Code)
		}
	}
}

// TestAKeylessDevBoxJoinsAndReports is the whole path from a fresh install: a dev box with no key
// logs a code, someone signed in enters it, and the box collects its key, streams and reports,
// visible to that account.
func TestAKeylessDevBoxJoinsAndReports(t *testing.T) {
	h, d := dbServer(t)
	srv := serve(t, h)
	daemon := paseotest.New(t, "tok")
	daemon.SetWorkspaces(paseotest.Workspace("w-app", "worktree", "running"))
	codes := make(chan string, 8)
	logf := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if _, rest, ok := strings.Cut(msg, "enter the code "); ok {
			codes <- strings.Fields(rest)[0]
		}
		t.Log(msg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	box := &devbox.Box{Name: "fresh-box", Version: "v", Settle: 10 * time.Millisecond, Logf: t.Logf,
		Watcher: &devbox.Watcher{RetryEvery: 20 * time.Millisecond, Dial: func(ctx context.Context) (*paseo.Client, error) {
			return paseo.Dial(ctx, paseo.Target{URL: daemon.URL(), Token: "tok", ClientID: "test"})
		}},
		Link: &devbox.Link{Server: srv.URL, Box: "fresh-box", RetryEvery: 20 * time.Millisecond,
			Keys: &devbox.Keys{File: filepath.Join(t.TempDir(), "devbox.key"), Server: srv.URL, Box: "fresh-box", Logf: logf,
				RetryEvery: 20 * time.Millisecond}}}
	stopped := make(chan struct{})
	go func() { box.Run(ctx); close(stopped) }()
	t.Cleanup(func() { cancel(); <-stopped })
	var code string
	select {
	case code = <-codes:
	case <-time.After(10 * time.Second):
		t.Fatal("the box never showed a code")
	}
	if rec := call(t, h, http.MethodPost, "/api/devbox-enrollments/claim", `{"code":"`+code+`"}`, sameOriginFetch); rec.Code != http.StatusOK {
		t.Fatalf("claim %q: %d %s", code, rec.Code, rec.Body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if page := get(t, h, "/", nil).Body.String(); strings.Contains(page, "fresh-box") && strings.Contains(page, "w-app") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the box never reported to its account: %+v", boxes(t, d))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
