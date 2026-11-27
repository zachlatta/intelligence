package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/agenttext"
)

// Temporary secrets: Zach gives a waiting run a password or API key in a password field on its
// ask; Intelligence keeps it only as AES-256-GCM ciphertext (INTELLIGENCE_SECRETS_KEY) in
// workflow_run_secrets, bound to its row, and the run's agents fetch it with their host's key.
// The ciphertext is wiped SecretTTL after it was stored, when Zach deletes it, or when the run
// ends (a trigger on workflow_runs), whichever comes first.

// SecretTTL is the longest a stored secret lives; its run ending wipes it sooner.
const SecretTTL = 7 * 24 * time.Hour

// ErrSecretsNotConfigured: the server has no INTELLIGENCE_SECRETS_KEY.
var ErrSecretsNotConfigured = errors.New("secret storage is not configured on this server: set INTELLIGENCE_SECRETS_KEY")

// ErrSecretUnreadable: a stored ciphertext did not open (the key changed or the row was altered).
var ErrSecretUnreadable = errors.New("the stored secret cannot be opened: INTELLIGENCE_SECRETS_KEY changed? Ask Zach for it again")

const (
	maxSecretsPerAsk   = 10
	maxSecretNameRunes = 120
	maxSecretPurpose   = 500
	maxSecretUsername  = 120
	maxSecretValue     = 64 << 10
	// A value shorter than this (a PIN) is not looked for in the reply text or the resumed
	// message: it would match ordinary words and digits.
	minLeakCheckLen = 6
)

// SecretRequest is one secret an orchestrator asks Zach for on an `ask` move.
type SecretRequest struct {
	Name      string `json:"name"`
	Purpose   string `json:"purpose,omitempty"`
	Username  string `json:"username,omitempty"`
	Multiline bool   `json:"multiline,omitempty"` // a PEM key or similar: a text area, not a one-line field
}

// SecretSubmission is one secret value Zach submits from the browser.
type SecretSubmission struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

func validateSecretRequests(move string, reqs []SecretRequest) ([]SecretRequest, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	if move != "ask" {
		return nil, bad("secrets can only be requested on an ask move")
	}
	if len(reqs) > maxSecretsPerAsk {
		return nil, bad("an ask can request at most %d secrets", maxSecretsPerAsk)
	}
	seen := make(map[string]bool, len(reqs))
	out := make([]SecretRequest, len(reqs))
	for i, r := range reqs {
		name := strings.TrimSpace(r.Name)
		purpose := strings.TrimSpace(r.Purpose)
		username := strings.TrimSpace(r.Username)
		if name == "" || utf8.RuneCountInString(name) > maxSecretNameRunes || hasDisallowedLineRune(name) {
			return nil, bad("each requested secret needs a single-line name of 1–%d characters", maxSecretNameRunes)
		}
		if uuidRe.MatchString(name) {
			return nil, bad("secret name %q looks like an id; give it a descriptive name", name)
		}
		if utf8.RuneCountInString(purpose) > maxSecretPurpose || strings.ContainsRune(purpose, 0) {
			return nil, bad("secret %q purpose must be at most %d characters with no NUL bytes", name, maxSecretPurpose)
		}
		if utf8.RuneCountInString(username) > maxSecretUsername || hasDisallowedLineRune(username) {
			return nil, bad("secret %q username must be a single line of at most %d characters", name, maxSecretUsername)
		}
		for _, field := range []string{name, purpose, username} {
			if err := agenttext.Validate(field); err != nil {
				return nil, bad("%s", err)
			}
		}
		key := strings.ToLower(name)
		if seen[key] {
			return nil, bad("duplicate requested secret name %q", name)
		}
		seen[key] = true
		out[i] = SecretRequest{Name: name, Purpose: purpose, Username: username, Multiline: r.Multiline}
	}
	return out, nil
}

func hasDisallowedLineRune(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return r == 0 || unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || unicode.Is(unicode.Cf, r)
	})
}

// secretAAD binds a ciphertext to its row: copied to another secret, run or account, it does not open.
func secretAAD(id, runID string, account int64) string {
	return "intelligence/workflow_run_secrets/v1\x00" + id + "\x00" + runID + "\x00" + strconv.FormatInt(account, 10)
}

