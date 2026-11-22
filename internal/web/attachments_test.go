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
	if len(agent.Attachments) != 1 || !bytes.Equal(agent.Attachments[0].Data, img.Bytes()) || strings.Contains(agent.Prompt, "attachment:") || agent.Mode != "bypassPermissions" {
		t.Fatalf("no materialized image: %s", cmd.Args)
	}
	succeed(t, b, cmd, `{"agentId":"finder-1"}`)
	if code, msg := move(t, h, keyOf(t, h, "host-1"), run, map[string]string{"move": "project", "directory": "/w/app"}); code != 200 {
		t.Fatalf("project: %d %s", code, msg)
	}
	cmds := commandsByKind(t, b, 2)
	var ws hostclient.CreateWorkspaceArgs
	json.Unmarshal(cmds[hostclient.CreateWorkspaceKind].Args, &ws)
	if ws.Agent == nil || len(ws.Agent.Attachments) != 1 || !bytes.Equal(ws.Agent.Attachments[0].Data, img.Bytes()) || ws.Agent.Mode != "bypassPermissions" {
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
	// Arbitrary non-image files (e.g. SVG, PDF, plain text) are accepted too.
	rec = call(t, h, http.MethodPost, "/api/attachments", `{"name":"x.svg","data":"PHN2Zz4="}`, sameOriginFetch)
	if rec.Code != http.StatusCreated {
		t.Fatalf("svg upload: %d %s", rec.Code, rec.Body)
	}
	corruptPNG := base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0})
	for _, data := range []string{`{"name":"x.png","data":"bad"}`, `{"name":"empty.txt","data":""}`, `{"name":"corrupt.png","data":"` + corruptPNG + `"}`} {
		rec = call(t, h, http.MethodPost, "/api/attachments", data, sameOriginFetch)
		if rec.Code != 400 {
			t.Fatalf("invalid upload (%s): %d", data, rec.Code)
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

func TestWorkflowRunReplyAndInputAttachments(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	b := openStream(t, h, "host-1")
	wf := createWorkflow(t, h, workflow.Starting)
	run := startRun(t, h, wf, "Check the Slack screenshot")
	cmd := b.command()
	succeed(t, b, cmd, `{"agentId":"finder-1"}`)
	key := keyOf(t, h, "host-1")
	if code, msg := move(t, h, key, run, map[string]string{"move": "project", "directory": "/w/app"}); code != 200 {
		t.Fatalf("project: %d %s", code, msg)
	}
	cmds := commandsByKind(t, b, 2)
	succeed(t, b, cmds[hostclient.CreateWorkspaceKind], `{"workspaceId":"ws-1","agentId":"orch-1"}`)
	if code, msg := move(t, h, key, run, map[string]string{"move": "ask", "note": "Paste a screenshot or attach the file."}); code != 200 {
		t.Fatalf("ask: %d %s", code, msg)
	}

	// Waiting run page renders the reply attachment container and binds imageAttachments.
	page := get(t, h, "/workflow-runs/"+run, nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), `src="/attachments.js"`) ||
		!strings.Contains(page.Body.String(), `id="reply-attachments"`) ||
		!strings.Contains(page.Body.String(), `imageAttachments(`) {
		t.Fatalf("waiting run page missing attachment UI: %d", page.Code)
	}

	// Upload an image and an arbitrary PDF file.
	var imgBuf bytes.Buffer
	png.Encode(&imgBuf, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	upload := func(name string, raw []byte) string {
		body, _ := json.Marshal(map[string]any{"name": name, "data": raw})
		rec := call(t, h, http.MethodPost, "/api/attachments", string(body), sameOriginFetch)
		if rec.Code != http.StatusCreated {
			t.Fatalf("upload %s: %d %s", name, rec.Code, rec.Body)
		}
		var out struct {
			Reference string `json:"reference"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Reference
	}
	imgRef := upload("slack.png", imgBuf.Bytes())
	pdfBytes := []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\n")
	pdfRef := upload("notes.pdf", pdfBytes)

	// Send reply with both attachments (no extra prose required, or with prose).
	replyBody, _ := json.Marshal(map[string]string{"text": "Here you go:\n\nAttached files:\n" + imgRef + "\n" + pdfRef})
	rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/reply", string(replyBody), sameOriginFetch)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reply with attachments: %d %s", rec.Code, rec.Body)
	}
	replyCmd := b.command()
	var delivered hostclient.AgentArgs
	if err := json.Unmarshal(replyCmd.Args, &delivered); err != nil {
		t.Fatal(err)
	}
	if len(delivered.Attachments) != 2 {
		t.Fatalf("expected 2 delivered attachments, got %d: %s", len(delivered.Attachments), replyCmd.Args)
	}
	if !bytes.Equal(delivered.Attachments[0].Data, imgBuf.Bytes()) || delivered.Attachments[0].MediaType != "image/png" {
		t.Fatalf("first attachment mismatch: %+v", delivered.Attachments[0].MediaType)
	}
	if !bytes.Equal(delivered.Attachments[1].Data, pdfBytes) || delivered.Attachments[1].MediaType != "application/pdf" || delivered.Attachments[1].Name != "notes.pdf" {
		t.Fatalf("second attachment mismatch: %q %q", delivered.Attachments[1].MediaType, delivered.Attachments[1].Name)
	}
	if strings.Contains(delivered.Text, "[attachment:") ||
		!strings.Contains(delivered.Text, delivered.Attachments[0].Path()) ||
		!strings.Contains(delivered.Text, delivered.Attachments[1].Path()) ||
		!strings.Contains(delivered.Text, "notes.pdf") {
		t.Fatalf("unexpected delivered reply text: %s", delivered.Text)
	}

	// Additional input on the now-running run also delivers file attachments.
	txtBytes := []byte("follow-up log line\n")
	txtRef := upload("error.log", txtBytes)
	inputBody, _ := json.Marshal(map[string]string{"text": "Attached files:\n" + txtRef})
	rec = call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/input", string(inputBody), sameOriginFetch)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("input with attachment: %d %s", rec.Code, rec.Body)
	}
	inputCmd := b.command()
	var deliveredInput hostclient.AgentArgs
	if err := json.Unmarshal(inputCmd.Args, &deliveredInput); err != nil {
		t.Fatal(err)
	}
	if len(deliveredInput.Attachments) != 1 || !bytes.Equal(deliveredInput.Attachments[0].Data, txtBytes) ||
		!strings.HasSuffix(deliveredInput.Attachments[0].Path(), ".log") ||
		!strings.Contains(deliveredInput.Text, deliveredInput.Attachments[0].Path()) {
		t.Fatalf("unexpected delivered input: %s", inputCmd.Args)
	}

	// Cross-account attachment reference is rejected on /input too.
	ts := h.(*testServer)
	bob, err := ts.store.Create(t.Context(), "bob-reply", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	asBob := sessionFor(t, ts.store, bob)
	bobUpload, _ := json.Marshal(map[string]any{"name": "secret.txt", "data": []byte("bob secret")})
	rec = call(t, h, http.MethodPost, "/api/attachments", string(bobUpload), sameOriginFetch, asBob)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bob upload: %d %s", rec.Code, rec.Body)
	}
	var bobOut struct {
		Reference string `json:"reference"`
	}
	json.Unmarshal(rec.Body.Bytes(), &bobOut)
	badInput, _ := json.Marshal(map[string]string{"text": "Steal " + bobOut.Reference})
	rec = call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/input", string(badInput), sameOriginFetch)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-account attachment in input: %d %s", rec.Code, rec.Body)
	}
}
