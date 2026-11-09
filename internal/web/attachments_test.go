package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestImageAttachments(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	var img bytes.Buffer
	png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	body, _ := json.Marshal(map[string]string{"name": "screen.png", "data": base64.StdEncoding.EncodeToString(img.Bytes())})
	rec := call(t, h, http.MethodPost, "/api/attachments", string(body), sameOriginFetch)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Reference string `json:"reference"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.Contains(out.Reference, "attachment:") {
		t.Fatal(out)
	}
	code, rows := sqlPost(t, h, q("SELECT name, octet_length(data) FROM attachments"))
	if code != 200 || len(rows.Rows) != 1 {
		t.Fatalf("SQL: %d %+v", code, rows)
	}
	b := openStream(t, h, "host-1")
	wf := createWorkflow(t, h, workflow.Starting)
	run := startRun(t, h, wf, "Use screenshot "+out.Reference)
	cmd := b.command()
	var agent hostclient.CreateAgentArgs
	json.Unmarshal(cmd.Args, &agent)
	if len(agent.Attachments) != 1 || !bytes.Equal(agent.Attachments[0].Data, img.Bytes()) || strings.Contains(agent.Prompt, "attachment:") {
		t.Fatalf("no materialized image: %s", cmd.Args)
	}
	succeed(t, b, cmd, `{"agentId":"finder-1"}`)
	if code, msg := move(t, h, keyOf(t, h, "host-1"), run, map[string]string{"move": "project", "directory": "/w/app"}); code != 200 {
		t.Fatalf("project: %d %s", code, msg)
	}
	cmds := commandsByKind(t, b, 2)
	var ws hostclient.CreateWorkspaceArgs
	json.Unmarshal(cmds[hostclient.CreateWorkspaceKind].Args, &ws)
	if ws.Agent == nil || len(ws.Agent.Attachments) != 1 || !bytes.Equal(ws.Agent.Attachments[0].Data, img.Bytes()) {
		t.Fatalf("orchestrator images: %+v", ws.Agent)
	}
	// The same references work in direct agent-session commands.
	launch, _ := json.Marshal(map[string]any{"kind": "create_agent", "args": map[string]string{"workspaceId": "w-app", "provider": "claude", "prompt": "Look at " + out.Reference}})
	rec = call(t, h, http.MethodPost, "/api/hosts/host-1/commands", string(launch), sameOriginFetch)
	if rec.Code != 202 {
		t.Fatalf("spawn: %d %s", rec.Code, rec.Body)
	}
	b.command()
	for _, behavior := range []string{"steer", "interrupt", "queue"} {
		message, _ := json.Marshal(map[string]any{"kind": "send_message", "args": map[string]string{"agentId": agentA1, "text": "Inspect " + out.Reference, "behavior": behavior}})
		rec = call(t, h, http.MethodPost, "/api/hosts/host-1/commands", string(message), sameOriginFetch)
		if rec.Code != 202 {
			t.Fatalf("message %s: %d %s", behavior, rec.Code, rec.Body)
		}
		command := b.command()
		var delivered struct {
			Text        string `json:"text"`
			Attachments []struct {
				Data []byte `json:"data"`
			} `json:"attachments"`
		}
		json.Unmarshal(command.Args, &delivered)
		if len(delivered.Attachments) != 1 || !bytes.Equal(delivered.Attachments[0].Data, img.Bytes()) || strings.Contains(delivered.Text, "attachment:") || !strings.Contains(delivered.Text, ".factory-scratch/attachments/") {
			t.Fatalf("message delivery: %s", command.Args)
		}
	}
	for _, data := range []string{`{"name":"x.svg","data":"PHN2Zz4="}`, `{"name":"x.png","data":"bad"}`} {
		rec = call(t, h, http.MethodPost, "/api/attachments", data, sameOriginFetch)
		if rec.Code != 400 {
			t.Fatalf("invalid upload: %d", rec.Code)
		}
	}
	rec = call(t, anon(h), http.MethodPost, "/api/attachments", string(body), sameOriginFetch)
	if rec.Code != 401 {
		t.Fatalf("anonymous upload: %d", rec.Code)
	}
	oversized, _ := json.Marshal(map[string]any{"name": "large.png", "data": make([]byte, (4<<20)+1)})
	rec = call(t, h, http.MethodPost, "/api/attachments", string(oversized), sameOriginFetch)
	if rec.Code != 400 {
		t.Fatalf("oversized upload: %d", rec.Code)
	}
	ts := h.(*testServer)
	bob, err := ts.store.Create(t.Context(), "bob", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	asBob := sessionFor(t, ts.store, bob)
	rec = call(t, h, http.MethodPost, "/api/sql", q("SELECT * FROM attachments"), sameOriginFetch, asBob)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"rowCount":0`) {
		t.Fatalf("isolation: %d %s", rec.Code, rec.Body)
	}
	// Another account's host cannot use a guessed image reference.
	joinHost(t, ts.store, bob, "bobs-host")
	rec = call(t, h, http.MethodPost, "/api/hosts/bobs-host/commands", string(launch), sameOriginFetch, asBob)
	if rec.Code != 400 {
		t.Fatalf("cross-account image launch: %d %s", rec.Code, rec.Body)
	}
	message, _ := json.Marshal(map[string]any{"kind": "send_message", "args": map[string]string{"agentId": agentA1, "text": out.Reference, "behavior": "queue"}})
	rec = call(t, h, http.MethodPost, "/api/hosts/bobs-host/commands", string(message), sameOriginFetch, asBob)
	if rec.Code != 400 {
		t.Fatalf("cross-account message image: %d %s", rec.Code, rec.Body)
	}
}

