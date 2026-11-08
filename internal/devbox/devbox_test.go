package devbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

// fakePaseo answers `paseo status --json` and `paseo ls --json` from testdata files (or errors).
type fakePaseo struct {
	status, ls, providers string // testdata file names; "" = the command fails
	calls                 [][]string
}

func (f *fakePaseo) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	file := ""
	switch {
	case len(args) > 0 && args[0] == "status":
		file = f.status
	case len(args) > 0 && args[0] == "ls":
		file = f.ls
	case len(args) > 1 && args[0] == "provider" && args[1] == "ls":
		file = f.providers
	}
	if file == "" {
		return nil, errors.New("exec: \"paseo\": executable file not found in $PATH")
	}
	return os.ReadFile(filepath.Join("testdata", file))
}

func collector(f *fakePaseo) *Collector {
	now := t0
	return &Collector{Run: f.run, Now: func() time.Time { return now }}
}

func TestCollectReadsDaemonStatusProvidersAndAgents(t *testing.T) {
	f := &fakePaseo{status: "status-full.json", ls: "ls.json", providers: "providers.json"}
	s := collector(f).Paseo(context.Background())
	if s.Error != "" {
		t.Fatalf("error = %q", s.Error)
	}
	if s.Daemon != "running" || s.Connected != "reachable" || s.Version != "0.11.4" || s.PID != 3029 ||
		s.Hostname != "devbox-1" || s.Listen != "0.0.0.0:6767" {
		t.Errorf("daemon fields = %+v", s)
	}
	if want := time.Date(2026, 11, 8, 7, 37, 56, 968e6, time.UTC); !s.StartedAt.Equal(want) {
		t.Errorf("startedAt = %v, want %v", s.StartedAt, want)
	}
	if len(s.Providers) != 4 || s.Providers[0] != (Provider{Name: "claude", Status: "available", Available: true}) ||
		s.Providers[2] != (Provider{Name: "copilot", Status: "unavailable"}) || s.Providers[3] != (Provider{Name: "antigravity", Status: "error"}) {
		t.Errorf("providers = %+v", s.Providers)
	}
	if !s.ProvidersAt.Equal(t0) {
		t.Errorf("providersAt = %v, want %v", s.ProvidersAt, t0)
	}
	if s.Agents["running"] != 1 || s.Agents["idle"] != 2 || len(s.Agents) != 2 {
		t.Errorf("agents = %v", s.Agents)
	}
	for _, c := range f.calls {
		if c[0] != "paseo" || c[len(c)-1] != "--json" {
			t.Errorf("unexpected call %v", c)
		}
	}
}

func TestCollectKeepsLastProvidersWhenProviderListFails(t *testing.T) {
	f := &fakePaseo{status: "status-full.json", ls: "ls.json", providers: "providers.json"}
	c := collector(f)
	c.Paseo(context.Background())
	later := t0.Add(15 * time.Second)
	c.Now = func() time.Time { return later }
	f.providers = ""
	s := c.Paseo(context.Background())
	if len(s.Providers) != 4 {
		t.Fatalf("providers = %+v, want the last known 4", s.Providers)
	}
	if !s.ProvidersAt.Equal(t0) {
		t.Errorf("providersAt = %v, want the earlier read %v", s.ProvidersAt, t0)
	}
}

func TestCollectKeepsPaseoNote(t *testing.T) {
	s := collector(&fakePaseo{status: "status-timeout.json", ls: "ls.json", providers: "providers.json"}).Paseo(context.Background())
	if !strings.Contains(s.Note, "DAEMON_REQUEST_TIMEOUT") || len(s.Providers) != 4 {
		t.Errorf("note = %q, providers = %d", s.Note, len(s.Providers))
	}
}

func TestCollectReportsMissingPaseo(t *testing.T) {
	s := collector(&fakePaseo{}).Paseo(context.Background())
	if !strings.Contains(s.Error, "executable file not found") {
		t.Fatalf("error = %q", s.Error)
	}
}

func TestCollectReportsAgentListFailureSeparately(t *testing.T) {
	s := collector(&fakePaseo{status: "status-full.json", providers: "providers.json"}).Paseo(context.Background())
	if s.Error != "" || s.Daemon != "running" {
		t.Fatalf("daemon status lost: %+v", s)
	}
	if s.AgentsError == "" || s.Agents != nil {
		t.Errorf("agents = %v, agentsError = %q", s.Agents, s.AgentsError)
	}
}

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
	r := Report{Name: "devbox-1", CollectedAt: t0, Version: "v1", Paseo: PaseoStatus{Daemon: "running"}}
	if err := Send(context.Background(), srv.Client(), srv.URL+"/", "secret", r); err != nil {
		t.Fatal(err)
	}
	if path != ReportPath || auth != "Bearer secret" || !strings.HasPrefix(ctype, "application/json") {
		t.Errorf("path %q auth %q content type %q", path, auth, ctype)
	}
	if got.Name != "devbox-1" || got.Paseo.Daemon != "running" || !got.CollectedAt.Equal(t0) {
		t.Errorf("server got %+v", got)
	}
}

func TestSendFailsOnRejectedReport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad token", http.StatusUnauthorized)
	}))
	defer srv.Close()
	err := Send(context.Background(), srv.Client(), srv.URL, "wrong", Report{Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad token") {
		t.Fatalf("err = %v, want the status and the server's message", err)
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"mew-dev", "box_1", "a.b"} {
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
