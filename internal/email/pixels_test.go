package email

import (
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPixelDetection(t *testing.T) {
	for _, markup := range []string{
		`<img width="1" height="1" style="height:1px !important;width:1px !important;border-width:0 !important;margin-top:0 !important;margin-bottom:0 !important;margin-right:0 !important;margin-left:0 !important;padding-top:0 !important;padding-bottom:0 !important;padding-right:0 !important;padding-left:0 !important;" src="https://example.com/p">`,
		`<img style="display:none !important" src="https://example.com/p">`,
		`<img style="display:none!important" src="https://example.com/p">`,
		`<img style="width:1px !IMPORTANT;height:1px!important" src="https://example.com/p">`,
		`<img width=1 height=1 style="width:auto;height:inherit" src="https://example.com/p">`,
		`<img style="max-height:0" src="https://example.com/p">`,
		`<img style="max-height:0px!important;overflow:hidden" src="https://example.com/p">`,
		`<img width="1.0pt" height="0.0pt" src="https://example.com/p">`,
		`<img width="1.0" height="0.0" src="https://example.com/p">`,
		`<img width=1 height=1 src="//example.com/p">`,
	} {
		g := GmailMessage{Payload: Part{MimeType: "text/html", Body: Body{Data: base64.RawURLEncoding.EncodeToString([]byte(markup))}}}
		if got := g.pixels(); !reflect.DeepEqual(got, []string{"https://example.com/p"}) {
			t.Errorf("markup %s: pixels=%v", markup, got)
		}
	}
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

func TestQuotedPixelsAreSkipped(t *testing.T) {
	pixel := `<img width=1 height=1 src="https://example.com/quoted">`
	for _, quoted := range []string{
		"<blockquote>" + pixel + "</blockquote>",
		`<blockquote type="cite"><div>` + pixel + `</div></blockquote>`,
		`<div class="other gmail_quote"><div>` + pixel + `</div></div>`,
		`<section class="gmail_quote_container">` + pixel + `</section>`,
		`<div class="yahoo_quoted">` + pixel + `</div>`,
		`<div id="divRplyFwdMsg">headers</div>` + pixel,
		`<div id="appendonsend"></div>` + pixel,
	} {
		markup := `<img width=1 height=1 src="https://example.com/current">` + quoted
		g := GmailMessage{Payload: Part{MimeType: "text/html", Body: Body{Data: base64.RawURLEncoding.EncodeToString([]byte(markup))}}}
		if got := g.pixels(); !reflect.DeepEqual(got, []string{"https://example.com/current"}) {
			t.Errorf("quoted %s: pixels=%v", quoted, got)
		}
	}
}

func TestPixelPorts(t *testing.T) {
	for raw, want := range map[string]bool{"http://example.com": true, "https://example.com": true, "http://example.com:80/p": true, "https://example.com:443/p": true, "http://example.com:8080/p": false, "https://example.com:8443/p": false, "http://example.com:443/p": false} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := validPixelURL(u); got != want {
			t.Errorf("validPixelURL(%s)=%v", raw, got)
		}
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
		if ua := r.UserAgent(); !strings.HasPrefix(ua, "Mozilla/5.0") {
			t.Errorf("not browser-like User-Agent: %q", ua)
		}
		if r.URL.Path == "/safe-redirect" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		if r.URL.Path == "/port-redirect" {
			http.Redirect(w, r, "http://example.com:8080/p", http.StatusFound)
			return
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
	localURL := routeTestPixelClient(t, client, server)
	if ok := fetchPixel(context.Background(), client, localURL+"/safe-redirect"); !ok {
		t.Fatal("fetch failed")
	}
	if ok := fetchPixel(context.Background(), client, localURL); !ok {
		t.Fatal("second fetch failed")
	}
	// The production guard refuses the loopback server, even through DNS.
	guarded := pixelClient(func(ctx context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}, publicPixelIP)
	routeTestPixelClient(t, guarded, server)
	if fetchPixel(context.Background(), guarded, localURL) {
		t.Fatal("guard bypassed")
	}
	if hits != 3 {
		t.Fatalf("hits=%d", hits)
	}
	if fetchPixel(context.Background(), client, localURL+"/port-redirect") {
		t.Fatal("nondefault port redirect followed")
	}
	// A redirect to a different hostname is resolved and guarded again.
	redirect := pixelClient(func(ctx context.Context, host string) ([]netip.Addr, error) {
		if host == "localhost" {
			return []netip.Addr{netip.MustParseAddr("::1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}, func(a netip.Addr) bool { return a.Is4() })
	routeTestPixelClient(t, redirect, server)
	if fetchPixel(context.Background(), redirect, localURL+"/redirect") {
		t.Fatal("unsafe redirect followed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if fetchPixel(ctx, client, localURL) {
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
	localURL := routeTestPixelClient(t, client, server)
	if fetchPixel(context.Background(), client, localURL) {
		t.Fatal("oversized response accepted")
	}
	mixed := pixelClient(func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil
	}, publicPixelIP)
	routeTestPixelClient(t, mixed, server)
	if fetchPixel(context.Background(), mixed, localURL) {
		t.Fatal("mixed DNS answers accepted")
	}
	for _, raw := range []string{"file:///secret", "ftp://example.com/a", "http://user:pass@example.com/a"} {
		if fetchPixel(context.Background(), client, raw) {
			t.Errorf("accepted %s", raw)
		}
	}
}

// Keep production URL port checks enabled while routing default-port URLs to httptest's port.
func routeTestPixelClient(t *testing.T, client *http.Client, server *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*http.Transport)
	dial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		return dial(ctx, network, net.JoinHostPort(host, u.Port()))
	}
	return "http://" + u.Hostname()
}