func (e *Engine) insertSecretRequests(ctx context.Context, tx pgx.Tx, r run, askEventID int64, reqs []SecretRequest) error {
	now := e.Now()
	for i, req := range reqs {
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_run_secrets
			(run_id, account_id, ask_event_id, position, name, purpose, username, multiline, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			r.ID, r.Account, askEventID, i, req.Name, req.Purpose, req.Username, req.Multiline, now); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) dismissPendingSecrets(ctx context.Context, tx pgx.Tx, runID string) error {
	_, err := tx.Exec(ctx, `UPDATE workflow_run_secrets SET status = 'dismissed' WHERE run_id = $1 AND status = 'pending'`, runID)
	return err
}

type pendingSecretRow struct {
	ID       string
	Name     string
	Username string
}

// SubmitSecretsFromBrowser seals the values Zach gave for the ask the run waits on and resumes it,
// all in one transaction: either every value is stored and the orchestrator told how to fetch
// them, or nothing is. Values never enter workflow_run_events, commands, logs or errors.
func (e *Engine) SubmitSecretsFromBrowser(ctx context.Context, account int64, actorUsername, runID string, submissions []SecretSubmission, note string, approve bool) error {
	if !uuidRe.MatchString(runID) {
		return ErrNotFound
	}
	if e.SecretBox == nil {
		return ErrSecretsNotConfigured
	}
	if len(submissions) == 0 {
		return bad("at least one secret value is required")
	}
	if strings.ContainsRune(note, 0) {
		return bad("a reply cannot contain a NUL character")
	}
	if err := agenttext.Validate(note); err != nil {
		return bad("%s", err)
	}
	byID := make(map[string]string, len(submissions))
	for _, s := range submissions {
		if !uuidRe.MatchString(s.ID) {
			return bad("invalid secret request id")
		}
		if _, dup := byID[s.ID]; dup {
			return bad("duplicate secret request id")
		}
		if s.Value == "" {
			return bad("every requested secret must have a non-empty value")
		}
		if len(s.Value) > maxSecretValue {
			return bad("a secret value can be at most %d KiB", maxSecretValue>>10)
		}
		if strings.ContainsRune(s.Value, 0) || !utf8.ValidString(s.Value) {
			return bad("a secret value must be UTF-8 text with no NUL character")
		}
		if len(s.Value) >= minLeakCheckLen && strings.Contains(note, s.Value) {
			return bad("do not paste a secret value into the text reply box")
		}
		byID[s.ID] = s.Value
	}
	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		r, err := e.lockRun(ctx, tx, account, runID)
		if err != nil {
			return err
		}
		if r.Status != Waiting {
			return conflict("this run is not waiting for a reply (%s)", r.Status)
		}
		onSignoff := r.Step >= 0 && r.Step < len(r.Def.Steps) && r.Def.Steps[r.Step].Kind == KindSignoff
		if approve && !onSignoff {
			return bad("approve is only valid on a sign-off step")
		}
		pending, err := loadPendingSecrets(ctx, tx, runID)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return conflict("this ask has no pending secret requests")
		}
		if len(pending) != len(byID) {
			return bad("fill in all %d requested secret(s), or leave them blank to send a plain reply", len(pending))
		}
		for _, p := range pending {
			if _, ok := byID[p.ID]; !ok {
				return bad("submitted secrets do not match the pending secret requests for this ask")
			}
		}
		now := e.Now()
		expires := now.Add(SecretTTL)
		names := make([]string, 0, len(pending))
		var lines []string
		for _, p := range pending {
			sealed, err := e.SecretBox.Seal([]byte(byID[p.ID]), secretAAD(p.ID, r.ID, r.Account))
			if err != nil {
				return errors.New("could not seal the secret")
			}
			if _, err := tx.Exec(ctx, `UPDATE workflow_run_secrets
				SET status = 'stored', ciphertext = $2, stored_by = $3, stored_at = $4, expires_at = $5
				WHERE id = $1 AND run_id = $6 AND status = 'pending'`,
				p.ID, sealed, actorUsername, now, expires, r.ID); err != nil {
				return err
			}
			if err := e.event(ctx, tx, r, "secret_stored", ptr(r.Step), nil,
				fmt.Sprintf("Stored secret %q (encrypted; expires %s or when the run ends)", p.Name, expires.UTC().Format(time.RFC3339))); err != nil {
				return err
			}
			names = append(names, strconv.Quote(p.Name))
			lines = append(lines, secretFetchLine(r.ID, p))
		}
		noteTrimmed := strings.TrimSpace(note)
		summary := fmt.Sprintf("Stored %d secret(s) in Intelligence: %s", len(pending), strings.Join(names, ", "))
		if noteTrimmed != "" {
			summary += "\n\n" + noteTrimmed
		}
		var msg strings.Builder
		fmt.Fprintf(&msg, "Zach stored %d secret(s) for this run in Intelligence. They are encrypted, readable only by this run's agents on %s, and wiped at %s or when the run ends, whichever is first.\n\n%s\n\n"+
			"Fetch a value only into the command or file that needs it: `intelligence secret env` sets it in that command's environment, and `intelligence secret get --to-file <path>` writes a 0600 file. "+
			"`intelligence secret get` without --to-file prints to a pipe only, never to a terminal. "+
			"Never print, echo, log, commit or paste a secret value into chat, notes, events, files in a repository or transcripts; pass it on to workers by these commands, not by value. Each fetch is recorded on the run.",
			len(pending), r.Host, expires.UTC().Format(time.RFC3339), strings.Join(lines, "\n"))
		if noteTrimmed != "" {
			fmt.Fprintf(&msg, "\n\nZach's note:\n\n%s", noteTrimmed)
		}
		for _, val := range byID {
			if len(val) >= minLeakCheckLen && (strings.Contains(summary, val) || strings.Contains(msg.String(), val)) {
				return bad("a secret value appears in its name or purpose; not stored")
			}
		}
		return e.replyTxRecording(ctx, tx, account, runID, msg.String(), summary, true, approve)
	})
}

