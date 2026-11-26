package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/agenttext"
	"github.com/zachlatta/intelligence/internal/vaultwarden"
)

// SecretStore writes submitted secrets to Vaultwarden without exposing values in Postgres.
type SecretStore interface {
	StoreSecret(ctx context.Context, req vaultwarden.StoreRequest) (vaultwarden.StoredItem, error)
	TargetCollection() string
}

// ErrSecretsNotConfigured is returned when a user tries to submit a secret on a server
// without Vaultwarden configured.
var ErrSecretsNotConfigured = errors.New("Vaultwarden is not configured on this server; set VAULTWARDEN_URL, VAULTWARDEN_CLIENT_ID, VAULTWARDEN_CLIENT_SECRET, VAULTWARDEN_MASTER_PASSWORD and VAULTWARDEN_ACCOUNT")

// ErrSecretStoreFailed wraps an upstream Vaultwarden write failure (never containing secret values).
var ErrSecretStoreFailed = errors.New("could not store secret in Vaultwarden")

const (
	maxSecretsPerAsk   = 10
	maxSecretNameRunes = 120
	maxSecretPurpose   = 500
	maxSecretUsername  = 120
)

// SecretRequest is one secret an orchestrator asks Zach for on an `ask` move.
type SecretRequest struct {
	Name     string `json:"name"`
	Purpose  string `json:"purpose,omitempty"`
	Username string `json:"username,omitempty"`
}

// SecretSubmission is one secret value submitted by Zach from the browser.
// Values are sent directly to Vaultwarden and never stored in Postgres, events, or transcripts.
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
		out[i] = SecretRequest{Name: name, Purpose: purpose, Username: username}
	}
	return out, nil
}

func hasDisallowedLineRune(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return r == 0 || unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || unicode.Is(unicode.Cf, r)
	})
}

func (e *Engine) targetSecretCollection() string {
	if e.Secrets != nil && strings.TrimSpace(e.Secrets.TargetCollection()) != "" {
		return strings.TrimSpace(e.Secrets.TargetCollection())
	}
	return vaultwarden.DefaultCollection
}

