package db

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AccountRoles gives each account its own login role, through which it reads only its own rows
// (row-level security keys on the role: current_account_id()). A role is named after the read-only
// role and the account ("<readonly>_a<id>"), is a member of the read-only role (so it can SELECT
// what that role can), and is read-only by default. Its password is derived from the read-only
// role's, which only the server knows, so every server instance derives the same one, and Ensure
// sets it again after the read-only password changes.
type AccountRoles struct {
	owner, readonly *pgxpool.Pool

	mu    sync.Mutex
	pools map[string]*pgxpool.Pool
}

// accountPoolConns bounds each account's read pool.
const accountPoolConns = 4

// NewAccountRoles makes account roles with owner (a role that can create roles) and connects them
// as readonly does, to the same database.
func NewAccountRoles(owner, readonly *pgxpool.Pool) *AccountRoles {
	return &AccountRoles{owner: owner, readonly: readonly, pools: map[string]*pgxpool.Pool{}}
}

// RoleName is account id's role.
func (a *AccountRoles) RoleName(id int64) (string, error) {
	name := a.readonly.Config().ConnConfig.User + "_a" + strconv.FormatInt(id, 10)
	if len(name) > 63 {
		return "", fmt.Errorf("account role %q is longer than Postgres allows: use a shorter read-only role name", name)
	}
	return name, nil
}

func (a *AccountRoles) password(role string) string {
	m := hmac.New(sha256.New, []byte(a.readonly.Config().ConnConfig.Password))
	m.Write([]byte("intelligence account role\x00" + role))
	return hex.EncodeToString(m.Sum(nil))
}

// Querier is a pool, a connection or a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Ensure creates role, or brings it up to date: its password, its settings, its membership.
// Within a transaction it is undone with it.
func (a *AccountRoles) Ensure(ctx context.Context, q Querier, role string) error {
	ro := a.readonly.Config().ConnConfig.User
	if role == "" || ro == "" {
		return errors.New("account role: no name")
	}
	var exists bool
	if err := q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&exists); err != nil {
		return err
	}
	verb := "ALTER"
	if !exists {
		verb = "CREATE"
	}
	var set string
	if err := q.QueryRow(ctx, "SELECT format('"+verb+" ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE INHERIT NOBYPASSRLS NOREPLICATION PASSWORD %L', $1::text, $2::text)",
		role, a.password(role)).Scan(&set); err != nil {
		return err
	}
	r := pgx.Identifier{role}.Sanitize()
	for _, s := range []string{
		set,
		"ALTER ROLE " + r + " SET default_transaction_read_only = on",
		"ALTER ROLE " + r + " SET statement_timeout = '10s'",
		"GRANT " + pgx.Identifier{ro}.Sanitize() + " TO " + r,
	} {
		if _, err := q.Exec(ctx, s); err != nil {
			return fmt.Errorf("account role %s: %w", role, err)
		}
	}
	return nil
}

// Pool is role's read pool, made (and the role brought up to date) the first time it is asked for.
func (a *AccountRoles) Pool(ctx context.Context, role string) (*pgxpool.Pool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p := a.pools[role]; p != nil {
		return p, nil
	}
	if err := a.Ensure(ctx, a.owner, role); err != nil {
		return nil, err
	}
	cfg := a.readonly.Config().Copy()
	cfg.ConnConfig.User, cfg.ConnConfig.Password = role, a.password(role)
	cfg.MaxConns = accountPoolConns
	cfg.MinConns = 0
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	a.pools[role] = p
	return p, nil
}

// Close closes every account's pool.
func (a *AccountRoles) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for role, p := range a.pools {
		p.Close()
		delete(a.pools, role)
	}
}
