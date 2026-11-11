package email

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimitErrors(t *testing.T) {
	for _, tt := range []struct {
		status      int
		body, retry string
		limited     bool
	}{
		{429, `{}`, "12", true},
		{403, `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`, "", true},
		{403, `{"error":{"errors":[{"reason":"userRateLimitExceeded"}]}}`, "", true},
		{403, `{"error":{"errors":[{"reason":"forbidden"}]}}`, "", false},
		{503, `{}`, "", true},
	} {
		t.Run(tt.body+tt.retry, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", tt.retry)
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			mb := (&Google{APIURL: srv.URL}).Mailbox("refresh")
			mb.access = "access"
			mb.expiry = time.Now().Add(time.Hour)
			_, _, err := mb.Added(t.Context(), "1")
			var rate *retryError
			if errors.As(err, &rate) != tt.limited {
				t.Fatalf("error = %v, want limited %v", err, tt.limited)
			}
			if tt.retry != "" && rate.Delay != 12*time.Second {
				t.Fatalf("delay=%v", rate.Delay)
			}
		})
	}
}

func TestQuotaPacingAndCancellation(t *testing.T) {
	now := time.Now()
	p := pacer{Next: now}
	if err := p.Wait(t.Context(), 60); err != nil {
		t.Fatal(err)
	}
	if p.Next.Before(now.Add(1200 * time.Millisecond)) {
		t.Fatalf("next=%v", p.Next)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := p.Next
	if err := p.Wait(ctx, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if !p.Next.Equal(before) {
		t.Fatal("cancelled wait charged quota")
	}
}

func TestRetryAfterAndBackoff(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	if got := retryAfter(now.Add(time.Minute).Format(http.TimeFormat), now); got != time.Minute {
		t.Fatal(got)
	}
	if got := retryAfter("bad", now); got != 0 {
		t.Fatal(got)
	}
	for _, n := range []int{0, 1, 6, 100} {
		got := retryDelay(n, 0)
		base := time.Second * time.Duration(1<<min(n, 6))
		if got < base || got > base+time.Second {
			t.Fatalf("delay(%d)=%v", n, got)
		}
	}
	if got := retryDelay(1, time.Hour); got != time.Hour {
		t.Fatal(got)
	}
}

func TestHistoryPollUsesOnlyIncrementalPagesAndDeduplicatesMessages(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/gmail/v1/users/me/history" || r.URL.Query().Get("startHistoryId") != "10" || r.URL.Query().Get("historyTypes") != "messageAdded" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		if calls == 1 {
			_, _ = w.Write([]byte(`{"historyId":"12","nextPageToken":"next","history":[{"messagesAdded":[{"message":{"id":"a"}}]}]}`))
			return
		}
		if r.URL.Query().Get("pageToken") != "next" {
			t.Error("missing page token")
		}
		_, _ = w.Write([]byte(`{"historyId":"13","history":[{"messagesAdded":[{"message":{"id":"a"}},{"message":{"id":"b"}}]}]}`))
	}))
	defer srv.Close()
	mb := (&Google{APIURL: srv.URL}).Mailbox("refresh")
	mb.access = "access"
	mb.expiry = time.Now().Add(time.Hour)
	p := &pacer{}
	ids, history, err := mb.Added(context.WithValue(t.Context(), paceKey{}, p), "10")
	if err != nil || history != "13" || len(ids) != 2 || ids[0] != "a" || ids[1] != "b" || calls != 2 {
		t.Fatalf("ids=%v history=%s calls=%d error=%v", ids, history, calls, err)
	}
	if p.Next.IsZero() {
		t.Fatal("history requests bypassed quota pacing")
	}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), paceKey{}, &pacer{Next: time.Now().Add(time.Hour)}))
	cancel()
	if _, _, err := mb.Added(ctx, "13"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("cancelled pacing made an HTTP request")
	}
}

func TestBackfillYieldsBetweenMessages(t *testing.T) {
	gets := []string{}
	searches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/history":
			_, _ = w.Write([]byte(`{"historyId":"11"}`))
		case "/gmail/v1/users/me/messages":
			searches++
			_, _ = w.Write([]byte(`{"messages":[{"id":"a"},{"id":"b"},{"id":"c"}]}`))
		default:
			gets = append(gets, r.URL.Path)
			_, _ = w.Write([]byte(`{"id":"old","internalDate":"1000","labelIds":["SENT"]}`))
		}
	}))
	defer srv.Close()
	key := testKey(t)
	token, err := key.Seal("refresh")
	if err != nil {
		t.Fatal(err)
	}
	g := &Google{APIURL: srv.URL}
	mb := g.Mailbox("refresh")
	mb.access = "access"
	mb.expiry = time.Now().Add(time.Hour)
	s := &Service{Google: g, Key: key, mailboxes: map[int64]*Mailbox{1: mb}}
	history := "10"
	backfilled := false
	var pending []string
	for i := 0; i < 3; i++ {
		if err := s.read(t.Context(), 1, nil, token, time.Now(), &history, &backfilled, &pending); err != nil {
			t.Fatal(err)
		}
		if len(gets) != i+1 || len(pending) != 2-i || backfilled != (i == 2) {
			t.Fatalf("poll %d: gets=%v pending=%v backfilled=%v", i, gets, pending, backfilled)
		}
	}
	if searches != 1 {
		t.Fatalf("searched %d times", searches)
	}
}
