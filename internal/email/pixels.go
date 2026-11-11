package email

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const maxPixels = 5

// pixels deliberately requires image markup indicating a tiny or hidden image. A tracker host
// alone is insufficient: trackers also serve ordinary content. Attachments are not rendered.
func (g GmailMessage) pixels() []string {
	var out []string
	walk(g.Payload, func(p Part) {
		if p.MimeType != "text/html" || p.Filename != "" || len(out) >= maxPixels {
			return
		}
		raw := decode(p.Body.Data)
		if len(raw) > 1<<20 {
			return
		}
		z := html.NewTokenizer(strings.NewReader(raw))
		for len(out) < maxPixels {
			tt := z.Next()
			if tt == html.ErrorToken {
				break
			}
			if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
				continue
			}
			tok := z.Token()
			if tok.Data != "img" {
				continue
			}
			attrs := map[string]string{}
			for _, a := range tok.Attr {
				if _, ok := attrs[a.Key]; !ok {
					attrs[a.Key] = strings.ToLower(strings.TrimSpace(a.Val))
					if a.Key == "src" {
						attrs[a.Key] = strings.TrimSpace(a.Val)
					}
				}
			}
			width, height := attrs["width"], attrs["height"]
			hidden := false
			for _, decl := range strings.Split(attrs["style"], ";") {
				k, v, ok := strings.Cut(decl, ":")
				if !ok {
					continue
				}
				k, v = strings.TrimSpace(k), strings.TrimSpace(v)
				switch k {
				case "width":
					width = v
				case "height":
					height = v
				case "display":
					hidden = hidden || v == "none"
				case "visibility":
					hidden = hidden || v == "hidden"
				}
			}
			_, hasHidden := attrs["hidden"]
			if !hidden && !hasHidden && !(tinyDimension(width) && tinyDimension(height)) {
				continue
			}
			u, err := url.Parse(attrs["src"])
			if err != nil || !validPixelURL(u) {
				continue
			}
			u.Fragment = ""
			if s := u.String(); !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	})
	return out
}

func tinyDimension(s string) bool { return s == "0" || s == "1" || s == "0px" || s == "1px" }

func validPixelURL(u *url.URL) bool {
	return (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && len(u.String()) <= 8192
}

// Public IPv4 and native IPv6 only. Besides local ranges, reject transition mechanisms that
// could tunnel to a forbidden IPv4 destination, shared space, and reserved/documentation ranges.
func publicPixelIP(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.Zone() != "" || !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	if a.Is6() && !netip.MustParsePrefix("2000::/3").Contains(a) {
		return false
	}
	for _, p := range []string{"0.0.0.0/8", "100.64" + ".0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20"} {
		if netip.MustParsePrefix(p).Contains(a) {
			return false
		}
	}
	return true
}

// DNS is resolved once per connection; every answer is checked, then the checked address is
// dialed directly to prevent rebinding. Redirects use the same transport and checks. No proxy,
// cookie jar, user credentials, referer, or shared authenticated transport is used.
func pixelClient(lookup func(context.Context, string) ([]netip.Addr, error), allowed func(netip.Addr) bool) *http.Client {
	transport := &http.Transport{
		DisableKeepAlives: true, DisableCompression: true, MaxResponseHeaderBytes: 8192,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := lookup(ctx, host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, errors.New("no pixel address")
			}
			for _, ip := range ips {
				if !allowed(ip) {
					return nil, errors.New("non-public pixel address")
				}
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", net.JoinHostPort(ips[0].String(), port))
		},
	}
	return &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || !validPixelURL(req.URL) {
			return errors.New("pixel redirect refused")
		}
		req.Header = make(http.Header)
		return nil
	}}
}

func fetchPixel(ctx context.Context, c *http.Client, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !validPixelURL(u) {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return false
	}
	res, err := c.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	// Never drain an unbounded body (including compressed responses).
	n, err := io.Copy(io.Discard, io.LimitReader(res.Body, (16<<10)+1))
	return err == nil && n <= 16<<10 && res.StatusCode >= 200 && res.StatusCode < 300
}

// Bounded concurrency; busy and interrupted attempts are not retried. The committed row is an
// at-most-once marker, not a claim of delivery: a crash can lose an open, but cannot repeat it.
var pixelSlots = make(chan struct{}, 8)

func (s *Service) openPixels(id int64, urls []string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		status, attempted, opened := "busy", 0, 0
		select {
		case pixelSlots <- struct{}{}:
			defer func() { <-pixelSlots }()
			status = "finished"
			client := pixelClient(func(ctx context.Context, host string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			}, publicPixelIP)
			if s.pixelHTTP != nil {
				client = s.pixelHTTP
			}
			for _, u := range urls {
				if ctx.Err() != nil {
					break
				}
				attempted++
				if fetchPixel(ctx, client, u) {
					opened++
				}
			}
		default:
		}
		// Reserve a fresh short deadline for bookkeeping even when network work timed out.
		save, done := context.WithTimeout(context.Background(), 2*time.Second)
		defer done()
		_, _ = s.DB.Exec(save, `UPDATE email_pixel_opens SET finished_at = now(), status = $2, attempted = $3, opened = $4 WHERE email_id = $1`, id, status, attempted, opened)
	}()
}
