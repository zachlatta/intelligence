package web

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

func TestProposalAnswerUsesDeskCatalog(t *testing.T) {
	project := func(name, dir string) hostclient.Project {
		return hostclient.Project{ID: name, Name: name, Kind: "git", Directory: dir}
	}
	for _, tc := range []struct {
		name              string
		first, second     []hostclient.Project
		wantHost, wantCwd string
		workspace         bool
		firstOffline      bool
	}{
		{name: "catalog identifier", first: []hostclient.Project{project("Desk", "/catalog/private-notes")}, wantHost: "host-1", wantCwd: "/catalog/private-notes"},
		{name: "desk host beats saved host", first: []hostclient.Project{project("other", "/catalog/desk-copy")}, second: []hostclient.Project{project("Notes", "/catalog/second/desk")}, wantHost: "host-2", wantCwd: "/catalog/second/desk"},
		{name: "saved host wins among desk hosts", first: []hostclient.Project{project("desk", "/catalog/preferred/desk")}, second: []hostclient.Project{project("desk", "/catalog/alternate/desk")}, wantHost: "host-1", wantCwd: "/catalog/preferred/desk"},
		{name: "online desk beats offline saved desk", first: []hostclient.Project{project("desk", "/catalog/preferred/desk")}, second: []hostclient.Project{project("desk", "/catalog/online/desk")}, firstOffline: true, wantHost: "host-2", wantCwd: "/catalog/online/desk"},
		{name: "offline desk beats online non-desk", first: []hostclient.Project{project("desk", "/catalog/preferred/desk")}, second: []hostclient.Project{project("other", "/catalog/online/other")}, firstOffline: true, wantHost: "host-1", wantCwd: "/catalog/preferred/desk"},
		{name: "online fallback beats offline saved host", first: []hostclient.Project{project("other", "/catalog/other")}, second: []hostclient.Project{project("other", "/catalog/another")}, firstOffline: true, wantHost: "host-2", wantCwd: "~"},
		{name: "workspace catalog fallback", workspace: true, wantHost: "host-1", wantCwd: "/catalog/legacy/desk"},
		{name: "no desk falls back visibly", first: []hostclient.Project{project("other", "/catalog/desk-copy"), {ID: "plain-dir", Name: "desk", Kind: "directory", Directory: "/catalog/not-a-repository"}}, wantHost: "host-1", wantCwd: "~"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, d := dbServer(t)
			rep := wfReport()
			rep.Paseo.Projects = tc.first
			if tc.workspace {
				rep.Paseo.Workspaces[0].Project = "desk"
				rep.Paseo.Workspaces[0].ProjectRoot = tc.wantCwd
				rep.Paseo.Workspaces[0].Directory = tc.wantCwd
			}
			sendReport(t, h, rep)
			if tc.second != nil {
				other := wfReport()
				other.Name = "host-2"
				other.Paseo.Projects = tc.second
				sendReport(t, h, other)
			}
			if tc.firstOffline {
				if _, err := d.Owner.Exec(t.Context(), "UPDATE hosts SET stream_id=NULL,last_seen=now()-interval '2 minutes' WHERE name='host-1'"); err != nil {
					t.Fatal(err)
				}
			}
			id := proposalForQuestion(t, h)
			question := askProposal(t, h, id, "Where can you read context?")
			var kind, host string
			var argsJSON []byte
			if err := d.Owner.QueryRow(t.Context(), `SELECT c.kind,c.host,c.args FROM workflow_proposal_questions q JOIN commands c ON c.id=q.command_id WHERE q.question_id=$1`, question).Scan(&kind, &host, &argsJSON); err != nil {
				t.Fatal(err)
			}
			var args hostclient.CreateAgentArgs
			if err := json.Unmarshal(argsJSON, &args); err != nil {
				t.Fatal(err)
			}
			if host != tc.wantHost || args.Cwd != tc.wantCwd || kind != hostclient.CreateAgentKind {
				t.Fatalf("location: %s %s %s; want %s %s create_agent", host, args.Cwd, kind, tc.wantHost, tc.wantCwd)
			}
			fallback := tc.wantCwd == "~"
			wantLocation := "desk"
			if fallback {
				wantLocation = "home_fallback"
			}
			code, res := sqlPost(t, h, q(`SELECT cwd,answer_location FROM workflow_proposal_question_status`))
			if code != 200 || len(res.Rows) != 1 || res.Rows[0][0] != tc.wantCwd || res.Rows[0][1] != wantLocation {
				t.Fatalf("question status location not SQL readable: %+v", res)
			}
			code, res = sqlPost(t, h, q(`SELECT args->>'cwd',args->'labels'->>'intelligence.answer_location' FROM commands WHERE id IN (SELECT command_id FROM workflow_proposal_questions)`))
			if code != 200 || len(res.Rows) != 1 || res.Rows[0][0] != tc.wantCwd || res.Rows[0][1] != wantLocation {
				t.Fatalf("location not SQL readable: %+v", res)
			}
			page := get(t, h, "/workflow-proposals/"+id, nil).Body.String()
			if fallback {
				if !strings.Contains(page, "No desk repository was found") || !strings.Contains(args.Prompt, "desk repository is unavailable") {
					t.Fatal("home fallback was not explained")
				}
			} else {
				if !strings.Contains(page, "Answer location: desk repository") || !strings.Contains(args.Prompt, "You are in the desk repository checkout") || !strings.Contains(args.Prompt, "read it for context") {
					t.Fatal("desk context was not explained")
				}
			}
			if !strings.Contains(args.Prompt, "Do not write files in the repository") {
				t.Fatal("missing repository read-only instruction")
			}
		})
	}
}

func TestProposalAnswerDoesNotUseAnotherAccountsDesk(t *testing.T) {
	h, d := dbServer(t)
	sendReport(t, h, wfReport())
	ts := h.(*testServer)
	other, err := ts.store.Create(t.Context(), "desk-other", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	joinHost(t, ts.store, other, "other-host")
	rep := wfReport()
	rep.Name = "other-host"
	rep.Paseo.Projects = []hostclient.Project{{ID: "desk", Name: "desk", Kind: "git", Directory: "/catalog/foreign/desk"}}
	if err := fleet.New(d.Owner).Record(t.Context(), rep, time.Now()); err != nil {
		t.Fatal(err)
	}
	id := proposalForQuestion(t, h)
	question := askProposal(t, h, id, "Use my own context")
	var host, cwd string
	if err := d.Owner.QueryRow(t.Context(), `SELECT c.host,c.args->>'cwd' FROM commands c JOIN workflow_proposal_questions q ON q.command_id=c.id WHERE q.question_id=$1`, question).Scan(&host, &cwd); err != nil {
		t.Fatal(err)
	}
	if host != "host-1" || cwd != "~" {
		t.Fatalf("foreign catalog affected selection: %s %s", host, cwd)
	}
}
