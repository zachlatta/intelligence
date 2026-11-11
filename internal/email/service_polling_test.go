package email

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

func TestRunKeepsPollingWhileAnotherMailboxIsSlow(t *testing.T) {
	d := dbtest.New(t)
	key := testKey(t)
	for _, host := range []string{"slow", "fast"} {
		_ = dbtest.Reader(t, d, host)
		sealed, err := key.Seal(host)
		if err != nil {
			t.Fatal(err)
		}
		_, err = d.Owner.Exec(t.Context(), `INSERT INTO gmail_connections(account_id,address,token,history_id,backfilled,connected_at)
 SELECT account_id,$1,$2,'10',true,now() FROM host_keys WHERE host=$1`, host, sealed)
		if err != nil {
			t.Fatal(err)
		}
	}
	var fast atomic.Int32
	slowStarted := make(chan struct{})
	var once sync.Once
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = r.ParseForm()
			_, _ = fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`, r.Form.Get("refresh_token"))
			return
		}
		if r.Header.Get("Authorization") == "Bearer slow" {
			once.Do(func() { close(slowStarted) })
			<-r.Context().Done()
			return
		}
		fast.Add(1)
		_, _ = w.Write([]byte(`{"historyId":"10"}`))
	}))
	defer fake.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	s := &Service{DB: d.Owner, Key: key, Google: &Google{APIURL: fake.URL, TokenURL: fake.URL + "/token"}}
	go func() { s.Run(ctx, 20*time.Millisecond); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case <-slowStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("slow mailbox never started")
	}
	deadline := time.Now().Add(3 * time.Second)
	for fast.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fast.Load() < 3 {
		t.Fatalf("slow mailbox blocked polling: %d fast reads", fast.Load())
	}
}

func TestPollingDeadlinesSurviveAnotherServerAndKeepHistoryOnFailure(t *testing.T) {
	d := dbtest.New(t)
	reader := dbtest.Reader(t, d, "test-host")
	ctx := t.Context()
	var account int64
	if err := d.Owner.QueryRow(ctx, "SELECT account_id FROM host_keys WHERE host='test-host'").Scan(&account); err != nil {
		t.Fatal(err)
	}
	key := testKey(t)
	sealed, err := key.Seal("refresh")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err = d.Owner.Exec(ctx, `INSERT INTO gmail_connections(account_id,address,token,history_id,backfilled,connected_at) VALUES($1,'bot@example.com',$2,'10',true,$3)`, account, sealed, now)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var limited atomic.Bool
	limited.Store(true)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"access_token":"access","expires_in":3600}`))
			return
		}
		if r.URL.Path != "/gmail/v1/users/me/history" {
			t.Errorf("unexpected fetch: %s", r.URL)
			w.WriteHeader(500)
			return
		}
		calls.Add(1)
		if limited.Load() {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"historyId":"11"}`))
	}))
	defer fake.Close()
	g := &Google{APIURL: fake.URL, TokenURL: fake.URL + "/token"}
	first := &Service{DB: d.Owner, Google: g, Key: key, Now: func() time.Time { return now }}
	second := &Service{DB: dbtest.Pool(t, d), Google: g, Key: key, Now: func() time.Time { return now }}
	if err := first.Poll(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := second.Poll(ctx, account); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("retry ignored durable deadline: %d calls", calls.Load())
	}
	var next time.Time
	var history string
	var retries int
	if err := reader.QueryRow(ctx, "SELECT next_poll_at,history_id,retry_count FROM gmail_connections").Scan(&next, &history, &retries); err != nil {
		t.Fatal(err)
	}
	if next.Before(now.Add(60*time.Second)) || history != "10" || retries != 1 {
		t.Fatalf("next=%v history=%s retries=%d", next, history, retries)
	}
	limited.Store(false)
	now = next.Add(time.Second)
	var wg sync.WaitGroup
	for _, s := range []*Service{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Poll(ctx, account); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 2 {
		t.Fatalf("concurrent polls: %d calls", calls.Load())
	}
	if err := reader.QueryRow(ctx, "SELECT history_id,retry_count FROM gmail_connections").Scan(&history, &retries); err != nil {
		t.Fatal(err)
	}
	if history != "11" || retries != 0 {
		t.Fatalf("history=%s retries=%d", history, retries)
	}
	if err := db.CheckContract(ctx, d.Owner, d.Readonly); err != nil {
		t.Fatal(err)
	}
}

func TestLatePollAcrossSecondBoundaryDoesNotSkipNextTick(t *testing.T) {
	d := dbtest.New(t)
	_ = dbtest.Reader(t, d, "cadence")
	var account int64
	if err := d.Owner.QueryRow(t.Context(), "SELECT account_id FROM host_keys WHERE host='cadence'").Scan(&account); err != nil {
		t.Fatal(err)
	}
	key := testKey(t)
	sealed, err := key.Seal("refresh")
	if err != nil {
		t.Fatal(err)
	}
	tick := time.Date(2026, 11, 10, 12, 0, 0, 998000000, time.UTC)
	_, err = d.Owner.Exec(t.Context(), `INSERT INTO gmail_connections(account_id,address,token,history_id,backfilled,connected_at) VALUES($1,'bot@example.com',$2,'10',true,$3)`, account, sealed, tick)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"access_token":"access","expires_in":3600}`))
			return
		}
		calls.Add(1)
		_, _ = w.Write([]byte(`{"historyId":"10"}`))
	}))
	defer fake.Close()
	now := tick
	s := &Service{DB: d.Owner, Key: key, Google: &Google{APIURL: fake.URL, TokenURL: fake.URL + "/token"}, Now: func() time.Time { return now }}
	for i := 0; i < 4; i++ {
		// The ticker fires just before the boundary, but the worker starts after it.
		// Check the scheduler's due predicate at the tick, before advancing the clock.
		var due bool
		if err := d.Owner.QueryRow(t.Context(), "SELECT next_poll_at <= $2 FROM gmail_connections WHERE account_id=$1", account, tick).Scan(&due); err != nil {
			t.Fatal(err)
		}
		if !due {
			t.Fatalf("tick %d skipped: mailbox slipped to a two-second cadence", i)
		}
		now = tick.Add(5 * time.Millisecond)
		if err := s.Poll(t.Context(), account); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != int32(i+1) {
			t.Fatalf("tick %d: %d calls", i, calls.Load())
		}
		tick = tick.Add(time.Second)
	}
}

