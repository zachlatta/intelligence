package accounts

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// MCPToken is a credential for the read-only MCP endpoint. Token is returned only at creation.
type MCPToken struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (s *Store) CreateMCPToken(ctx context.Context, a Account) (MCPToken, error) {
	now := s.Now()
	tok := MCPToken{Token: "imcp_" + randomToken(), ExpiresAt: now.Add(90 * 24 * time.Hour)}
	err := s.p.QueryRow(ctx, `INSERT INTO mcp_tokens (account_id,token_hash,created_at,expires_at) VALUES ($1,$2,$3,$4) RETURNING id`, a.ID, sha(tok.Token), now, tok.ExpiresAt).Scan(&tok.ID)
	return tok, err
}

// MCPAccount authenticates only MCP tokens, without writing any application state.
func (s *Store) MCPAccount(ctx context.Context, token string) (Account, error) {
	var a Account
	err := s.p.QueryRow(ctx, `SELECT a.id,a.username,a.role_name FROM mcp_tokens t JOIN accounts a ON a.id=t.account_id
 WHERE t.token_hash=$1 AND t.expires_at>$2`, sha(token), s.Now()).Scan(&a.ID, &a.Username, &a.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNoSession
	}
	return a, err
}

func (s *Store) RevokeMCPToken(ctx context.Context, a Account, id string) (bool, error) {
	tag, err := s.p.Exec(ctx, `DELETE FROM mcp_tokens WHERE account_id=$1 AND id::text=$2`, a.ID, id)
	return tag.RowsAffected() == 1, err
}
