package email

import (
	"bytes"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testKey(t *testing.T) *Keyring {
	t.Helper()
	k, err := NewKeyring(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestATokenIsSealedSoOnlyTheKeyOpensIt(t *testing.T) {
	k := testKey(t)
	sealed, err := k.Seal("a refresh token")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("refresh")) {
		t.Error("the sealed token shows the token")
	}
	if got, err := k.Open(sealed); err != nil || got != "a refresh token" {
		t.Errorf("Open = %q, %v", got, err)
	}
	other, _ := NewKeyring(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	if _, err := other.Open(sealed); err == nil {
		t.Error("another key opened it")
	}
	for _, bad := range []string{"", "short", base64.StdEncoding.EncodeToString([]byte("sixteen bytes!!!"))} {
		if _, err := NewKeyring(bad); err == nil {
			t.Errorf("key %q: no error", bad)
		}
	}
}

func TestTheSignInStateNamesItsAccountAndExpires(t *testing.T) {
	k := testKey(t)
	now := time.Unix(1762632000, 0)
	state := k.State(42, now)
	if acct, err := k.CheckState(state, now.Add(5*time.Minute)); err != nil || acct != 42 {
		t.Errorf("CheckState = %d, %v", acct, err)
	}
	if _, err := k.CheckState(state, now.Add(time.Hour)); err == nil {
		t.Error("an hour-old state was accepted")
	}
	tampered := strings.Replace(state, "42.", "43.", 1)
	if _, err := k.CheckState(tampered, now); err == nil {
		t.Error("a state for another account was accepted")
	}
	if v := k.Verifier(state); len(v) < 43 || v == k.Verifier(k.State(42, now.Add(time.Second))) {
		t.Errorf("verifier %q: too short, or the same for another state", v)
	}
}

func TestAGoogleClientIsReadFromItsJSON(t *testing.T) {
	installed := `{"installed":{"client_id":"id-1","client_secret":"s-1","auth_uri":"https://accounts.example.com/auth","token_uri":"https://oauth.example.com/token"}}`
	for _, raw := range []string{installed, base64.StdEncoding.EncodeToString([]byte(installed))} {
		g, err := ParseClient(raw)
		if err != nil {
			t.Fatal(err)
		}
		if g.ClientID != "id-1" || g.ClientSecret != "s-1" || !g.Installed || g.AuthURL != "https://accounts.example.com/auth" ||
			g.TokenURL != "https://oauth.example.com/token" || g.APIURL != "https://gmail.googleapis.com" {
			t.Errorf("client = %+v", g)
		}
	}
	g, err := ParseClient(`{"web":{"client_id":"id-2","client_secret":"s-2"}}`)
	if err != nil || g.Installed || g.AuthURL != "https://accounts.google.com/o/oauth2/v2/auth" {
		t.Errorf("web client = %+v, %v", g, err)
	}
	if _, err := ParseClient(`{"other":{}}`); err == nil {
		t.Error("no client: no error")
	}
}

func TestTheSignInLinkAsksOnlyToReadMail(t *testing.T) {
	g := &Google{ClientID: "id-1", AuthURL: "https://accounts.example.com/auth"}
	u, err := url.Parse(g.SignInURL("st", "challenge", "https://app.example.com/gmail/callback"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("scope") != "https://www.googleapis.com/auth/gmail.readonly" || q.Get("access_type") != "offline" || q.Get("prompt") != "consent" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != "challenge" || q.Get("state") != "st" ||
		q.Get("redirect_uri") != "https://app.example.com/gmail/callback" || q.Get("client_id") != "id-1" {
		t.Errorf("sign-in URL = %s", u)
	}
	if g.Redirect("https://app.example.com") != "https://app.example.com/gmail/callback" {
		t.Errorf("web redirect = %s", g.Redirect("https://app.example.com"))
	}
	g.Installed = true
	if g.Redirect("https://app.example.com") != "http://localhost" {
		t.Errorf("installed redirect = %s", g.Redirect("https://app.example.com"))
	}
}

func startBlackholeHTTP2Proxy(t *testing.T, handler http.Handler) (proxyURL string, srv *httptest.Server, blackhole *atomic.Bool, conns *atomic.Int32) {
	t.Helper()
	srv = httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	blackhole = &atomic.Bool{}
	conns = &atomic.Int32{}
	var mu sync.Mutex
	var open []net.Conn
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			clientConn, err := ln.Accept()
			if err != nil {
				return
			}
			n := conns.Add(1)
			backendConn, err := net.Dial("tcp", srv.Listener.Addr().String())
			if err != nil {
				_ = clientConn.Close()
				continue
			}
			mu.Lock()
			open = append(open, clientConn, backendConn)
			mu.Unlock()
			forward := func(dst, src net.Conn, dropWhenBlackholed bool) {
				defer wg.Done()
				buf := make([]byte, 4096)
				for {
					nr, rerr := src.Read(buf)
					if nr > 0 {
						if dropWhenBlackholed && n == 1 && blackhole.Load() {
							// Absorb client bytes without forwarding or closing, simulating a
							// half-open/blackholed TCP socket.
							continue
						}
						if _, werr := dst.Write(buf[:nr]); werr != nil {
							return
						}
					}
					if rerr != nil {
						return
					}
				}
			}
			wg.Add(2)
			go forward(backendConn, clientConn, true)
			go forward(clientConn, backendConn, false)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range open {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
		srv.Close()
	})
	return "https://" + ln.Addr().String(), srv, blackhole, conns
}

func TestBlackholedHTTP2ConnectionRecoversOnNextPoll(t *testing.T) {
	var calls atomic.Int32
	proxyURL, srv, blackhole, conns := startBlackholeHTTP2Proxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("expected HTTP/2, got %s", r.Proto)
		}
		n := calls.Add(1)
		if n == 1 {
			_, _ = io.WriteString(w, `{"historyId":"11"}`)
			return
		}
		_, _ = io.WriteString(w, `{"historyId":"12"}`)
	}))
	tr := newGoogleTransport(srv.Client().Transport.(*http.Transport), 50*time.Millisecond, 50*time.Millisecond)
	tr.ForceAttemptHTTP2 = true
	g := &Google{
		APIURL: proxyURL,
		HTTP:   &http.Client{Timeout: 5 * time.Second, Transport: tr},
	}
	mb := g.Mailbox("refresh")
	mb.access = "access"
	mb.expiry = time.Now().Add(time.Hour)

	if _, hist, err := mb.Added(t.Context(), "10"); err != nil || hist != "11" {
		t.Fatalf("first poll = %q, %v; want 11, nil", hist, err)
	}
	if got := conns.Load(); got != 1 {
		t.Fatalf("connections after first poll = %d; want 1", got)
	}

	// Blackhole the first TCP connection without closing it (no FIN/RST).
	// HTTP/2 PING health checks must tear down the dead connection well before
	// the 5s Client.Timeout expires.
	blackhole.Store(true)
	started := time.Now()
	if _, _, err := mb.Added(t.Context(), "11"); err == nil {
		t.Fatal("second poll succeeded on blackholed connection; want error")
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("second poll took %v; expected HTTP/2 ping health check to fail well before 5s timeout", elapsed)
	}

	// The next poll must dial a fresh connection and succeed.
	if _, hist, err := mb.Added(t.Context(), "11"); err != nil || hist != "12" {
		t.Fatalf("third poll after blackhole = %q, %v; want 12, nil (connections=%d)", hist, err, conns.Load())
	}
	if got := conns.Load(); got < 2 {
		t.Fatalf("connections after recovery = %d; want >= 2", got)
	}
}

func TestDefaultGoogleClientConfiguresHTTP2HealthChecks(t *testing.T) {
	g := &Google{}
	c1 := g.client()
	c2 := g.client()
	if c1 != c2 {
		t.Fatal("default client was recreated on every call instead of reused")
	}
	if c1.Transport == nil || c1.Transport == http.DefaultTransport {
		t.Fatal("default client must use a dedicated Transport, not http.DefaultTransport")
	}
	tr, ok := c1.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default transport type = %T; want *http.Transport", c1.Transport)
	}
	if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout <= 0 || tr.HTTP2.PingTimeout <= 0 {
		t.Fatalf("HTTP2 ping health checks not configured: %+v", tr.HTTP2)
	}
	if tr.HTTP2.SendPingTimeout+tr.HTTP2.PingTimeout >= c1.Timeout {
		t.Fatalf("HTTP2 ping timeout (%v + %v) must finish before Client.Timeout (%v)",
			tr.HTTP2.SendPingTimeout, tr.HTTP2.PingTimeout, c1.Timeout)
	}
}
