package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/email"
	"github.com/zachlatta/intelligence/internal/email/gmailfake"
	"github.com/zachlatta/intelligence/internal/hostclient"
	"github.com/zachlatta/intelligence/internal/workflow"
)

const verifiedAR = "mx.google.com; dkim=pass header.i=@example.com; spf=pass smtp.mailfrom=%s; dmarc=pass (p=NONE) header.from=example.com"

// mailMsg is an email in the fake mailbox: from (at example.com, verified unless forged), in thread.
func mailMsg(id, thread, from, subject, text string, extra ...email.Header) email.GmailMessage {
	headers := []email.Header{{Name: "Authentication-Results", Value: strings.Replace(verifiedAR, "%s", from, 1)}}
	headers = append(headers, extra...)
	headers = append(headers, email.Header{Name: "From", Value: from}, email.Header{Name: "To", Value: "bot@example.com"},
		email.Header{Name: "Subject", Value: subject})
	return email.GmailMessage{ID: id, ThreadID: thread, LabelIDs: []string{"INBOX"}, Payload: email.Part{MimeType: "text/plain",
		Headers: headers, Body: email.Body{Data: base64.URLEncoding.EncodeToString([]byte(text))}}}
}

// forged is m without Gmail's verdict that its sender sent it.
func forged(m email.GmailMessage) email.GmailMessage {
	m.Payload.Headers[0].Value = strings.Replace(m.Payload.Headers[0].Value, "dmarc=pass", "dmarc=fail", 1)
	return m
}

