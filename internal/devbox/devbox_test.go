package devbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func TestSendPostsReportWithBearerToken(t *testing.T) {
	var got Report
	var auth, ctype, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, ctype, path = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	r := Report{Name: "devbox-1", CollectedAt: t0, Version: "v1", Paseo: PaseoStatus{Connected: true, Version: "0.15.3"}}
	if _, err := SendReport(context.Background(), srv.Client(), srv.URL+"/", "secret", r); err != nil {
		t.Fatal(err)
	}
	if path != ReportPath || auth != "Bearer secret" || !strings.HasPrefix(ctype, "application/json") {
		t.Errorf("path %q auth %q content type %q", path, auth, ctype)
	}
	if got.Name != "devbox-1" || !got.Paseo.Connected || got.Paseo.Version != "0.15.3" || !got.CollectedAt.Equal(t0) {
		t.Errorf("server got %+v", got)
	}
}

func TestSendFailsOnRejectedReport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad token", http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := SendReport(context.Background(), srv.Client(), srv.URL, "wrong", Report{Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad token") {
		t.Fatalf("err = %v, want the status and the server's message", err)
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"devbox-1", "box_1", "a.b"} {
		if !ValidName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "has space", "<x>", "Upper", strings.Repeat("a", 65)} {
		if ValidName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
