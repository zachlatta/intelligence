package webpush_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/accounts"
	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/webpush"
)

func TestStoreUpsertSendAndExpire(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	ctx := context.Background()
	roles := db.NewAccountRoles(d.Owner, d.Readonly)
	t.Cleanup(roles.Close)
	accts := accounts.New(d.Owner, roles)
	a1, err := accts.Create(ctx, "alice", "password-twelve-chars")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := accts.Create(ctx, "bob", "password-twelve-chars")
	if err != nil {
		t.Fatal(err)
	}

	pubB64, privB64, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	vapid, err := webpush.ParseVAPID(pubB64, privB64, "mailto:alerts@example.com")
	if err != nil {
		t.Fatal(err)
	}

	var code atomic.Int32
	code.Store(http.StatusCreated)
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(int(code.Load()))
	}))
	defer srv.Close()

	now := time.Date(2026, 11, 23, 6, 30, 0, 0, time.UTC)
	store := &webpush.Store{
		DB:     d.Owner,
		Client: &webpush.Client{VAPID: vapid, HTTP: srv.Client(), Now: func() time.Time { return now }},
		Now:    func() time.Time { return now },
	}

	uaPriv, err := ecdh.P256().GenerateKey(bytes.NewReader(bytes.Repeat([]byte{5}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	sub := webpush.Subscription{
		Endpoint: srv.URL + "/sub/1",
		P256dh:   base64.RawURLEncoding.EncodeToString(uaPriv.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{4}, 16)),
	}

	id1, err := store.Upsert(ctx, a1.ID, sub, "Android Chrome")
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// Upserting the same endpoint updates in place and keeps the same row ID.
	id2, err := store.Upsert(ctx, a1.ID, sub, "Android Chrome 2")
	if err != nil || id2 != id1 {
		t.Fatalf("Upsert duplicate: id=%q (want %q) err=%v", id2, id1, err)
	}

	// RLS check: alice sees her row, bob sees none.
	r1, err := accts.Reader(ctx, a1)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := accts.Reader(ctx, a2)
	if err != nil {
		t.Fatal(err)
	}
	var c1, c2 int
	if err := r1.QueryRow(ctx, "SELECT count(*) FROM push_subscriptions").Scan(&c1); err != nil {
		t.Fatal(err)
	}
	if err := r2.QueryRow(ctx, "SELECT count(*) FROM push_subscriptions").Scan(&c2); err != nil {
		t.Fatal(err)
	}
	if c1 != 1 || c2 != 0 {
		t.Fatalf("RLS counts: alice=%d bob=%d, want 1, 0", c1, c2)
	}

	// Send succeeds and records last_sent_at.
	sent, expired, err := store.SendAccount(ctx, a1.ID, webpush.Message{Title: "Hi", Body: "Test", URL: "/"})
	if err != nil || sent != 1 || expired != 0 || hits.Load() != 1 {
		t.Fatalf("SendAccount = (%d, %d, %v), hits=%d", sent, expired, err, hits.Load())
	}
	var lastSent *time.Time
	if err := r1.QueryRow(ctx, "SELECT last_sent_at FROM push_subscriptions WHERE id = $1::uuid", id1).Scan(&lastSent); err != nil || lastSent == nil {
		t.Fatalf("last_sent_at = %v, err=%v", lastSent, err)
	}

	// 410 Gone removes the expired subscription automatically.
	code.Store(http.StatusGone)
	sent, expired, err = store.SendAccount(ctx, a1.ID, webpush.Message{Title: "Hi 2"})
	if err != nil || sent != 0 || expired != 1 {
		t.Fatalf("SendAccount expired = (%d, %d, %v)", sent, expired, err)
	}
	if err := r1.QueryRow(ctx, "SELECT count(*) FROM push_subscriptions").Scan(&c1); err != nil {
		t.Fatal(err)
	}
	if c1 != 0 {
		t.Fatalf("expired subscription was not deleted: count=%d", c1)
	}
}
