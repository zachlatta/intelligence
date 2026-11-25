package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zachlatta/intelligence/internal/attachment"
)

// APIAttachment is an inline file attached to an API-triggered run, reply or input.
type APIAttachment struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// APIRunRequest is what an API-key caller sends to trigger a workflow run.
type APIRunRequest struct {
	Input          string          `json:"input"`
	Attachments    []APIAttachment `json:"attachments,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
}

// APIRunResult distinguishes a newly started run from an idempotent replay (Replayed=true).
type APIRunResult struct {
	RunID    string
	Replayed bool
}

const maxAPIAttachments = 10

func cleanIdempotencyKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", nil
	}
	if len(key) > 255 {
		return "", bad("idempotency key must be at most 255 characters")
	}
	if strings.IndexFunc(key, func(r rune) bool { return unicode.IsControl(r) || r > 127 }) >= 0 {
		return "", bad("idempotency key must contain printable ASCII characters only")
	}
	return key, nil
}

func apiRequestHash(input string, atts []APIAttachment) string {
	h := sha256.New()
	_, _ = h.Write([]byte(input))
	_, _ = h.Write([]byte{0})
	for _, a := range atts {
		_, _ = h.Write([]byte(strings.TrimSpace(a.Name)))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(a.Data)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// AppendAttachments validates and stores inline attachments in tx, appending [attachment:<id>]
// references to text in the same format the web UI uses.
func AppendAttachments(ctx context.Context, tx pgx.Tx, account int64, text string, atts []APIAttachment) (string, error) {
	if len(atts) == 0 {
		return text, nil
	}
	if len(atts) > maxAPIAttachments {
		return "", bad("at most %d attachments per request", maxAPIAttachments)
	}
	refs := make([]string, 0, len(atts))
	for _, a := range atts {
		name := strings.TrimSpace(a.Name)
		if name == "" || len(name) > 255 {
			return "", bad("attachment name must be 1–255 bytes")
		}
		media, err := attachment.ValidateFile(name, a.Data)
		if err != nil {
			return "", bad("%s", err)
		}
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO attachments(account_id,name,media_type,data) VALUES($1,$2,$3,$4) RETURNING id::text`,
			account, name, media, a.Data).Scan(&id); err != nil {
			return "", err
		}
		refs = append(refs, "[attachment:"+id+"]")
	}
	trimmed := strings.TrimRight(text, "\n")
	if trimmed == "" {
		return strings.Join(refs, "\n"), nil
	}
	return trimmed + "\n\n" + strings.Join(refs, "\n"), nil
}

// StartAPIRun triggers a run of workflowID using an authenticated API key. It enforces
// that the workflow's trigger is "api", stores optional attachments in the same transaction,
// records api_key_id, and supports idempotent retries via idempotencyKey.
func (e *Engine) StartAPIRun(ctx context.Context, account int64, workflowID, keyID string, req APIRunRequest) (APIRunResult, error) {
	idemKey, err := cleanIdempotencyKey(req.IdempotencyKey)
	if err != nil {
		return APIRunResult{}, err
	}
	if strings.TrimSpace(req.Input) == "" {
		return APIRunResult{}, bad("a run needs input: what should be done")
	}
	reqHash := ""
	if idemKey != "" {
		reqHash = apiRequestHash(req.Input, req.Attachments)
	}
	ctx, flushPushes := e.CollectPushes(ctx)
	var out APIRunResult
	err = pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		var trigger string
		var def []byte
		err := tx.QueryRow(ctx, `SELECT coalesce(v.definition->>'trigger', ''), v.definition
			FROM workflows w JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version
			WHERE w.id = $1 AND w.account_id = $2 AND w.deleted_at IS NULL FOR SHARE OF w`,
			workflowID, account).Scan(&trigger, &def)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if trigger != TriggerAPI {
			return conflict("API trigger is disabled for this workflow")
		}
		if idemKey != "" {
			var existingID, existingHash string
			err := tx.QueryRow(ctx, `SELECT id::text, idempotency_hash FROM workflow_runs
				WHERE workflow_id = $1 AND idempotency_key = $2`, workflowID, idemKey).Scan(&existingID, &existingHash)
			if err == nil {
				if existingHash != reqHash {
					return conflict("idempotency key already used with different request parameters")
				}
				out = APIRunResult{RunID: existingID, Replayed: true}
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		fullInput, err := AppendAttachments(ctx, tx, account, req.Input, req.Attachments)
		if err != nil {
			return err
		}
		runID, err := e.StartRunTx(ctx, tx, account, workflowID, fullInput, time.Time{})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET api_key_id = $2, idempotency_key = $3, idempotency_hash = $4 WHERE id = $1`,
			runID, keyID, idemKey, reqHash); err != nil {
			return err
		}
		out = APIRunResult{RunID: runID, Replayed: false}
		return nil
	})
	if err != nil {
		var pg *pgconn.PgError
		if idemKey != "" && errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "workflow_runs_api_idempotency" {
			// Concurrent duplicate POST won the unique index race: look up the committed run.
			var existingID, existingHash string
			qerr := e.p.QueryRow(ctx, `SELECT id::text, idempotency_hash FROM workflow_runs
				WHERE workflow_id = $1 AND idempotency_key = $2`, workflowID, idemKey).Scan(&existingID, &existingHash)
			if qerr != nil {
				return APIRunResult{}, qerr
			}
			if existingHash != reqHash {
				return APIRunResult{}, conflict("idempotency key already used with different request parameters")
			}
			return APIRunResult{RunID: existingID, Replayed: true}, nil
		}
		return APIRunResult{}, err
	}
	if !out.Replayed {
		e.sendFailureAlertsBestEffort(ctx)
		flushPushes()
	}
	return out, nil
}

// StoreAttachments stores inline attachments in a transaction and returns text with references appended.
func (e *Engine) StoreAttachments(ctx context.Context, account int64, text string, atts []APIAttachment) (string, error) {
	if len(atts) == 0 {
		return text, nil
	}
	var out string
	err := pgx.BeginFunc(ctx, e.p, func(tx pgx.Tx) error {
		var err error
		out, err = AppendAttachments(ctx, tx, account, text, atts)
		return err
	})
	return out, err
}

// Ensure json import stays used if needed later.
var _ = json.Valid
