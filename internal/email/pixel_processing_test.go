package email

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
	"github.com/zachlatta/intelligence/internal/workflow"
)

func TestProcessingOpensPixelsOnceAfterCommitWithoutWaiting(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	roles := db.NewAccountRoles(d.Owner, d.Readonly)
	defer roles.Close()
	role, err := roles.RoleName(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(ctx, `INSERT INTO accounts(id,username,password_hash,role_name) VALUES(1,'ann','x',$1)`, role); err != nil {
		t.Fatal(err)
	}
	if err := roles.Ensure(ctx, d.Owner, role); err != nil {
		t.Fatal(err)
	}
	ro, err := roles.Pool(ctx, role)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}, 10), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-release; w.WriteHeader(500) }))
	defer server.Close()
	defer close(release)
	s := Service{DB: d.Owner, pixelHTTP: pixelClient(func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}, func(netip.Addr) bool { return true })}
	localURL := routeTestPixelClient(t, s.pixelHTTP, server)
	g := GmailMessage{ID: "m1", Payload: Part{MimeType: "text/html", Body: Body{Data: base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`<img width=1 height=1 src="%s">`, localURL)))}}}
	decide := func(pgx.Tx, workflow.Mail) (decision, error) { return decision{Decision: "started"}, nil }
	done := make(chan error, 1)
	go func() { done <- s.keep(ctx, 1, g, decide) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("processing blocked on fetch")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no pixel fetch")
	}
	var n int
	if err := ro.QueryRow(ctx, `SELECT count(*) FROM email_pixel_opens`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("own SQL rows=%d: %v", n, err)
	}
	if err := d.Readonly.QueryRow(ctx, `SELECT count(*) FROM email_pixel_opens`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unowned SQL rows=%d: %v", n, err)
	}
	if _, err := ro.Exec(ctx, `DELETE FROM email_pixel_opens`); err == nil {
		t.Fatal("read role wrote")
	}
	if err := s.keep(ctx, 1, g, decide); err != nil {
		t.Fatal(err)
	}
	for i, kind := range []string{"before", "ignored", "added", "answered", "follow_up"} {
		g.ID = fmt.Sprint("m", i+2)
		if err := s.keep(ctx, 1, g, func(pgx.Tx, workflow.Mail) (decision, error) { return decision{Decision: kind}, nil }); err != nil {
			t.Fatal(err)
		}
	}
	g.ID = "rollback"
	if err := s.keep(ctx, 1, g, func(pgx.Tx, workflow.Mail) (decision, error) { return decision{}, errors.New("rollback") }); err == nil {
		t.Fatal("expected rollback")
	}
	for _, labels := range [][]string{{"SENT"}, {"DRAFT"}, {"SPAM"}, {"TRASH"}} {
		g.ID = labels[0]
		g.LabelIDs = labels
		if err := s.keep(ctx, 1, g, decide); err != nil {
			t.Fatal(err)
		}
	}
	if err := ro.QueryRow(ctx, `SELECT count(*) FROM email_pixel_opens`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("marker rows=%d: %v", n, err)
	}
	// Release the slow, failed fetches, then wait for persisted results before closing the DB.
	release <- struct{}{}
	release <- struct{}{}
	release <- struct{}{}
	release <- struct{}{}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := ro.QueryRow(ctx, `SELECT count(*) FROM email_pixel_opens WHERE finished_at IS NOT NULL AND attempted=1 AND opened=0`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("finished failed opens=%d", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("missing fetch")
	}
	// Three other successful processing decisions, and no duplicates/skipped-mail fetches.
	if len(entered) != 2 {
		t.Fatalf("extra or missing fetches: %d", len(entered))
	}
	if err := db.CheckContract(ctx, d.Owner, d.Readonly); err != nil {
		t.Fatal(err)
	}
}
