package voicememo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPDWReadAndFailures(t *testing.T) {
	body := `{"feed":"feed:voice-memos","next_cursor":"MQ","recordings":[{"source":"apple_voice_memos","account":"owner","recording_id":"one","transcript":"hello","duration_seconds":15}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/external-feeds/voice-memos" || r.Header.Get("Authorization") != "Bearer intelligence:test-key" {
			t.Error("bad PDW request")
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()
	s := Service{URL: srv.URL, Key: "test-key"}
	rows, err := s.read(context.Background(), "")
	if err != nil || len(rows.Rows) != 1 || rows.Rows[0].ID != "one" {
		t.Fatalf("%+v %v", rows, err)
	}
	body = `{"data":{"error":"permission denied"}}`
	if _, err = s.read(context.Background(), ""); err == nil {
		t.Fatal("invalid feed accepted")
	}
	body = `{"feed":"feed:voice-memos","next_cursor":"MQ","recordings":[],"has_more":true}`
	if _, err = s.read(context.Background(), "MQ"); err == nil {
		t.Fatal("non-advancing cursor accepted")
	}
}
