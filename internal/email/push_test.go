package email

import (
	"context"
	"encoding/base64"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/accounts"
	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/webpush"
	"github.com/zachlatta/intelligence/internal/workflow"
)

type spyPush struct {
	mu   sync.Mutex
	msgs []webpush.Message
}

func (s *spyPush) SendAccount(_ context.Context, _ int64, msg webpush.Message) (int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msg)
	return 1, 0, nil
}

func TestEmailDisabledProviderSendsOneWebPush(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	roles := db.NewAccountRoles(d.Owner, d.Readonly)
	defer roles.Close()
	accts := accounts.New(d.Owner, roles)
	acct, err := accts.Create(ctx, "alice", "password-twelve-chars")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(ctx, `INSERT INTO host_keys (host, account_id, key_hash, created_at) VALUES ('host-1', $1, 'h', now())`, acct.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(ctx, `INSERT INTO hosts (name, last_seen, connected) VALUES ('host-1', now(), true)`); err != nil {
		t.Fatal(err)
	}

	spy := &spyPush{}
	eng := workflow.NewEngine(d.Owner, time.Now)
	eng.Push = spy
	def := workflow.Definition{
		Host:    "host-1",
		Trigger: workflow.TriggerEmail,
		Email:   &workflow.EmailFilter{From: []string{"sender@example.com"}},
		Project: workflow.Project{Mode: workflow.ProjectFixed, Directory: "/w/app"},
		Orchestrator: workflow.Orchestrator{
			Provider: "muse", Model: "muse-1", Thinking: "medium", Instructions: "go",
		},
		Steps: []workflow.Step{{ID: "s1", Title: "Do", Kind: workflow.KindStep, Instructions: "do"}},
	}
	if _, err := eng.CreateWorkflow(ctx, acct.ID, "EmailMuse", "d", def); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(ctx, `UPDATE provider_plan_settings SET enabled=false WHERE provider='muse'`); err != nil {
		t.Fatal(err)
	}

	svc := &Service{DB: d.Owner, Engine: eng, Now: time.Now}
	g := GmailMessage{
		ID:       "gmail-1",
		ThreadID: "thread-1",
		Payload: Part{
			MimeType: "text/plain",
			Headers: []Header{
				{Name: "From", Value: "sender@example.com"},
				{Name: "Subject", Value: "Build this"},
				{Name: "Authentication-Results", Value: "mx.google.com; dmarc=pass (p=NONE) header.from=example.com"},
			},
			Body: Body{Data: base64.RawURLEncoding.EncodeToString([]byte("Please build it"))},
		},
	}
	if err := svc.keep(ctx, acct.ID, g, func(txCtx context.Context, tx pgx.Tx, m workflow.Mail) (decision, error) {
		return svc.route(txCtx, tx, acct.ID, nil, g, m)
	}); err != nil {
		t.Fatalf("keep: %v", err)
	}

	spy.mu.Lock()
	got := len(spy.msgs)
	spy.mu.Unlock()
	if got != 1 {
		var dec, reason string
		_ = d.Owner.QueryRow(ctx, `SELECT decision, reason FROM emails WHERE gmail_id='gmail-1'`).Scan(&dec, &reason)
		t.Fatalf("email disabled-provider push count = %d, want 1 (decision=%q reason=%q)", got, dec, reason)
	}
}