// secretFetchLine is how the orchestrator fetches one secret, by name, without seeing it.
func secretFetchLine(runID string, p pendingSecretRow) string {
	line := fmt.Sprintf("- %q: `intelligence secret env --run %s %s=%s -- <command>` or `intelligence secret get --run %s --to-file <path> %s`",
		p.Name, runID, secretEnvName(p.Name), shellWord(p.Name), runID, shellWord(p.Name))
	if p.Username != "" {
		line += fmt.Sprintf(" (username: %s)", p.Username)
	}
	return line
}

// secretEnvName suggests an environment variable for a secret's name: "Stripe API key" is STRIPE_API_KEY.
func secretEnvName(name string) string {
	var b strings.Builder
	under := false
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			under = false
		} else if !under && b.Len() > 0 {
			b.WriteByte('_')
			under = true
		}
	}
	s := strings.TrimRight(b.String(), "_")
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		s = "SECRET_" + s
	}
	return strings.TrimRight(s, "_")
}

// shellWord is s as one shell word, quoted only when it needs to be.
func shellWord(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:@", r))
	}) < 0 {
		return s
	}
	return shellQuote(s)
}

func loadPendingSecrets(ctx context.Context, tx pgx.Tx, runID string) ([]pendingSecretRow, error) {
	rows, err := tx.Query(ctx, `SELECT id::text, name, username
		FROM workflow_run_secrets
		WHERE run_id = $1 AND status = 'pending'
		  AND ask_event_id = (SELECT coalesce(max(id), 0) FROM workflow_run_events WHERE run_id = $1 AND kind = 'ask')
		ORDER BY position, id
		FOR UPDATE`, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (pendingSecretRow, error) {
		var p pendingSecretRow
		err := row.Scan(&p.ID, &p.Name, &p.Username)
		return p, err
	})
}

// FetchedSecret is a stored secret's value, opened for one of its run's agents.
type FetchedSecret struct {
	ID    string
	Name  string
	Value string
}