func (e *Engine) insertSecretRequests(ctx context.Context, tx pgx.Tx, r run, askEventID int64, reqs []SecretRequest) error {
	if len(reqs) == 0 {
		return nil
	}
	col := e.targetSecretCollection()
	now := e.Now()
	for i, req := range reqs {
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_run_secret_requests
			(run_id, account_id, ask_event_id, position, name, purpose, username, target_collection, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			r.ID, r.Account, askEventID, i, req.Name, req.Purpose, req.Username, col, now); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) dismissPendingSecrets(ctx context.Context, tx pgx.Tx, runID string) error {
	_, err := tx.Exec(ctx, `UPDATE workflow_run_secret_requests SET status = 'dismissed' WHERE run_id = $1 AND status = 'pending'`, runID)
	return err
}

type pendingSecretRow struct {
	ID         string
	AskEventID int64
	Name       string
	Purpose    string
	Username   string
}

func (e *Engine) secretsAllowedForAccount(ctx context.Context, tx pgx.Tx, accountID int64) (bool, error) {
	if e.Secrets == nil || strings.TrimSpace(e.SecretsAccount) == "" {
		return false, nil
	}
	var username string
	if err := tx.QueryRow(ctx, `SELECT username FROM accounts WHERE id = $1`, accountID).Scan(&username); err != nil {
		return false, err
	}
	return username == strings.TrimSpace(e.SecretsAccount), nil
}

// SubmitSecretsFromBrowser stores submitted secret values in Vaultwarden for the configured
// account and resumes the waiting run with only non-secret item references. Secret values never
// enter Postgres, workflow_run_events, commands, logs, or error messages.
func (e *Engine) SubmitSecretsFromBrowser(ctx context.Context, account int64, actorUsername, runID string, submissions []SecretSubmission, note string, approve bool) error {
	if !uuidRe.MatchString(runID) {
		return ErrNotFound
	}
	if e.Secrets == nil {
		return ErrSecretsNotConfigured
	}
	if strings.TrimSpace(e.SecretsAccount) == "" || actorUsername != strings.TrimSpace(e.SecretsAccount) {
		return forbidden("Vaultwarden secret storage is not enabled for this account")
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
		if strings.ContainsRune(s.Value, 0) {
			return bad("a secret value cannot contain a NUL character")
		}
		if note != "" && strings.Contains(note, s.Value) {
			return bad("do not paste a secret value into the text reply box")
		}
		byID[s.ID] = s.Value
	}

	// Phase 1: verify run state and load pending secret requests before writing to Vaultwarden.
	var pending []pendingSecretRow
	err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
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
		pending, err = loadPendingSecrets(ctx, tx, runID)
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
		return nil
	})
	if err != nil {
		return err
	}

	// Phase 2: write each secret to Vaultwarden outside the Postgres transaction.
	type storedSecret struct {
		req  pendingSecretRow
		item vaultwarden.StoredItem
	}
	stored := make([]storedSecret, 0, len(pending))
	for _, p := range pending {
		metaNotes := fmt.Sprintf("Stored via Intelligence for workflow run %s.", runID)
		if p.Purpose != "" {
			metaNotes += " Purpose: " + p.Purpose
		}
		item, err := e.Secrets.StoreSecret(ctx, vaultwarden.StoreRequest{
			Name:     p.Name,
			Username: p.Username,
			Password: byID[p.ID],
			Notes:    metaNotes,
		})
		if err != nil {
			return fmt.Errorf("%w: %s", ErrSecretStoreFailed, sanitizeSecretStoreError(err, byID))
		}
		stored = append(stored, storedSecret{req: p, item: item})
	}

	// Phase 3: record non-secret references in Postgres and resume the orchestrator.
	noteTrimmed := strings.TrimSpace(note)
	var eventParts []string
	var msgLines []string
	collection := e.targetSecretCollection()
	for _, s := range stored {
		if s.item.Collection != "" {
			collection = s.item.Collection
		}
		eventParts = append(eventParts, fmt.Sprintf("%s (item %s)", s.item.Name, s.item.ID))
		line := fmt.Sprintf("- %s (item ID: %s): fetch with `vaultwarden-agent get password %s` (or `vaultwarden-agent get item %s`)",
			s.item.Name, s.item.ID, s.item.ID, s.item.ID)
		if s.req.Username != "" {
			line += fmt.Sprintf(" (username: %s)", s.req.Username)
		}
		msgLines = append(msgLines, line)
	}
	eventSummary := fmt.Sprintf("Stored %d secret(s) in Vaultwarden (%s): %s", len(stored), collection, strings.Join(eventParts, ", "))
	if noteTrimmed != "" {
		eventSummary += "\n\n" + noteTrimmed
	}

	var msgBody strings.Builder
	fmt.Fprintf(&msgBody, "The requested secret(s) were stored in Vaultwarden (%s):\n%s\n\nNever print, log, or paste secret values into chat, events, or transcripts.",
		collection, strings.Join(msgLines, "\n"))
	if noteTrimmed != "" {
		fmt.Fprintf(&msgBody, "\n\nAdditional note:\n\n%s", noteTrimmed)
	}

	// Defense-in-depth: guarantee no submitted secret value ever appears in eventSummary or msgBody.
	for _, val := range byID {
		if strings.Contains(eventSummary, val) || strings.Contains(msgBody.String(), val) {
			return errors.New("internal guard: refused to record message containing secret value")
		}
	}

	return pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		r, err := e.lockRun(ctx, tx, account, runID)
		if err != nil {
			return err
		}
		if r.Status != Waiting {
			return conflict("this run is no longer waiting for a reply (%s)", r.Status)
		}
		now := e.Now()
		for _, s := range stored {
			col := s.item.Collection
			if col == "" {
				col = collection
			}
			if _, err := tx.Exec(ctx, `UPDATE workflow_run_secret_requests
				SET status = 'stored', vault_item_id = $2, vault_item_name = $3, target_collection = $4, stored_by = $5, stored_at = $6
				WHERE id = $1 AND run_id = $7 AND status = 'pending'`,
				s.req.ID, s.item.ID, s.item.Name, col, actorUsername, now, r.ID); err != nil {
				return err
			}
			evText := fmt.Sprintf("Stored secret %q in Vaultwarden (%s, item %s)", s.item.Name, col, s.item.ID)
			if err := e.event(ctx, tx, r, "secret_stored", ptr(r.Step), nil, evText); err != nil {
				return err
			}
		}
		return e.replyTxRecording(ctx, tx, account, runID, msgBody.String(), eventSummary, true, approve)
	})
}

func loadPendingSecrets(ctx context.Context, tx pgx.Tx, runID string) ([]pendingSecretRow, error) {
	rows, err := tx.Query(ctx, `SELECT id::text, ask_event_id, name, purpose, username
		FROM workflow_run_secret_requests
		WHERE run_id = $1 AND status = 'pending'
		  AND ask_event_id = (SELECT coalesce(max(id), 0) FROM workflow_run_events WHERE run_id = $1 AND kind = 'ask')
		ORDER BY position, id`, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (pendingSecretRow, error) {
		var p pendingSecretRow
		err := row.Scan(&p.ID, &p.AskEventID, &p.Name, &p.Purpose, &p.Username)
		return p, err
	})
}

func sanitizeSecretStoreError(err error, values map[string]string) string {
	msg := err.Error()
	for _, v := range values {
		if v != "" && strings.Contains(msg, v) {
			return "upstream Vaultwarden error"
		}
	}
	return msg
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
