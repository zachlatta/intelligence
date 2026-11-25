package accounts

import (
	"context"
	"crypto/subtle"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/zachlatta/intelligence/internal/fleet"
)

var (
	ErrNoSuchWorkflow     = errors.New("no such workflow in this account")
	ErrKeyRevoked         = errors.New("API key has been revoked")
	ErrAPITriggerDisabled = errors.New("API trigger is disabled for this workflow")
	ErrWorkflowDeleted    = errors.New("workflow has been deleted")
)

var workflowKeyUUIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// WorkflowAPIKey is a scoped credential for one API-triggered workflow.
// Key is populated only at creation and never stored or returned again.
type WorkflowAPIKey struct {
	ID         string     `json:"id"`
	WorkflowID string     `json:"workflowId"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Key        string     `json:"key,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

// WorkflowAPIKeyAuth is the authenticated identity of a scoped workflow API key.
type WorkflowAPIKeyAuth struct {
	Account           Account
	KeyID             string
	KeyName           string
	WorkflowID        string
	WorkflowName      string
	APITriggerEnabled bool
}

func cleanKeyName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return "", &BadInputError{"API key name must be 1 to 100 characters"}
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", &BadInputError{"API key name cannot contain control characters"}
	}
	return name, nil
}

// CreateWorkflowAPIKey issues a new scoped API key for a's workflowID.
// The raw secret is returned once; only its SHA-256 hash is stored.
func (s *Store) CreateWorkflowAPIKey(ctx context.Context, a Account, workflowID, name string) (WorkflowAPIKey, error) {
	clean, err := cleanKeyName(name)
	if err != nil {
		return WorkflowAPIKey{}, err
	}
	if !workflowKeyUUIDRe.MatchString(workflowID) {
		return WorkflowAPIKey{}, ErrNoSuchWorkflow
	}
	now := s.Now()
	raw := "iwfk_" + randomToken()
	prefix := raw[:12]
	out := WorkflowAPIKey{
		WorkflowID: workflowID,
		Name:       clean,
		Prefix:     prefix,
		Key:        raw,
		CreatedAt:  now,
	}
	err = pgx.BeginFunc(ctx, s.p, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workflows WHERE id = $1 AND account_id = $2 AND deleted_at IS NULL)`,
			workflowID, a.ID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNoSuchWorkflow
		}
		return tx.QueryRow(ctx, `INSERT INTO workflow_api_keys (account_id, workflow_id, name, key_prefix, key_hash, created_at)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id::text`,
			a.ID, workflowID, clean, prefix, sha(raw), now).Scan(&out.ID)
	})
	if err != nil {
		return WorkflowAPIKey{}, err
	}
	return out, nil
}

// RevokeWorkflowAPIKey marks keyID revoked and notifies active streams to disconnect.
// Returns false when no such key exists for a's workflowID.
func (s *Store) RevokeWorkflowAPIKey(ctx context.Context, a Account, workflowID, keyID string) (bool, error) {
	if !workflowKeyUUIDRe.MatchString(workflowID) || !workflowKeyUUIDRe.MatchString(keyID) {
		return false, nil
	}
	var found bool
	err := pgx.BeginFunc(ctx, s.p, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE workflow_api_keys SET revoked_at = coalesce(revoked_at, $4)
			WHERE account_id = $1 AND workflow_id = $2 AND id = $3`, a.ID, workflowID, keyID, s.Now())
		if err != nil {
			return err
		}
		found = tag.RowsAffected() == 1
		if !found {
			return nil
		}
		_, err = tx.Exec(ctx, fleet.NotifySQL, fleet.WorkflowKeyTopic(keyID))
		return err
	})
	return found, err
}

// AuthenticateWorkflowAPIKey validates a scoped workflow API key and records its use.
// It fails with ErrNoSession (unknown/missing), ErrKeyRevoked, or ErrWorkflowDeleted.
// APITriggerEnabled reports whether new runs/retries are currently allowed; reads and
// controls on existing runs remain allowed while the key is active.
func (s *Store) AuthenticateWorkflowAPIKey(ctx context.Context, key string) (WorkflowAPIKeyAuth, error) {
	if key == "" || !strings.HasPrefix(key, "iwfk_") {
		return WorkflowAPIKeyAuth{}, ErrNoSession
	}
	wantHash := sha(key)
	var auth WorkflowAPIKeyAuth
	var storedHash string
	var revoked, deleted bool
	err := s.p.QueryRow(ctx, `SELECT a.id, a.username, a.role_name, k.id::text, k.name, k.key_hash,
		k.revoked_at IS NOT NULL, w.id::text, w.name, w.deleted_at IS NOT NULL,
		coalesce((v.definition->>'apiTrigger')::boolean, false)
		FROM workflow_api_keys k
		JOIN accounts a ON a.id = k.account_id
		JOIN workflows w ON w.id = k.workflow_id
		JOIN workflow_versions v ON v.workflow_id = w.id AND v.version = w.version
		WHERE k.key_hash = $1`, wantHash).
		Scan(&auth.Account.ID, &auth.Account.Username, &auth.Account.Role, &auth.KeyID, &auth.KeyName, &storedHash,
			&revoked, &auth.WorkflowID, &auth.WorkflowName, &deleted, &auth.APITriggerEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowAPIKeyAuth{}, ErrNoSession
	}
	if err != nil {
		return WorkflowAPIKeyAuth{}, err
	}
	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(wantHash)) != 1 {
		return WorkflowAPIKeyAuth{}, ErrNoSession
	}
	if revoked {
		return WorkflowAPIKeyAuth{}, ErrKeyRevoked
	}
	if deleted {
		return WorkflowAPIKeyAuth{}, ErrWorkflowDeleted
	}
	if _, err := s.p.Exec(ctx, `UPDATE workflow_api_keys SET last_used_at = $2 WHERE id = $1`, auth.KeyID, s.Now()); err != nil {
		return WorkflowAPIKeyAuth{}, err
	}
	return auth, nil
}

// CheckWorkflowAPIKeyActive checks whether an already-authenticated key is still allowed
// to stream (not revoked and workflow not deleted). Disabling apiTrigger blocks new runs
// and retries without cutting off streams of existing runs.
func (s *Store) CheckWorkflowAPIKeyActive(ctx context.Context, keyID string) error {
	if !workflowKeyUUIDRe.MatchString(keyID) {
		return ErrNoSession
	}
	var revoked, deleted bool
	err := s.p.QueryRow(ctx, `SELECT k.revoked_at IS NOT NULL, w.deleted_at IS NOT NULL
		FROM workflow_api_keys k
		JOIN workflows w ON w.id = k.workflow_id
		WHERE k.id = $1`, keyID).Scan(&revoked, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoSession
	}
	if err != nil {
		return err
	}
	if revoked {
		return ErrKeyRevoked
	}
	if deleted {
		return ErrWorkflowDeleted
	}
	return nil
}