// FetchSecret opens the secret named (or with the id) ref on runID for an agent on host, whose
// key belongs to account: the run must be that account's and on that host, live, and the secret
// stored and unexpired. Each fetch is recorded (who, when, which), never the value.
func (e *Engine) FetchSecret(ctx context.Context, account int64, host, runID, ref string) (FetchedSecret, error) {
	ref = strings.TrimSpace(ref)
	if !uuidRe.MatchString(runID) {
		return FetchedSecret{}, ErrNotFound
	}
	if e.SecretBox == nil {
		return FetchedSecret{}, ErrSecretsNotConfigured
	}
	if ref == "" {
		return FetchedSecret{}, bad("name the secret to fetch")
	}
	var out FetchedSecret
	var refusal error
	err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		var r run
		var status string
		if err := tx.QueryRow(ctx, `SELECT id::text, account_id, host, status FROM workflow_runs WHERE id = $1`, runID).
			Scan(&r.ID, &r.Account, &r.Host, &status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if r.Account != account || r.Host != host {
			return ErrNotFound
		}
		switch status {
		case Succeeded, Failed, Canceled:
			refusal = conflict("this run has ended (%s); its secrets were wiped", status)
			return nil
		}
		var (
			sealed  []byte
			sStatus string
			reason  string
			expires *time.Time
		)
		err := tx.QueryRow(ctx, `SELECT id::text, name, status, ciphertext, expires_at, purge_reason FROM workflow_run_secrets
			WHERE run_id = $1 AND (id::text = $2 OR lower(name) = lower($2))
			ORDER BY (status = 'stored') DESC, stored_at DESC NULLS LAST, created_at DESC LIMIT 1 FOR UPDATE`, runID, ref).
			Scan(&out.ID, &out.Name, &sStatus, &sealed, &expires, &reason)
		if errors.Is(err, pgx.ErrNoRows) {
			refusal = &Refusal{Msg: fmt.Sprintf("this run has no secret %q: ask Zach for it with `intelligence workflow ask --secret`", ref)}
			return nil
		}
		if err != nil {
			return err
		}
		switch sStatus {
		case "pending":
			refusal = conflict("Zach has not given %q yet", out.Name)
			return nil
		case "dismissed":
			refusal = conflict("Zach replied without giving %q; ask again if it is still needed", out.Name)
			return nil
		case "purged":
			refusal = conflict("%q was wiped (%s); ask Zach again if it is still needed", out.Name, strings.ReplaceAll(reason, "_", " "))
			return nil
		}
		now := e.Now()
		if expires == nil || !now.Before(*expires) {
			if _, err := tx.Exec(ctx, `UPDATE workflow_run_secrets SET status = 'purged', ciphertext = NULL, purged_at = $2, purge_reason = 'expired' WHERE id = $1`, out.ID, now); err != nil {
				return err
			}
			refusal = conflict("%q expired and was wiped; ask Zach again if it is still needed", out.Name)
			return nil
		}
		plain, err := e.SecretBox.Open(sealed, secretAAD(out.ID, r.ID, r.Account))
		if err != nil {
			return ErrSecretUnreadable
		}
		out.Value = string(plain)
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_run_secret_fetches (secret_id, run_id, account_id, host, at) VALUES ($1, $2, $3, $4, $5)`,
			out.ID, r.ID, r.Account, host, now); err != nil {
			return err
		}
		return e.event(ctx, tx, r, "secret_fetched", nil, nil, fmt.Sprintf("An agent on %s fetched secret %q", host, out.Name))
	})
	if err != nil {
		return FetchedSecret{}, err
	}
	if refusal != nil {
		return FetchedSecret{}, refusal
	}
	return out, nil
}

// DeleteSecret wipes a stored secret early, for Zach from the run's page.
func (e *Engine) DeleteSecret(ctx context.Context, account int64, actorUsername, runID, secretID string) error {
	if !uuidRe.MatchString(runID) || !uuidRe.MatchString(secretID) {
		return ErrNotFound
	}
	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		r, err := e.lockRun(ctx, tx, account, runID)
		if err != nil {
			return err
		}
		var name, status string
		if err := tx.QueryRow(ctx, `SELECT name, status FROM workflow_run_secrets WHERE id = $1 AND run_id = $2 FOR UPDATE`, secretID, r.ID).Scan(&name, &status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if status != "stored" {
			return conflict("%q holds no value to delete (%s)", name, status)
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow_run_secrets SET status = 'purged', ciphertext = NULL, purged_at = $2, purge_reason = 'deleted' WHERE id = $1`, secretID, e.Now()); err != nil {
			return err
		}
		return e.event(ctx, tx, r, "secret_deleted", nil, nil, fmt.Sprintf("%s deleted secret %q", actorUsername, name))
	})
}

// PurgeExpiredSecrets wipes every stored secret past its expiry, recording it on each run.
func (e *Engine) PurgeExpiredSecrets(ctx context.Context) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE workflow_run_secrets SET status = 'purged', ciphertext = NULL, purged_at = $1, purge_reason = 'expired'
			WHERE status = 'stored' AND expires_at <= $1 RETURNING run_id::text, account_id, name`, e.Now())
		if err != nil {
			return err
		}
		type purged struct {
			r    run
			name string
		}
		list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (purged, error) {
			var p purged
			err := row.Scan(&p.r.ID, &p.r.Account, &p.name)
			return p, err
		})
		if err != nil {
			return err
		}
		for _, p := range list {
			if err := e.event(ctx, tx, p.r, "secret_expired", nil, nil, fmt.Sprintf("Secret %q expired; its value was wiped", p.name)); err != nil {
				return err
			}
		}
		n = len(list)
		return nil
	})
	return n, err
}

// RunSecretPurge wipes expired secrets every minute until ctx ends.
func (e *Engine) RunSecretPurge(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := e.PurgeExpiredSecrets(bounded)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("workflow secret purge: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func parseSecretFlag(raw string) (SecretRequest, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return SecretRequest{}, errors.New("--secret needs a name (e.g. \"Stripe API key: Read invoices\") or JSON object")
	}
	if strings.HasPrefix(s, "{") {
		var req SecretRequest
		dec := json.NewDecoder(strings.NewReader(s))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			return SecretRequest{}, fmt.Errorf("invalid --secret JSON: %w", err)
		}
		return req, nil
	}
	name, purpose, _ := strings.Cut(s, ":")
	return SecretRequest{
		Name:    strings.TrimSpace(name),
		Purpose: strings.TrimSpace(purpose),
	}, nil
}
