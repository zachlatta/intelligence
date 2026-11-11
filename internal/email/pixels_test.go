package email

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPixelDetection(t *testing.T) {
	html := `<img src="https://example.com/photo" width=600 height=400>
 <IMG SRC="https://example.com/open?a=1&amp;b=2" WIDTH="1" HEIGHT="1">
 <img src="https://example.com/hidden" style="display: none">
 <img src="https://example.com/css" style="width:1px;height:1px">
 <img src="https://example.com/open?a=1&amp;b=2" width=1 height=1>
 <img src="cid:attached" width=1 height=1>
 <img src="https://user:password@example.com/private" width=1 height=1>
 <img src="https://example.com/narrow" width=1 height=400>
 <script>var s = '<img src="https://example.com/script" width=1 height=1>';</script>`
	g := GmailMessage{Payload: Part{MimeType: "text/html", Body: Body{Data: base64.RawURLEncoding.EncodeToString([]byte(html))}}}
	want := []string{"https://example.com/open?a=1&b=2", "https://example.com/hidden", "https://example.com/css"}
	if got := g.pixels(); !reflect.DeepEqual(got, want) {
		t.Fatalf("pixels = %v, want %v", got, want)
	}
	g.Payload.Filename = "attached.html"
	if got := g.pixels(); len(got) != 0 {
		t.Fatalf("attachment pixels: %v", got)
	}
}

func TestPixelAddressGuard(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "::1", "0.0.0.0", "10.0" + ".0.1", "169.254" + ".169.254", "100.64" + ".0.1", "fc00::1", "fe80::1", "224.0.0.1", "::ffff:127.0.0.1", "2002::1", "64:ff9b::1"} {
		if publicPixelIP(netip.MustParseAddr(addr)) {
			t.Errorf("allowed %s", addr)
		}
	}
	if !publicPixelIP(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("rejected public address")
	}
}

func TestPixelFetchHasNoCredentialsAndChecksRedirects(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Referer") != "" {
			t.Error("credentials sent")
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://localhost/secret", http.StatusFound)
			return
		}
		w.Header().Set("Set-Cookie", "session=secret")
		w.Write([]byte("pixel"))
	}))
	defer server.Close()
	client := pixelClient(func(ctx context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}, func(netip.Addr) bool { return true })
	if ok := fetchPixel(context.Background(), client, server.URL); !ok {
		t.Fatal("fetch failed")
	}
	if ok := fetchPixel(context.Background(), client, server.URL); !ok {
		t.Fatal("second fetch failed")
	}
	// The production guard refuses the loopback server, even through DNS.
	guarded := pixelClient(func(ctx context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}, publicPixelIP)
	if fetchPixel(context.Background(), guarded, server.URL) {
		t.Fatal("guard bypassed")
	}
	if hits != 2 {
		t.Fatalf("hits=%d", hits)
	}
	// A redirect to a different hostname is resolved and guarded again.
	redirect := pixelClient(func(ctx context.Context, host string) ([]netip.Addr, error) {
		if host == "localhost" {
			return []netip.Addr{netip.MustParseAddr("::1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}, func(a netip.Addr) bool { return a.Is4() })
	if fetchPixel(context.Background(), redirect, server.URL+"/redirect") {
		t.Fatal("unsafe redirect followed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if fetchPixel(ctx, client, server.URL) {
		t.Fatal("ignored deadline")
	}
}

func TestPixelLimits(t *testing.T) {
	var markup strings.Builder
	for i := 0; i < 20; i++ {
		markup.WriteString(`<img src="https://example.com/` + strings.Repeat("x", i) + `" width=1 height=1>`)
	}
	g := GmailMessage{Payload: Part{MimeType: "text/html", Body: Body{Data: base64.RawURLEncoding.EncodeToString([]byte(markup.String()))}}}
	if got := g.pixels(); len(got) != maxPixels {
		t.Fatalf("pixel cap = %d", len(got))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat("x", 32<<10))) }))
	defer server.Close()
	lookup := func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	client := pixelClient(lookup, func(netip.Addr) bool { return true })
	if fetchPixel(context.Background(), client, server.URL) {
		t.Fatal("oversized response accepted")
	}
	mixed := pixelClient(func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil
	}, publicPixelIP)
	if fetchPixel(context.Background(), mixed, server.URL) {
		t.Fatal("mixed DNS answers accepted")
	}
	for _, raw := range []string{"file:///secret", "ftp://example.com/a", "http://user:pass@example.com/a"} {
		if fetchPixel(context.Background(), client, raw) {
			t.Errorf("accepted %s", raw)
		}
	}
}
