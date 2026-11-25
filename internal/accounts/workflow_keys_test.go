package accounts_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/intelligence/internal/accounts"
	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/dbtest"
)

func TestWorkflowAPIKeysLifecycle(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	roles := db.NewAccountRoles(d.Owner, d.Readonly)
	defer roles.Close()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	store := accounts.New(d.Owner, roles)
	store.Now = func() time.Time { return now }

	alice, err := store.Create(ctx, "alice", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.Create(ctx, "bob", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}

	var wfID string
	if err := d.Owner.QueryRow(ctx, `INSERT INTO workflows (account_id, name, description, version, created_at, updated_at)
		VALUES ($1, 'Agreement review', '', 1, $2, $2) RETURNING id::text`, alice.ID, now).Scan(&wfID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Owner.Exec(ctx, `INSERT INTO workflow_versions (workflow_id, version, account_id, definition, created_at)
		VALUES ($1, 1, $2, '{"trigger":"api","host":"box-1","project":{"mode":"fixed","directory":"/w/app"},"orchestrator":{"provider":"claude","instructions":"run"},"steps":[{"id":"s1","kind":"step","title":"S1","instructions":"do"}]}'::jsonb, $3)`, wfID, alice.ID, now); err != nil {
		t.Fatal(err)
	}

	// Bad name refused.
	if _, err := store.CreateWorkflowAPIKey(ctx, alice, wfID, "   "); err == nil {
		t.Fatal("expected error for blank name")
	}

	// Another account cannot issue a key for alice's workflow.
	if _, err := store.CreateWorkflowAPIKey(ctx, bob, wfID, "stolen"); !errors.Is(err, accounts.ErrNoSuchWorkflow) {
		t.Fatalf("bob create key: got %v, want ErrNoSuchWorkflow", err)
	}

	// Issue a key.
	k, err := store.CreateWorkflowAPIKey(ctx, alice, wfID, "ci-prod")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Key, "iwfk_") || len(k.Key) < 48 {
		t.Fatalf("unexpected key format: %q", k.Key)
	}
	if k.Prefix != k.Key[:12] || k.Name != "ci-prod" || k.WorkflowID != wfID {
		t.Fatalf("unexpected key metadata: %+v", k)
	}

	// Raw key is never stored; hash is stored; RLS isolates accounts.
	var storedHash string
	if err := d.Owner.QueryRow(ctx, `SELECT key_hash FROM workflow_api_keys WHERE id = $1`, k.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash == k.Key || len(storedHash) != 64 {
		t.Fatalf("expected 64-hex sha256 hash, got %q", storedHash)
	}
	bobPool, err := store.Reader(ctx, bob)
	if err != nil {
		t.Fatal(err)
	}
	var bobCount int
	if err := bobPool.QueryRow(ctx, `SELECT count(*) FROM workflow_api_keys`).Scan(&bobCount); err != nil {
		t.Fatal(err)
	}
	if bobCount != 0 {
		t.Fatalf("bob saw %d keys, want 0", bobCount)
	}

	// Authenticate succeeds and updates last_used_at.
	now = now.Add(5 * time.Minute)
	auth, err := store.AuthenticateWorkflowAPIKey(ctx, k.Key)
	if err != nil {
		t.Fatalf("auth failed: %v", err)
	}
	if auth.Account.ID != alice.ID || auth.WorkflowID != wfID || auth.KeyID != k.ID || auth.KeyName != "ci-prod" {
		t.Fatalf("unexpected auth: %+v", auth)
	}
	var lastUsed *time.Time
	if err := d.Owner.QueryRow(ctx, `SELECT last_used_at FROM workflow_api_keys WHERE id = $1`, k.ID).Scan(&lastUsed); err != nil {
		t.Fatal(err)
	}
	if lastUsed == nil || !lastUsed.Equal(now) {
		t.Fatalf("last_used_at = %v, want %v", lastUsed, now)
	}

	// Unknown / malformed keys fail with ErrNoSession.
	for _, bad := range []string{"", "wrong", "iwfk_" + strings.Repeat("x", 43)} {
		if _, err := store.AuthenticateWorkflowAPIKey(ctx, bad); !errors.Is(err, accounts.ErrNoSession) {
			t.Fatalf("auth(%q) = %v, want ErrNoSession", bad, err)
		}
	}

	// Disabling API trigger makes auth return ErrAPITriggerDisabled.
	if _, err := d.Owner.Exec(ctx, `UPDATE workflow_versions SET definition = jsonb_set(definition, '{trigger}', '"manual"') WHERE workflow_id = $1`, wfID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthenticateWorkflowAPIKey(ctx, k.Key); !errors.Is(err, accounts.ErrAPITriggerDisabled) {
		t.Fatalf("disabled trigger auth = %v, want ErrAPITriggerDisabled", err)
	}
	// Re-enable trigger.
	if _, err := d.Owner.Exec(ctx, `UPDATE workflow_versions SET definition = jsonb_set(definition, '{trigger}', '"api"') WHERE workflow_id = $1`, wfID); err != nil {
		t.Fatal(err)
	}

	// Bob cannot revoke Alice's key.
	ok, err := store.RevokeWorkflowAPIKey(ctx, bob, wfID, k.ID)
	if err != nil || ok {
		t.Fatalf("bob revoke = (%v, %v), want (false, nil)", ok, err)
	}

	// Alice revokes her key; auth now returns ErrKeyRevoked.
	now = now.Add(time.Minute)
	ok, err = store.RevokeWorkflowAPIKey(ctx, alice, wfID, k.ID)
	if err != nil || !ok {
		t.Fatalf("alice revoke = (%v, %v), want (true, nil)", ok, err)
	}
	if _, err := store.AuthenticateWorkflowAPIKey(ctx, k.Key); !errors.Is(err, accounts.ErrKeyRevoked) {
		t.Fatalf("revoked auth = %v, want ErrKeyRevoked", err)
	}

	// Contract check still passes with new table/columns.
	if err := db.CheckContract(ctx, d.Owner, d.Readonly); err != nil {
		t.Fatalf("CheckContract: %v", err)
	}
}