func emailKey(t *testing.T) *email.Keyring {
	t.Helper()
	k, err := email.NewKeyring(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// emailServer is a server whose account can connect the fake mailbox, which it reads every few
// milliseconds.
func emailServer(t *testing.T) (*testServer, *gmailfake.Server) {
	t.Helper()
	fake := gmailfake.New(t, "bot@example.com")
	d := dbtest.New(t)
	h := newServer(t, d, d.Owner, "v", func(c *Config) {
		c.Google, c.EmailKey, c.EmailInterval = fake.Google(), emailKey(t), 20*time.Millisecond
	})
	return h, fake
}

// connectGmail signs the account in to the fake mailbox, as the browser does.
func connectGmail(t *testing.T, h http.Handler, fake *gmailfake.Server) {
	t.Helper()
	rec := call(t, h, http.MethodPost, "/api/gmail/sign-in", `{}`, sameOriginFetch)
	var start struct {
		URL   string `json:"url"`
		Paste bool   `json:"paste"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &start) != nil || start.Paste {
		t.Fatalf("sign-in = %d %s", rec.Code, rec.Body)
	}
	back := fake.Consent(start.URL)
	if !strings.HasPrefix(back, "http://example.com/gmail/callback?") {
		t.Fatalf("Google sends the browser to %s", back)
	}
	if page := get(t, anon(h), strings.TrimPrefix(back, "http://example.com"), nil); page.Code != http.StatusOK ||
		!strings.Contains(page.Body.String(), "/api/gmail/connect") {
		t.Fatalf("callback page = %d %s", page.Code, page.Body)
	}
	body, _ := json.Marshal(map[string]string{"url": back})
	rec = call(t, h, http.MethodPost, "/api/gmail/connect", string(body), sameOriginFetch)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "bot@example.com") {
		t.Fatalf("connect = %d %s", rec.Code, rec.Body)
	}
}

// eventually waits up to 5 seconds for f.
func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// emailDecision is what the emails table says message id did.
func emailDecision(t *testing.T, h http.Handler, id string) map[string]any {
	t.Helper()
	_, r := sqlPost(t, h, q(`SELECT decision, reason, coalesce(run_id::text, '') AS run_id, verified FROM emails WHERE gmail_id = '`+id+`'`))
	if len(r.Rows) == 0 {
		return nil
	}
	out := map[string]any{}
	for i, c := range r.Columns {
		out[c.Name] = r.Rows[0][i]
	}
	return out
}

// decided waits for message id's decision.
func decided(t *testing.T, h http.Handler, id string) map[string]any {
	t.Helper()
	var row map[string]any
	eventually(t, "email "+id, func() bool { row = emailDecision(t, h, id); return row != nil })
	return row
}

// christina is a workflow on host-1 that email from christina@example.com starts, in /w/app.
func christina(t *testing.T, h http.Handler) string {
	t.Helper()
	d := workflow.Starting
	d.Trigger = workflow.TriggerEmail
	d.Email = &workflow.EmailFilter{From: []string{"Christina@example.com"}}
	d.Project = workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"}
	return createWorkflow(t, h, d)
}

func TestEmailStartsTheWorkflowThatMatchesItAndRepliesGoToItsRun(t *testing.T) {
	h, fake := emailServer(t)
	sendReport(t, h, wfReport())
	key := keyOf(t, h, "host-1")
	b := openStream(t, h, "host-1")
	wf := christina(t, h)

	fake.Add(mailMsg("old", "t-old", "christina@example.com", "From last week", "Already handled."))
	connectGmail(t, h, fake)
	if rec := call(t, h, http.MethodPut, "/api/gmail/owners", `{"owners":["Zach@example.com"]}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("owners = %d %s", rec.Code, rec.Body)
	}
	// what came before the mailbox was connected is read in, for the editor's preview, and starts nothing
	if r := decided(t, h, "old"); r["decision"] != "before" || r["run_id"] != "" {
		t.Fatalf("old email = %v", r)
	}

	// a new email from Christina starts her workflow; its input says it is not from Zach
	fake.Add(mailMsg("m1", "t1", "Christina <christina@example.com>", "Q4 board deck", "Numbers for slide 6, please.", email.Header{Name: "Date", Value: "Wed, 11 Nov 2026 14:39:00 -0500"}))
	r := decided(t, h, "m1")
	run, _ := r["run_id"].(string)
	if r["decision"] != "started" || run == "" {
		t.Fatalf("m1 = %v", r)
	}
	code, requested := sqlPost(t, h, q("SELECT requested_at = TIMESTAMPTZ '2026-11-11T19:39:00Z' FROM workflow_runs WHERE id='"+run+"'"))
	if code != 200 || len(requested.Rows) != 1 || requested.Rows[0][0] != true {
		t.Fatalf("requested_at: %+v", requested)
	}
	receipt := call(t, anon(h), "POST", "/api/workflow-runs/"+run+"/answers", `{"channel":"email","messageId":"reply-example","threadId":"t1","sentAt":"2026-11-11T20:00:00Z","checked":true}`, map[string]string{"Authorization": "Bearer " + key})
	if receipt.Code != 200 {
		t.Fatal(receipt.Body.String())
	}
	code, answerReceipt := sqlPost(t, h, q("SELECT message_id,thread_id,sent_at=TIMESTAMPTZ '2026-11-11T20:00:00Z' FROM workflow_run_answers WHERE run_id='"+run+"'"))
	if code != 200 || len(answerReceipt.Rows) != 1 || answerReceipt.Rows[0][0] != "reply-example" || answerReceipt.Rows[0][1] != "t1" || answerReceipt.Rows[0][2] != true {
		t.Fatalf("reply receipt: %+v", answerReceipt)
	}
	ws := b.command()
	var wa hostclient.CreateWorkspaceArgs
	json.Unmarshal(ws.Args, &wa)
	if ws.Kind != hostclient.CreateWorkspaceKind || wa.Agent == nil || !strings.Contains(wa.Agent.Prompt, "Numbers for slide 6") ||
		!strings.Contains(wa.Agent.Prompt, "not from Zach") ||
		wa.Title != "Task from Intelligence: Q4 board deck · "+strings.ReplaceAll(run, "-", "")[:12] ||
		wa.Branch != "wf-task-from-intelligence-q4-board-deck-"+strings.ReplaceAll(run, "-", "")[:12] {
		t.Fatalf("command = %s %s", ws.Kind, ws.Args)
	}
	succeed(t, b, ws, `{"workspaceId":"w-run","agentId":"orch-1"}`)
	// Email links use the same task label as the runs list, with the legacy title as fallback.
	if page := get(t, h, "/workflows/email", nil).Body.String(); !strings.Contains(page, "Q4 board deck</a>") {
		t.Fatal("email page missing fallback title")
	}
	if code, msg := move(t, h, key, run, map[string]string{"move": "summarize", "summary": "Prepare quarterly board presentation"}); code != http.StatusOK {
		t.Fatalf("summary: %d %s", code, msg)
	}
	if page := get(t, h, "/workflows/email", nil).Body.String(); !strings.Contains(page, "Prepare quarterly board presentation</a>") {
		t.Fatal("email page missing task summary")
	}

	// her reply in the thread goes to the run's orchestrator, without its quoted history
	fake.Add(mailMsg("m2", "t1", "christina@example.com", "Re: Q4 board deck",
		"Use the November 1 numbers.\n\nOn Sat, Nov 8, 2026 at 2:30 PM Bot <bot@example.com> wrote:\n> Draft attached"))
	if r := decided(t, h, "m2"); r["decision"] != "added" || r["run_id"] != run {
		t.Fatalf("m2 = %v", r)
	}
	msg := b.command()
	var ma hostclient.AgentArgs
	json.Unmarshal(msg.Args, &ma)
	if msg.Kind != hostclient.SendMessageKind || ma.AgentID != "orch-1" || !strings.Contains(ma.Text, "November 1") || strings.Contains(ma.Text, "Draft attached") {
		t.Fatalf("message = %s %s", msg.Kind, msg.Args)
	}

	// the run asks Zach; his emailed reply answers it
	if code, m := move(t, h, key, run, map[string]string{"move": "ask", "note": "Send the draft?"}); code != http.StatusOK {
		t.Fatalf("ask = %d %s", code, m)
	}
	fake.Add(mailMsg("m3", "t1", "zach@example.com", "Re: Q4 board deck", "Yes, send it."))
	if r := decided(t, h, "m3"); r["decision"] != "answered" {
		t.Fatalf("m3 = %v", r)
	}
	answer := b.command()
	json.Unmarshal(answer.Args, &ma)
	if !strings.Contains(ma.Text, "Zach replied to your note") || !strings.Contains(ma.Text, "Yes, send it.") {
		t.Fatalf("answer = %s", answer.Args)
	}
	if st := readRun(t, h, run)["status"]; st != "running" {
		t.Fatalf("run = %v", st)
	}

	// once the run is over, a reply in its thread starts a new run of the workflow
	if code, m := move(t, h, key, run, map[string]string{"move": "done", "summary": "sent"}); code != http.StatusOK {
		t.Fatalf("done = %d %s", code, m)
	}
	// the finished run's workspace is archived once its orchestrator's turn ends
	var aa hostclient.WorkspaceArgs
	archive := b.command()
	json.Unmarshal(archive.Args, &aa)
	if archive.Kind != hostclient.ArchiveWorkspaceKind || aa.WorkspaceID != "w-run" || aa.WaitForAgent != "orch-1" {
		t.Fatalf("archive = %s %s", archive.Kind, archive.Args)
	}
	fake.Add(mailMsg("m4", "t1", "christina@example.com", "Re: Q4 board deck", "One more thing: slide 7 too."))
	r = decided(t, h, "m4")
	if r["decision"] != "follow_up" || r["run_id"] == run || r["run_id"] == "" {
		t.Fatalf("m4 = %v", r)
	}
	if next := b.command(); next.Kind != hostclient.CreateWorkspaceKind || !strings.Contains(string(next.Args), run) {
		t.Fatalf("follow-up = %s %s", next.Kind, next.Args)
	}

	// a forged sender, a newsletter and a stranger start nothing, and say why
	fake.Add(forged(mailMsg("f1", "t5", "christina@example.com", "Wire $40,000 today", "Before 5pm.")))
	fake.Add(mailMsg("n1", "t6", "christina@example.com", "Our newsletter", "News.", email.Header{Name: "List-Unsubscribe", Value: "<mailto:u@example.com>"}))
	fake.Add(mailMsg("s1", "t7", "someone@example.com", "Hello", "Hi."))
	for id, why := range map[string]string{"f1": "forged sender", "n1": "newsletter", "s1": "no workflow"} {
		if r := decided(t, h, id); r["decision"] != "ignored" || !strings.Contains(r["reason"].(string), why) {
			t.Errorf("%s = %v", id, r)
		}
	}
	_ = wf
}

func TestTheMailboxTokenIsNotReadableThroughSQL(t *testing.T) {
	h, fake := emailServer(t)
	connectGmail(t, h, fake)
	_, r := sqlPost(t, h, q("SELECT address, encode(token, 'escape') FROM gmail_connections"))
	if len(r.Rows) != 1 || r.Rows[0][0] != "bot@example.com" {
		t.Fatalf("connection = %+v", r)
	}
	if s, _ := json.Marshal(r.Rows); strings.Contains(string(s), "refresh-for") {
		t.Error("the refresh token is readable")
	}
}

func TestTwoWorkflowsCannotClaimTheSameSender(t *testing.T) {
	h, _ := dbServer(t)
	sendReport(t, h, wfReport())
	christina(t, h)
	d := workflow.Starting
	d.Trigger = workflow.TriggerEmail
	d.Email = &workflow.EmailFilter{From: []string{"sarah@example.com", "christina@example.com"}}
	rec := call(t, h, http.MethodPost, "/api/workflows", wfBody(t, "Events", d), sameOriginFetch)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "christina@example.com already starts") {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	d.Email.Subject = "venue" // a field tells them apart
	if rec := call(t, h, http.MethodPost, "/api/workflows", wfBody(t, "Events", d), sameOriginFetch); rec.Code != http.StatusCreated {
		t.Fatalf("POST with a subject = %d %s", rec.Code, rec.Body)
	}
}

func TestASignInStartedByAnotherAccountIsRefused(t *testing.T) {
	h, fake := emailServer(t)
	other := emailKey(t).State(h.acct.ID+1, time.Now())
	back := fake.Consent(fake.Google().SignInURL(other, "c", "http://example.com/gmail/callback"))
	body, _ := json.Marshal(map[string]string{"url": back})
	if rec := call(t, h, http.MethodPost, "/api/gmail/connect", string(body), sameOriginFetch); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "another account") {
		t.Fatalf("connect = %d %s", rec.Code, rec.Body)
	}
}

func TestWithoutGoogleSettingsEmailIsOff(t *testing.T) {
	h, _ := dbServer(t)
	if rec := call(t, h, http.MethodPost, "/api/gmail/sign-in", `{}`, sameOriginFetch); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("sign-in = %d %s", rec.Code, rec.Body)
	}
	if page := get(t, h, "/workflows/email", nil); page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "GOOGLE_OAUTH_CLIENT_JSON") {
		t.Fatalf("page = %d %s", page.Code, page.Body)
	}
}