func TestRetryDoesNotRefetchMessagesSavedBeforeRateLimit(t *testing.T) {
	d := dbtest.New(t)
	_ = dbtest.Reader(t, d, "partial-batch")
	var account int64
	if err := d.Owner.QueryRow(t.Context(), "SELECT account_id FROM host_keys WHERE host='partial-batch'").Scan(&account); err != nil {
		t.Fatal(err)
	}
	key := testKey(t)
	sealed, err := key.Seal("refresh")
	if err != nil {
		t.Fatal(err)
	}
	var fetchedA, fetchedB atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/history":
			_, _ = w.Write([]byte(`{"historyId":"12","history":[{"messagesAdded":[{"message":{"id":"a"}},{"message":{"id":"b"}}]}]}`))
		case "/gmail/v1/users/me/messages/a":
			fetchedA.Add(1)
			_, _ = w.Write([]byte(`{"id":"a","threadId":"thread","labelIds":["INBOX"],"internalDate":"1000"}`))
		case "/gmail/v1/users/me/messages/b":
			if fetchedB.Add(1) == 1 {
				w.WriteHeader(429)
				_, _ = w.Write([]byte(`{}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"b","threadId":"thread","labelIds":["INBOX"],"internalDate":"1000"}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer fake.Close()
	g := &Google{APIURL: fake.URL}
	mb := g.Mailbox("refresh")
	mb.access = "access"
	mb.expiry = time.Now().Add(time.Hour)
	s := &Service{DB: d.Owner, Key: key, Google: g, mailboxes: map[int64]*Mailbox{account: mb}}
	history := "10"
	backfilled := true
	var pending []string
	if err := s.read(t.Context(), account, nil, sealed, time.Now(), &history, &backfilled, &pending); err == nil {
		t.Fatal("expected rate limit partway through batch")
	}
	if history != "10" {
		t.Fatalf("history advanced past failed message: %s", history)
	}
	if err := s.read(t.Context(), account, nil, sealed, time.Now(), &history, &backfilled, &pending); err != nil {
		t.Fatal(err)
	}
	if got := fetchedA.Load(); got != 1 {
		t.Fatalf("saved message fetched %d times; want once", got)
	}
	if got := fetchedB.Load(); got != 2 {
		t.Fatalf("failed message fetched %d times; want twice", got)
	}
	if history != "12" {
		t.Fatalf("history did not advance after successful retry: %s", history)
	}
}
