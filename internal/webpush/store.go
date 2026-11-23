package webpush

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists sealed Web Push subscriptions in Postgres and sends notifications to an account's devices.
type Store struct {
	DB     *pgxpool.Pool
	Client *Client
	Now    func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Upsert validates, seals and saves a browser push subscription for accountID.
// Re-subscribing the same endpoint updates its keys and user agent in place.
func (s *Store) Upsert(ctx context.Context, accountID int64, sub Subscription, userAgent string) (string, error) {
	if s.Client == nil || s.Client.VAPID == nil {
		return "", errors.New("web push is not configured")
	}
	host, _, _, err := sub.Validate()
	if err != nil {
		return "", err
	}
	sealed, err := s.Client.VAPID.SealSubscription(sub)
	if err != nil {
		return "", err
	}
	ua := strings.TrimSpace(userAgent)
	if len(ua) > 512 {
		ua = ua[:512]
	}
	var id string
	err = s.DB.QueryRow(ctx, `INSERT INTO push_subscriptions
		(account_id, endpoint_hash, endpoint_host, user_agent, subscription_sealed, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (account_id, endpoint_hash) DO UPDATE
		SET endpoint_host = EXCLUDED.endpoint_host,
		    user_agent = EXCLUDED.user_agent,
		    subscription_sealed = EXCLUDED.subscription_sealed,
		    last_error = '',
		    last_error_at = NULL
		RETURNING id::text`,
		accountID, EndpointHash(sub.Endpoint), host, ua, sealed, s.now()).Scan(&id)
	return id, err
}

// DeleteByEndpoint removes the account's subscription matching endpoint. Returns true if a row was deleted.
func (s *Store) DeleteByEndpoint(ctx context.Context, accountID int64, endpoint string) (bool, error) {
	tag, err := s.DB.Exec(ctx, `DELETE FROM push_subscriptions WHERE account_id = $1 AND endpoint_hash = $2`,
		accountID, EndpointHash(endpoint))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteByID removes one of the account's subscriptions by its UUID. Returns true if a row was deleted.
func (s *Store) DeleteByID(ctx context.Context, accountID int64, id string) (bool, error) {
	tag, err := s.DB.Exec(ctx, `DELETE FROM push_subscriptions WHERE account_id = $1 AND id = $2::uuid`,
		accountID, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

type storedSub struct {
	id     string
	sealed []byte
}

// SendAccount delivers msg to every push subscription owned by accountID.
// Subscriptions rejected with 404 or 410 are deleted automatically.
func (s *Store) SendAccount(ctx context.Context, accountID int64, msg Message) (sent, expired int, err error) {
	if s.Client == nil || s.Client.VAPID == nil {
		return 0, 0, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text, subscription_sealed FROM push_subscriptions WHERE account_id = $1 ORDER BY created_at, id`, accountID)
	if err != nil {
		return 0, 0, err
	}
	subs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (storedSub, error) {
		var row storedSub
		return row, r.Scan(&row.id, &row.sealed)
	})
	if err != nil {
		return 0, 0, err
	}
	var errs []error
	now := s.now()
	for _, row := range subs {
		sub, uerr := s.Client.VAPID.UnsealSubscription(row.sealed)
		if uerr != nil {
			errs = append(errs, fmt.Errorf("subscription %s: unseal: %w", row.id, uerr))
			_, _ = s.DB.Exec(ctx, `UPDATE push_subscriptions SET last_error = $2, last_error_at = $3 WHERE id = $1::uuid`,
				row.id, clipError(uerr.Error()), now)
			continue
		}
		serr := s.Client.Send(ctx, sub, msg)
		switch {
		case serr == nil:
			sent++
			_, _ = s.DB.Exec(ctx, `UPDATE push_subscriptions SET last_sent_at = $2, last_error = '', last_error_at = NULL WHERE id = $1::uuid`,
				row.id, now)
		case errors.Is(serr, ErrSubscriptionExpired):
			expired++
			_, _ = s.DB.Exec(ctx, `DELETE FROM push_subscriptions WHERE id = $1::uuid`, row.id)
		default:
			errs = append(errs, fmt.Errorf("subscription %s: %w", row.id, serr))
			_, _ = s.DB.Exec(ctx, `UPDATE push_subscriptions SET last_error = $2, last_error_at = $3 WHERE id = $1::uuid`,
				row.id, clipError(serr.Error()), now)
		}
	}
	return sent, expired, errors.Join(errs...)
}

func clipError(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 500 {
		return string(r[:499]) + "…"
	}
	return s
}
