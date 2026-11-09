// Package email connects an account's Gmail mailbox to its workflows: it reads each new email,
// keeps it in the emails table, and starts, steers or answers the runs the account's email
// triggers say (workflow.EmailFilter).
package email

import (
	"encoding/base64"
	"html"
	"net/mail"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zachlatta/intelligence/internal/workflow"
)

// GmailMessage is a message as Gmail's API returns it with format=full.
type GmailMessage struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId"`
	LabelIDs     []string `json:"labelIds,omitempty"`
	InternalDate string   `json:"internalDate,omitempty"` // milliseconds since the epoch, as a string
	Payload      Part     `json:"payload"`
}

// Part is one MIME part; the payload is the top one.
type Part struct {
	MimeType string   `json:"mimeType"`
	Filename string   `json:"filename,omitempty"`
	Headers  []Header `json:"headers,omitempty"`
	Body     Body     `json:"body"`
	Parts    []Part   `json:"parts,omitempty"`
}

// Header is one header, in the order the message has them.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Body is a part's content: inline as base64url, or an attachment fetched apart.
type Body struct {
	AttachmentID string `json:"attachmentId,omitempty"`
	Size         int    `json:"size"`
	Data         string `json:"data,omitempty"`
}

// maxText is how much of an email's text is kept.
const maxText = 64 << 10

// headers are the message's values of the header name, in order.
func (g GmailMessage) headers(name string) []string {
	var out []string
	for _, h := range g.Payload.Headers {
		if strings.EqualFold(h.Name, name) {
			out = append(out, h.Value)
		}
	}
	return out
}

func (g GmailMessage) header(name string) string {
	if v := g.headers(name); len(v) > 0 {
		return v[0]
	}
	return ""
}

func (g GmailMessage) has(label string) bool { return slices.Contains(g.LabelIDs, label) }

// Ours: the mailbox sent it (or it is a draft): it is no one's request.
func (g GmailMessage) Ours() bool { return g.has("SENT") || g.has("DRAFT") }

// Junk: spam or trash.
func (g GmailMessage) Junk() bool { return g.has("SPAM") || g.has("TRASH") }

// Bulk: a mailing list or newsletter, which never starts a run (a newsletter may well come from a
// sender a trigger names).
func (g GmailMessage) Bulk() bool {
	for _, h := range []string{"List-Id", "List-Unsubscribe", "List-Post"} {
		if g.header(h) != "" {
			return true
		}
	}
	p := strings.ToLower(strings.TrimSpace(g.header("Precedence")))
	return p == "bulk" || p == "list" || p == "junk"
}

// Received is when Gmail received it.
func (g GmailMessage) Received() time.Time {
	ms, err := strconv.ParseInt(g.InternalDate, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// from is the message's one From address, lowercase, and its name.
func (g GmailMessage) from() (name, addr string, ok bool) {
	v := g.headers("From")
	if len(v) != 1 {
		return "", "", false
	}
	a, err := mail.ParseAddress(v[0])
	if err != nil {
		return "", strings.ToLower(strings.TrimSpace(v[0])), false
	}
	return a.Name, strings.ToLower(a.Address), true
}

var (
	dmarcPass = regexp.MustCompile(`(?i)\bdmarc=pass\b[^;]*\bheader\.from=([a-z0-9.-]+)`)
	spaces    = regexp.MustCompile(`\s+`)
)

// Verify says whether Gmail found that the From address's domain sent the message (DMARC passed
// for it), and why. Only the first Authentication-Results counts: Gmail adds its own above the
// message's, so any below it could be forged.
func (g GmailMessage) Verify() (bool, string) {
	_, addr, ok := g.from()
	if !ok {
		return false, "not one From address"
	}
	domain := addr[strings.LastIndex(addr, "@")+1:]
	ar := g.headers("Authentication-Results")
	if len(ar) == 0 {
		return false, "no Authentication-Results from Gmail"
	}
	verdict := spaces.ReplaceAllString(strings.TrimSpace(ar[0]), " ")
	if !strings.HasPrefix(strings.ToLower(verdict), "mx.google.com") {
		return false, "the first Authentication-Results is not from Gmail"
	}
	if m := dmarcPass.FindStringSubmatch(verdict); m == nil || !strings.EqualFold(m[1], domain) {
		return false, "DMARC did not pass for " + domain
	}
	return true, "DMARC passed for " + domain
}

// Mail is the message as a filter reads it.
func (g GmailMessage) Mail() workflow.Mail {
	name, addr, _ := g.from()
	ok, _ := g.Verify()
	m := workflow.Mail{MessageID: g.ID, ThreadID: g.ThreadID, FromName: name, From: addr, Verified: ok,
		Subject: strings.TrimSpace(g.header("Subject")), Date: g.header("Date")}
	for _, h := range []string{"To", "Cc", "Delivered-To"} {
		for _, v := range g.headers(h) {
			list, err := mail.ParseAddressList(v)
			if err != nil {
				continue
			}
			for _, a := range list {
				if s := strings.ToLower(a.Address); !slices.Contains(m.To, s) {
					m.To = append(m.To, s)
				}
			}
		}
	}
	var plain, htmlText string
	walk(g.Payload, func(p Part) {
		switch {
		case p.Filename != "":
			m.Attachments = append(m.Attachments, p.Filename)
		case p.MimeType == "text/plain" && plain == "":
			plain = decode(p.Body.Data)
		case p.MimeType == "text/html" && htmlText == "":
			htmlText = decode(p.Body.Data)
		}
	})
	text := strings.TrimSpace(strings.ReplaceAll(plain, "\r\n", "\n"))
	if text == "" && htmlText != "" {
		text = textOf(htmlText)
	}
	text = TrimQuoted(text)
	if len(text) > maxText {
		text = strings.ToValidUTF8(text[:maxText], "") + "\n[cut]"
	}
	m.Text = text
	return m
}

func walk(p Part, f func(Part)) {
	f(p)
	for _, c := range p.Parts {
		walk(c, f)
	}
}

func decode(data string) string {
	b, err := base64.URLEncoding.DecodeString(data)
	if err != nil {
		b, err = base64.RawURLEncoding.DecodeString(data)
	}
	if err != nil {
		return ""
	}
	return string(b)
}

var (
	invisible = regexp.MustCompile(`(?is)<(style|script|head)\b.*?</(style|script|head)>`)
	breaks    = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|li|tr|h[1-6]|blockquote)>`)
	tags      = regexp.MustCompile(`<[^>]*>`)
)

// textOf is HTML as plain text: one line per paragraph, no blank lines.
func textOf(s string) string {
	s = invisible.ReplaceAllString(s, "")
	s = breaks.ReplaceAllString(s, "\n")
	s = html.UnescapeString(tags.ReplaceAllString(s, ""))
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}