func TestTheEmailPageShowsTheMailboxAndWhatEachEmailDid(t *testing.T) {
	h, fake := emailServer(t)
	sendReport(t, h, wfReport())
	christina(t, h)
	connectGmail(t, h, fake)
	fake.Add(mailMsg("s1", "t7", "someone@example.com", "Hello there", "Hi."))
	decided(t, h, "s1")
	page := get(t, h, "/workflows/email", nil)
	for _, want := range []string{"bot@example.com", "Hello there", "someone@example.com", "no workflow"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// a mailbox error's instant shows in the reader's own zone (localtime.js)
	if _, err := h.d.Owner.Exec(t.Context(), `UPDATE gmail_connections SET error = 'the mailbox was not read for too long: email from before 2026-11-11T23:57:05Z was skipped'`); err != nil {
		t.Fatal(err)
	}
	page = get(t, h, "/workflows/email", nil)
	if want := `<p class="banner">the mailbox was not read for too long: email from before <time datetime="2026-11-11T23:57:05Z" data-format="short"`; !strings.Contains(page.Body.String(), want) {
		t.Errorf("mailbox error not shown locally; want %s in\n%s", want, page.Body)
	}
	editor := get(t, h, "/workflows", nil)
	if !strings.Contains(editor.Body.String(), "christina@example.com") {
		t.Error("the workflows page does not say what email starts the workflow")
	}
}

// TestADeletedEmailWorkflowStartsNothingAndFreesItsSender: once deleted, an email workflow starts
// no run, from a new email or a reply in an old run's thread, and its senders can start another.
func TestADeletedEmailWorkflowStartsNothingAndFreesItsSender(t *testing.T) {
	h, fake := emailServer(t)
	sendReport(t, h, wfReport())
	wf := christina(t, h)
	connectGmail(t, h, fake)
	fake.Add(mailMsg("m1", "t1", "christina@example.com", "Q4 board deck", "Numbers for slide 6, please."))
	run, _ := decided(t, h, "m1")["run_id"].(string)
	if run == "" {
		t.Fatal("m1 started no run")
	}
	if rec := call(t, h, http.MethodPost, "/api/workflow-runs/"+run+"/cancel", `{}`, sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, http.MethodDelete, "/api/workflows/"+wf, "", sameOriginFetch); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}

	fake.Add(mailMsg("m2", "t1", "christina@example.com", "Re: Q4 board deck", "And slide 7."))
	if r := decided(t, h, "m2"); r["decision"] != "ignored" || !strings.Contains(r["reason"].(string), "gone") {
		t.Errorf("a reply to a deleted workflow's run = %v", r)
	}
	fake.Add(mailMsg("m3", "t3", "christina@example.com", "New request", "Hi."))
	if r := decided(t, h, "m3"); r["decision"] != "ignored" || !strings.Contains(r["reason"].(string), "no workflow") {
		t.Errorf("a new email for a deleted workflow = %v", r)
	}
	christina(t, h) // her address is free again
}
