package devbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// binServer serves a fake binary for linux/amd64 at version v, counting downloads.
type binServer struct {
	v, body   string
	badSum    bool
	downloads int
}

func (b *binServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != BinaryPath("linux", "amd64") {
		http.NotFound(w, r)
		return
	}
	b.downloads++
	sum := sha256.Sum256([]byte(b.body))
	if b.badSum {
		sum[0]++
	}
	w.Header().Set(VersionHeader, b.v)
	w.Header().Set(SHA256Header, hex.EncodeToString(sum[:]))
	_, _ = w.Write([]byte(b.body))
}

type updaterRig struct {
	u        *Updater
	srv      *binServer
	exe      string
	now      time.Time
	execed   []string
	verified []string
}

func newRig(t *testing.T, current, serverVersion string) *updaterRig {
	t.Helper()
	dir := t.TempDir()
	r := &updaterRig{srv: &binServer{v: serverVersion, body: "new binary " + serverVersion}, exe: filepath.Join(dir, "intelligence"), now: t0}
	if err := os.WriteFile(r.exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(r.srv)
	t.Cleanup(hs.Close)
	r.u = &Updater{
		Server: hs.URL, Client: hs.Client(), Current: current, Exe: r.exe, OS: "linux", Arch: "amd64",
		Now: func() time.Time { return r.now },
		Verify: func(_ context.Context, path string) (string, error) {
			r.verified = append(r.verified, path)
			b, err := os.ReadFile(path)
			return strings.TrimPrefix(string(b), "new binary "), err
		},
		Exec: func(path string) error { r.execed = append(r.execed, path); return nil },
	}
	return r
}

func (r *updaterRig) exeBody(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(r.exe)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNoUpdateWhenVersionsMatch(t *testing.T) {
	r := newRig(t, "abc", "abc")
	if err := r.u.Maybe(context.Background(), "abc"); err != nil {
		t.Fatal(err)
	}
	if r.srv.downloads != 0 || len(r.execed) != 0 {
		t.Fatalf("downloads %d execs %v", r.srv.downloads, r.execed)
	}
}

func TestNoUpdateWithoutAServerVersion(t *testing.T) {
	r := newRig(t, "abc", "def")
	if err := r.u.Maybe(context.Background(), ""); err != nil || r.srv.downloads != 0 {
		t.Fatalf("err %v downloads %d", err, r.srv.downloads)
	}
}

func TestUpdateReplacesTheBinaryAndReExecs(t *testing.T) {
	r := newRig(t, "abc", "def")
	if err := r.u.Maybe(context.Background(), "def"); err != nil {
		t.Fatal(err)
	}
	if got := r.exeBody(t); got != "new binary def" {
		t.Fatalf("exe = %q", got)
	}
	if fi, _ := os.Stat(r.exe); fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("new binary is not executable: %v", fi.Mode())
	}
	if len(r.verified) != 1 || r.verified[0] == r.exe {
		t.Errorf("verified %v: want the downloaded file checked before it replaces the binary", r.verified)
	}
	if len(r.execed) != 1 || r.execed[0] != r.exe {
		t.Errorf("execed %v, want %s", r.execed, r.exe)
	}
	if left, _ := filepath.Glob(r.exe + ".*"); len(left) != 0 {
		t.Errorf("leftover files %v", left)
	}
}

func TestUpdateChecksAtMostOncePerMinute(t *testing.T) {
	r := newRig(t, "abc", "def")
	r.srv.badSum = true // every attempt fails, so each one is a fresh try
	ctx := context.Background()
	_ = r.u.Maybe(ctx, "def")
	r.now = r.now.Add(30 * time.Second)
	_ = r.u.Maybe(ctx, "def")
	if r.srv.downloads != 1 {
		t.Fatalf("downloads = %d within a minute, want 1", r.srv.downloads)
	}
	r.now = r.now.Add(31 * time.Second)
	_ = r.u.Maybe(ctx, "def")
	if r.srv.downloads != 2 {
		t.Fatalf("downloads = %d after a minute, want 2", r.srv.downloads)
	}
}

func TestUpdateRefusesABadChecksum(t *testing.T) {
	r := newRig(t, "abc", "def")
	r.srv.badSum = true
	if err := r.u.Maybe(context.Background(), "def"); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err = %v", err)
	}
	if r.exeBody(t) != "old binary" || len(r.execed) != 0 {
		t.Fatalf("binary replaced or execed despite a bad checksum")
	}
}

func TestUpdateRefusesABinaryThatIsNotTheAdvertisedVersion(t *testing.T) {
	r := newRig(t, "abc", "def")
	r.srv.body = "new binary zzz" // claims def, is zzz
	if err := r.u.Maybe(context.Background(), "def"); err == nil {
		t.Fatal("want an error")
	}
	if r.exeBody(t) != "old binary" || len(r.execed) != 0 {
		t.Fatalf("binary replaced or execed though it is the wrong version")
	}
}

func TestUpdateRefusesABinaryThatDoesNotRun(t *testing.T) {
	r := newRig(t, "abc", "def")
	r.u.Verify = func(context.Context, string) (string, error) { return "", errors.New("exec format error") }
	if err := r.u.Maybe(context.Background(), "def"); err == nil || !strings.Contains(err.Error(), "exec format error") {
		t.Fatalf("err = %v", err)
	}
	if r.exeBody(t) != "old binary" || len(r.execed) != 0 {
		t.Fatalf("binary replaced or execed though it does not run")
	}
}

func TestSendReturnsTheServerVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(VersionHeader, "srv1")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	v, err := SendReport(context.Background(), srv.Client(), srv.URL, "t", Report{Name: "x"})
	if err != nil || v != "srv1" {
		t.Fatalf("version %q err %v", v, err)
	}
}

func TestSendReturnsTheServerVersionEvenWhenRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(VersionHeader, "srv2")
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	defer srv.Close()
	v, err := SendReport(context.Background(), srv.Client(), srv.URL, "t", Report{Name: "x"})
	if err == nil || v != "srv2" {
		t.Fatalf("version %q err %v: a refused report must still let the client update to a fixed server", v, err)
	}
}