func TestAttachmentControlsOnTaskAndSpawnForms(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	wf := createWorkflow(t, h, workflow.Starting)
	for _, path := range []string{"/workflows", "/workflows/" + wf, newAgentPath, "/hosts/host-1/workspaces/new", "/hosts/host-1/agents/" + agentA1} {
		page := get(t, h, path, nil)
		if page.Code != 200 || !strings.Contains(page.Body.String(), `src="/attachments.js"`) || !strings.Contains(page.Body.String(), `imageAttachments(`) {
			t.Errorf("missing attachments on %s", path)
		}
	}
}

func TestAttachmentsScriptIsServed(t *testing.T) {
	rec := get(t, dictationServer(t, "", ""), "/attachments.js", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") ||
		!strings.Contains(rec.Body.String(), "function imageAttachments") || rec.Header().Get("ETag") == "" {
		t.Errorf("/attachments.js: %d %q, etag %q", rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("ETag"))
	}
}

func TestTooManyImageAttachmentsRefusesRun(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	var img bytes.Buffer
	png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	upload, _ := json.Marshal(map[string]any{"name": "image.png", "data": img.Bytes()})
	input := "Check these images:"
	for range 9 {
		rec := call(t, h, http.MethodPost, "/api/attachments", string(upload), sameOriginFetch)
		if rec.Code != 201 {
			t.Fatalf("upload: %d %s", rec.Code, rec.Body)
		}
		var out struct {
			Reference string `json:"reference"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		input += "\n" + out.Reference
	}
	wf := createWorkflow(t, h, workflow.Starting)
	body, _ := json.Marshal(map[string]string{"workflowId": wf, "input": input})
	rec := call(t, h, http.MethodPost, "/api/workflow-runs", string(body), sameOriginFetch)
	if rec.Code != 400 {
		t.Fatalf("image count limit: %d %s", rec.Code, rec.Body)
	}
	code, rows := sqlPost(t, h, q("SELECT id FROM workflow_runs"))
	if code != 200 || len(rows.Rows) != 0 {
		t.Fatal("refused run left persisted state")
	}
	message, _ := json.Marshal(map[string]any{"kind": "send_message", "args": map[string]string{"agentId": agentA1, "text": input, "behavior": "queue"}})
	rec = call(t, h, http.MethodPost, "/api/hosts/host-1/commands", string(message), sameOriginFetch)
	if rec.Code != 400 {
		t.Fatalf("message image limit: %d %s", rec.Code, rec.Body)
	}
}
