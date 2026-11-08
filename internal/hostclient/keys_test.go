package hostclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeJoin is the server's side of joining: each enrollment's code is claimed after `waits`
// collects have waited, unless `expire` is set, when the first enrollment expires instead.
type fakeJoin struct {
	t                 *testing.T
	mu                sync.Mutex
	enrolls, collects int
	waits             int
	expire            bool
	host              string
}

func (f *fakeJoin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set(VersionHeader, "server-v")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/host-enrollments":
		var req struct{ Host string }
		json.NewDecoder(r.Body).Decode(&req)
		f.host = req.Host
		f.enrolls++
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": "e" + string(rune('0'+f.enrolls)), "host": req.Host,
			"code": "ABCD-123" + string(rune('0'+f.enrolls)), "secret": "s3cret", "expiresAt": time.Now().Add(time.Hour)})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/key"):
		if r.Header.Get("Authorization") != "Bearer s3cret" || r.URL.Query().Get("wait") == "" {
			http.Error(w, "bad collect", http.StatusBadRequest)
			return
		}
		f.collects++
		if f.expire && f.enrolls == 1 {
			http.Error(w, "expired", http.StatusNotFound)
			return
		}
		if f.collects <= f.waits {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"key": "the-key", "host": f.host})
	default:
		http.NotFound(w, r)
	}
}

func TestAKeylessHostJoinsAndKeepsItsKey(t *testing.T) {
	srv := httptest.NewServer(&fakeJoin{t: t, waits: 2})
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "conf", "host.key")
	var logged []string
	var versions []string
	k := &Keys{File: file, Server: srv.URL, Host: "host-1", Logf: func(f string, a ...any) { logged = append(logged, f) },
		ServerVersion: func(v string) { versions = append(versions, v) }, RetryEvery: time.Millisecond}
	key, err := k.Key(context.Background())
	if err != nil || key != "the-key" {
		t.Fatalf("key %q %v", key, err)
	}
	st, err := os.Stat(file)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", st, err)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "code") {
		t.Errorf("the code was never shown: %q", logged)
	}
	if len(versions) == 0 || versions[0] != "server-v" {
		t.Errorf("a joining host does not follow the server's version: %v", versions)
	}
	// a restarted host reads its key from the file, without joining again
	again := &Keys{File: file, Server: "http://unreachable.invalid", Host: "host-1"}
	if key, err := again.Key(context.Background()); err != nil || key != "the-key" {
		t.Errorf("from the file: %q %v", key, err)
	}
}

func TestAnExpiredCodeIsReplacedWithANewOne(t *testing.T) {
	f := &fakeJoin{t: t, expire: true}
	srv := httptest.NewServer(f)
	defer srv.Close()
	k := &Keys{File: filepath.Join(t.TempDir(), "host.key"), Server: srv.URL, Host: "host-1", RetryEvery: time.Millisecond}
	if key, err := k.Key(context.Background()); err != nil || key != "the-key" || f.enrolls != 2 {
		t.Fatalf("key %q %v after %d enrollments", key, err, f.enrolls)
	}
}

func TestARefusedKeyIsForgottenSoTheHostJoinsAgain(t *testing.T) {
	file := filepath.Join(t.TempDir(), "host.key")
	os.WriteFile(file, []byte("old-key\n"), 0o600)
	k := &Keys{File: file, Host: "host-1"}
	k.Refused("some-other-key") // a key the file no longer holds: kept
	if b, _ := os.ReadFile(file); strings.TrimSpace(string(b)) != "old-key" {
		t.Fatalf("file = %q", b)
	}
	k.Refused("old-key")
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("the refused key is still on disk: %v", err)
	}
}
