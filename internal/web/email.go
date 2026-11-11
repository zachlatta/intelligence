package web

import (
	"context"
	_ "embed"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/email"
	"github.com/zachlatta/intelligence/internal/workflow"
)

// Email triggers. An account connects one Gmail mailbox (Google's sign-in, through the API
// below), and the server reads it (email.Service): each email may start, steer or answer a run of
// a workflow whose trigger is email. What each email did is in the emails table.

//go:embed email.html
var emailHTML string

var emailTmpl = template.Must(template.New("email").Funcs(template.FuncMap{"short": short, "ago": ago}).Parse(emailHTML + layoutHTML))

// baseURL is the server's origin as the browser reaches it: PUBLIC_URL, else the request's.
func (a *app) baseURL(r *http.Request) string {
	if a.cfg.PublicURL != "" {
		return strings.TrimRight(a.cfg.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
		scheme = p
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}

const emailOff = "Email triggers are off on this server: set GOOGLE_OAUTH_CLIENT_JSON (a Google OAuth client) and CONNECTIONS_KEY (openssl rand -base64 32)."

func (a *app) emailOn(w http.ResponseWriter) bool {
	if a.mail == nil {
		http.Error(w, emailOff, http.StatusServiceUnavailable)
		return false
	}
	return true
}

func emailError(w http.ResponseWriter, err error) {
	var ref *email.Refusal
	if errors.As(err, &ref) {
		http.Error(w, ref.Msg, http.StatusBadRequest)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// gmailSignIn: POST /api/gmail/sign-in {}: {"url": Google's consent page, "paste": whether Google
// then sends the browser to a localhost page whose address must be pasted back}.
func (a *app) gmailSignIn(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.emailOn(w) {
			return
		}
		u, paste := a.mail.Start(v.acct.ID, a.baseURL(r))
		writeJSON(w, map[string]any{"url": u, "paste": paste})
	}
}

// gmailConnect: POST /api/gmail/connect {"code", "state"} or {"url"} (where Google sent the
// browser): connects the mailbox, and answers {"address"}.
func (a *app) gmailConnect(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.emailOn(w) {
			return
		}
		var req struct {
			Code  string `json:"code"`
			State string `json:"state"`
			URL   string `json:"url"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.URL != "" {
			u, err := url.Parse(strings.TrimSpace(req.URL))
			if err != nil {
				http.Error(w, "that is not the address Google sent you to", http.StatusBadRequest)
				return
			}
			q := u.Query()
			if e := q.Get("error"); e != "" {
				http.Error(w, "Google said: "+e, http.StatusBadRequest)
				return
			}
			req.Code, req.State = q.Get("code"), q.Get("state")
		}
		if req.Code == "" || req.State == "" {
			http.Error(w, "no code and state: paste the whole address Google sent you to", http.StatusBadRequest)
			return
		}
		addr, err := a.mail.Connect(r.Context(), v.acct.ID, req.Code, req.State, a.baseURL(r))
		if err != nil {
			emailError(w, err)
			return
		}
		writeJSON(w, map[string]string{"address": addr})
	}
}

// gmailOwners: PUT /api/gmail/owners {"owners": [addresses]}: the account holder's own addresses.
func (a *app) gmailOwners(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.emailOn(w) {
			return
		}
		var req struct {
			Owners []string `json:"owners"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if err := a.mail.SetOwners(r.Context(), v.acct.ID, req.Owners); err != nil {
			emailError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// gmailDisconnect: DELETE /api/gmail: forgets the mailbox; its emails stay.
func (a *app) gmailDisconnect(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.emailOn(w) {
			return
		}
		if err := a.mail.Disconnect(r.Context(), v.acct.ID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// gmailCallback: GET /gmail/callback: where Google sends the browser (a web client). The session
// cookie is SameSite=Strict, so it is not sent on Google's redirect: this page posts the code from
// this origin, where it is.
func gmailCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_ = emailTmpl.ExecuteTemplate(w, "callback", nil)
}

// --- reads, through the account's role ---

type mailbox struct {
	Address               string
	Owners                []string
	ConnectedAt           time.Time
	CheckedAt, ErrorAt    *time.Time
	Error                 string
	Backfilled, Connected bool
}

func readMailbox(ctx context.Context, p *pgxpool.Pool) (mailbox, error) {
	var m mailbox
	err := p.QueryRow(ctx, "SELECT address, owners, connected_at, checked_at, error, error_at, backfilled FROM gmail_connections").
		Scan(&m.Address, &m.Owners, &m.ConnectedAt, &m.CheckedAt, &m.Error, &m.ErrorAt, &m.Backfilled)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, nil
	}
	m.Connected = err == nil
	return m, err
}

// emailRow is an email as the pages show it.
type emailRow struct {
	ID, Thread                          string
	ReceivedAt                          time.Time
	FromName, From, Subject, Body       string
	To, Attachments                     []string
	Verified, Bulk                      bool
	Decision, Reason                    string
	Workflow, WorkflowID, Run, RunTitle string
}

// previewEmail is an email as the trigger editor's preview matches it, as workflow.EmailFilter
// does. Replies in a run's thread are left out: they go to that run, whatever the filters say.
type previewEmail struct {
	ID          string    `json:"id"`
	At          time.Time `json:"at"`
	Name        string    `json:"name"`
	From        string    `json:"from"`
	To          []string  `json:"to"`
	Subject     string    `json:"subject"`
	Body        string    `json:"body"`
	Attachments []string  `json:"attachments"`
	Verified    bool      `json:"verified"`
	Bulk        bool      `json:"bulk"`
}

func previews(list []emailRow) []previewEmail {
	out := make([]previewEmail, 0, len(list))
	for _, e := range list {
		if e.Decision == "added" || e.Decision == "answered" || e.Decision == "follow_up" {
			continue
		}
		out = append(out, previewEmail{ID: e.ID, At: e.ReceivedAt, Name: e.FromName, From: e.From, To: e.To, Subject: e.Subject,
			Body: e.Body, Attachments: e.Attachments, Verified: e.Verified, Bulk: e.Bulk})
	}
	return out
}

// readEmails are the newest emails (up to limit), or those of run.
func readEmails(ctx context.Context, p *pgxpool.Pool, run string, limit int) ([]emailRow, error) {
	rows, err := p.Query(ctx, `SELECT e.gmail_id, e.thread_id, e.received_at, e.from_name, e.from_address, e.to_addresses, e.subject,
		left(e.body, 4000), e.attachments, e.verified, e.bulk, e.decision, e.reason, coalesce(w.name, ''), coalesce(e.workflow_id::text, ''),
		coalesce(e.run_id::text, ''), coalesce(nullif(r.task_summary, ''), r.title, '')
		FROM emails e LEFT JOIN workflows w ON w.id = e.workflow_id LEFT JOIN workflow_runs r ON r.id = e.run_id
		WHERE $1 = '' OR e.run_id::text = $1 ORDER BY e.received_at DESC, e.id DESC LIMIT $2`, run, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (emailRow, error) {
		var e emailRow
		err := row.Scan(&e.ID, &e.Thread, &e.ReceivedAt, &e.FromName, &e.From, &e.To, &e.Subject, &e.Body, &e.Attachments, &e.Verified,
			&e.Bulk, &e.Decision, &e.Reason, &e.Workflow, &e.WorkflowID, &e.Run, &e.RunTitle)
		return e, err
	})
	if out == nil {
		out = []emailRow{}
	}
	return out, err
}

// Label is what the email did, in a few words.
func (e emailRow) Label() string {
	return map[string]string{"before": "before connecting", "started": "started", "follow_up": "started again",
		"added": "added to its run", "answered": "answered its run", "ignored": "ignored"}[e.Decision]
}

func (e emailRow) Badge() string {
	return map[string]string{"started": "blue", "follow_up": "blue", "added": "purple", "answered": "purple"}[e.Decision]
}

// Who is the sender as a person reads it.
func (e emailRow) Who() string {
	if e.FromName != "" {
		return e.FromName
	}
	return e.From
}

// emailPage: GET /workflows/email: the mailbox, and what each email did.
func emailPage(cfg Config, a *app, v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mb, err := readMailbox(r.Context(), v.pool)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		emails, err := readEmails(r.Context(), v.pool, "", 200)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := emailTmpl.ExecuteTemplate(w, "page", map[string]any{"Version": cfg.Version, "Now": cfg.Now(), "On": a.mail != nil,
			"Off": emailOff, "Mailbox": mb, "Emails": emails}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// emailSummary is what starts an email workflow, in a line: "Email from a@x, @y · subject has deck".
func emailSummary(f *workflow.EmailFilter) string {
	if f == nil {
		return ""
	}
	var p []string
	if len(f.From) > 0 {
		p = append(p, "from "+strings.Join(f.From, ", "))
	}
	if f.To != "" {
		p = append(p, "to "+f.To)
	}
	if f.Subject != "" {
		p = append(p, "subject has “"+f.Subject+"”")
	}
	if f.Words != "" {
		p = append(p, "has “"+f.Words+"”")
	}
	if f.Without != "" {
		p = append(p, "without “"+f.Without+"”")
	}
	if f.Attachment {
		p = append(p, "with an attachment")
	}
	return "Email " + strings.Join(p, ", ")
}
