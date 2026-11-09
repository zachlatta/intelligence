package email

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func b64(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }

const passAR = "mx.google.com; dkim=pass header.i=@example.com header.s=s1; spf=pass smtp.mailfrom=christina@example.com; " +
	"dmarc=pass (p=NONE sp=NONE dis=NONE) header.from=example.com"

func gmailJSON(t *testing.T, m GmailMessage) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAGmailMessageIsReadAsMail(t *testing.T) {
	raw := gmailJSON(t, GmailMessage{ID: "m1", ThreadID: "t1", LabelIDs: []string{"INBOX", "UNREAD"}, InternalDate: "1762632000000",
		Payload: Part{MimeType: "multipart/mixed", Headers: []Header{
			{"Authentication-Results", passAR},
			{"From", `"Christina" <Christina@Example.com>`},
			{"To", "Bot <bot@example.com>"},
			{"Cc", "sarah@example.org"},
			{"Subject", "Q4 board deck"},
			{"Date", "Sat, 8 Nov 2026 14:30:00 -0500"},
		}, Parts: []Part{
			{MimeType: "multipart/alternative", Parts: []Part{
				{MimeType: "text/plain", Body: Body{Data: b64("Numbers for slide 6.\r\n")}},
				{MimeType: "text/html", Body: Body{Data: b64("<p>Numbers for <b>slide 6</b>.</p>")}},
			}},
			{MimeType: "application/pdf", Filename: "deck.pdf", Body: Body{AttachmentID: "a1", Size: 1000}},
		}}})
	var g GmailMessage
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	m := g.Mail()
	if m.MessageID != "m1" || m.ThreadID != "t1" || m.From != "christina@example.com" || m.FromName != "Christina" || !m.Verified ||
		m.Subject != "Q4 board deck" || m.Text != "Numbers for slide 6." || strings.Join(m.Attachments, ",") != "deck.pdf" ||
		strings.Join(m.To, ",") != "bot@example.com,sarah@example.org" {
		t.Errorf("mail = %+v", m)
	}
	if g.Received().Unix() != 1762632000 {
		t.Errorf("received = %v", g.Received())
	}
	if g.Ours() || g.Bulk() {
		t.Error("an ordinary inbox message counted as ours or bulk")
	}
}

func TestHTMLOnlyMailIsReadAsText(t *testing.T) {
	g := GmailMessage{Payload: Part{MimeType: "text/html", Headers: []Header{{"From", "x@example.com"}},
		Body: Body{Data: b64("<div>Hi &amp; thanks</div><br><p>Second line</p><style>p{}</style>")}}}
	if got := g.Mail().Text; got != "Hi & thanks\nSecond line" {
		t.Errorf("text = %q", got)
	}
}

func TestOnlyGmailsOwnVerdictVerifiesASender(t *testing.T) {
	for _, c := range []struct {
		name    string
		headers []Header
		ok      bool
		why     string
	}{
		{"dmarc passes for the From domain", []Header{{"Authentication-Results", passAR}, {"From", "christina@example.com"}}, true, "DMARC"},
		{"dmarc passes for another domain", []Header{{"Authentication-Results", strings.Replace(passAR, "header.from=example.com", "header.from=example.net", 1)},
			{"From", "christina@example.com"}}, false, "DMARC"},
		{"a lookalike domain", []Header{{"Authentication-Results", strings.Replace(passAR, "header.from=example.com", "header.from=example.com.evil.example", 1)},
			{"From", "christina@example.com"}}, false, "DMARC"},
		{"dmarc fails", []Header{{"Authentication-Results", strings.Replace(passAR, "dmarc=pass", "dmarc=fail", 1)},
			{"From", "christina@example.com"}}, false, "DMARC"},
		{"a forged verdict below Gmail's", []Header{
			{"Authentication-Results", strings.Replace(passAR, "dmarc=pass", "dmarc=fail", 1)},
			{"Authentication-Results", passAR}, {"From", "christina@example.com"}}, false, "DMARC"},
		{"a verdict not from Gmail", []Header{{"Authentication-Results", strings.Replace(passAR, "mx.google.com", "mx.example.net", 1)},
			{"From", "christina@example.com"}}, false, "not from Gmail"},
		{"no verdict", []Header{{"From", "christina@example.com"}}, false, "no Authentication-Results"},
		{"two From headers", []Header{{"Authentication-Results", passAR}, {"From", "christina@example.com"}, {"From", "x@example.net"}}, false, "From"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ok, why := (GmailMessage{Payload: Part{Headers: c.headers}}).Verify()
			if ok != c.ok || !strings.Contains(why, c.why) {
				t.Errorf("Verify = %v, %q", ok, why)
			}
		})
	}
}

func TestOwnAndBulkMailAreKnown(t *testing.T) {
	if !(GmailMessage{LabelIDs: []string{"SENT"}}).Ours() || !(GmailMessage{LabelIDs: []string{"DRAFT"}}).Ours() {
		t.Error("sent mail and drafts are ours")
	}
	for _, h := range []Header{{"List-Unsubscribe", "<mailto:u@example.com>"}, {"List-Id", "news.example.com"}, {"Precedence", "bulk"}} {
		if !(GmailMessage{Payload: Part{Headers: []Header{h}}}).Bulk() {
			t.Errorf("%s: not bulk", h.Name)
		}
	}
	if !(GmailMessage{LabelIDs: []string{"SPAM"}}).Junk() || !(GmailMessage{LabelIDs: []string{"TRASH"}}).Junk() {
		t.Error("spam and trash are junk")
	}
}

func TestARepliesQuotedHistoryIsCut(t *testing.T) {
	for in, want := range map[string]string{
		"Use Nov 1.\n\nOn Sat, Nov 8, 2026 at 2:30 PM Zach <zach@example.com> wrote:\n> Draft attached": "Use Nov 1.",
		"Yes.\n\nOn Sat, Nov 8, 2026 at 2:30 PM Christina\n<christina@example.com> wrote:\n> old":       "Yes.",
		"Sure\n-----Original Message-----\nFrom: x":                                                     "Sure",
		"Fine by me.\n> quoted\n> more":                                                                 "Fine by me.",
		"On second thought, use Nov 1.":                                                                 "On second thought, use Nov 1.",
		"Keep this.\nOn Monday we meet.\nAnd this.":                                                     "Keep this.\nOn Monday we meet.\nAnd this.",
	} {
		if got := TrimQuoted(in); got != want {
			t.Errorf("TrimQuoted(%q) = %q, want %q", in, got, want)
		}
	}
}
