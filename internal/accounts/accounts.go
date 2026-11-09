// Package accounts holds who can sign in, their browsers' sessions, and the hosts each owns.
//
// An account signs in with a username and password. A host joins an account by showing a short
// code that a signed-in account enters; the host then collects its own key, which it streams with.
// Only hashes of passwords, session tokens, codes and keys are stored. Everything an account reads
// it reads through its own Postgres role (Reader), scoped to its own rows by row-level security.
package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zachlatta/intelligence/internal/db"
	"github.com/zachlatta/intelligence/internal/fleet"
	"github.com/zachlatta/intelligence/internal/hostclient"
)

// Account is someone who can sign in.
type Account struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Role     string `json:"-"` // the Postgres role it reads as
}

var (
	ErrTaken            = errors.New("that username is taken")
	ErrBadCredentials   = errors.New("wrong username or password")
	ErrNoSession        = errors.New("not signed in")
	ErrNoSuchCode       = errors.New("no host is waiting with that code (codes last an hour)")
	ErrHostTaken        = errors.New("a host with that name belongs to another account")
	ErrNotClaimed       = errors.New("not claimed yet")
	ErrNoSuchEnrollment = errors.New("no such enrollment, or it expired")
	ErrNoSuchHost       = errors.New("no such host in this account")
)

// BadInputError is a username or password that cannot be used, and why.
type BadInputError struct{ Reason string }

func (e *BadInputError) Error() string { return e.Reason }

const (
	// SessionLife is how long a browser stays signed in.
	SessionLife = 30 * 24 * time.Hour
	// EnrollmentLife is how long a host's code can be entered.
	EnrollmentLife = time.Hour
	minPassword    = 12
	maxPassword    = 1024
)

var usernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Store keeps accounts in the app's database.
type Store struct {
	p     *pgxpool.Pool
	roles *db.AccountRoles
	Now   func() time.Time
}

// New keeps accounts in owner's database, each reading through its role from roles.
func New(owner *pgxpool.Pool, roles *db.AccountRoles) *Store {
	return &Store{p: owner, roles: roles, Now: time.Now}
}

// Reader is the pool a reads through: it sees only a's own rows.
func (s *Store) Reader(ctx context.Context, a Account) (*pgxpool.Pool, error) {
	return s.roles.Pool(ctx, a.Role)
}

func normalize(username string) string { return strings.ToLower(strings.TrimSpace(username)) }

func checkPassword(pw string) error {
	switch {
	case len(pw) < minPassword:
		return &BadInputError{fmt.Sprintf("use a password of at least %d characters", minPassword)}
	case len(pw) > maxPassword:
		return &BadInputError{fmt.Sprintf("use a password of at most %d characters", maxPassword)}
	}
	return nil
}

// Create makes an account, and its role.
func (s *Store) Create(ctx context.Context, username, password string) (Account, error) {
	username = normalize(username)
	if !usernameRE.MatchString(username) {
		return Account{}, &BadInputError{"use a username of 1 to 32 lowercase letters, digits, '-' and '_'"}
	}
	if err := checkPassword(password); err != nil {
		return Account{}, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return Account{}, err
	}
	a := Account{Username: username}
	err = pgx.BeginFunc(ctx, s.p, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT nextval(pg_get_serial_sequence('accounts', 'id'))").Scan(&a.ID); err != nil {
			return err
		}
		if a.Role, err = s.roles.RoleName(a.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO accounts (id, username, password_hash, role_name, created_at, password_changed_at)
			VALUES ($1, $2, $3, $4, $5, $5)`, a.ID, a.Username, hash, a.Role, s.Now()); err != nil {
			var pg *pgconn.PgError
			if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "accounts_username_key" {
				return ErrTaken
			}
			return err
		}
		return s.roles.Ensure(ctx, tx, a.Role)
	})
	if err != nil {
		return Account{}, err
	}
	return a, nil
}

// Authenticate is the account with username and password, or ErrBadCredentials.
func (s *Store) Authenticate(ctx context.Context, username, password string) (Account, error) {
	a := Account{Username: normalize(username)}
	var hash string
	err := s.p.QueryRow(ctx, "SELECT id, role_name, password_hash FROM accounts WHERE username = $1", a.Username).Scan(&a.ID, &a.Role, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = verifyPassword(dummyHash, password) // take as long as for a real account
		return Account{}, ErrBadCredentials
	}
	if err != nil {
		return Account{}, err
	}
	if !verifyPassword(hash, password) {
		return Account{}, ErrBadCredentials
	}
	return a, nil
}

// ChangePassword sets account id's password, given its current one, and ends all its sessions.
func (s *Store) ChangePassword(ctx context.Context, id int64, current, next string) error {
	var hash string
	if err := s.p.QueryRow(ctx, "SELECT password_hash FROM accounts WHERE id = $1", id).Scan(&hash); err != nil {
		return err
	}
	if !verifyPassword(hash, current) {
		return ErrBadCredentials
	}
	if err := checkPassword(next); err != nil {
		return err
	}
	newHash, err := hashPassword(next)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.p, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE accounts SET password_hash = $2, password_changed_at = $3 WHERE id = $1", id, newHash, s.Now()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "DELETE FROM sessions WHERE account_id = $1", id)
		return err
	})
}

// randomToken is 32 random bytes, URL-safe.
func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// StartSession signs a browser in as a: the token goes in its cookie.
func (s *Store) StartSession(ctx context.Context, a Account) (token string, expires time.Time, err error) {
	token, now := randomToken(), s.Now()
	expires = now.Add(SessionLife)
	_, err = s.p.Exec(ctx, `INSERT INTO sessions (token_hash, account_id, created_at, expires_at, last_seen_at) VALUES ($1, $2, $3, $4, $3)`,
		sha(token), a.ID, now, expires)
	return token, expires, err
}

// Session is the account signed in with token, or ErrNoSession.
func (s *Store) Session(ctx context.Context, token string) (Account, error) {
	if token == "" {
		return Account{}, ErrNoSession
	}
	var a Account
	// last_seen_at is kept to the minute, so most requests write nothing
	err := s.p.QueryRow(ctx, `WITH seen AS (
			UPDATE sessions SET last_seen_at = $2 WHERE token_hash = $1 AND expires_at > $2 AND last_seen_at < $2 - interval '1 minute')
		SELECT a.id, a.username, a.role_name FROM sessions s JOIN accounts a ON a.id = s.account_id
		WHERE s.token_hash = $1 AND s.expires_at > $2`, sha(token), s.Now()).Scan(&a.ID, &a.Username, &a.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNoSession
	}
	return a, err
}

// EndSession signs token's browser out.
func (s *Store) EndSession(ctx context.Context, token string) error {
	_, err := s.p.Exec(ctx, "DELETE FROM sessions WHERE token_hash = $1 OR expires_at < $2", sha(token), s.Now())
	return err
}

// Enrollment is a host asking to join an account. Code is shown to a person, who enters it
// while signed in; Secret stays on the host, which collects its key with it.
type Enrollment struct {
	ID        string    `json:"id"`
	Host      string    `json:"host"`
	Code      string    `json:"code"`
	Secret    string    `json:"secret"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// codeAlphabet is Crockford's base 32: no I, L, O or U to misread.
const codeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func newCode() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	var out strings.Builder
	for i, c := range b {
		if i == 4 {
			out.WriteByte('-')
		}
		out.WriteByte(codeAlphabet[int(c)%32])
	}
	return out.String()
}

// normalizeCode reads a code as typed: any case, dashes and spaces anywhere, O for 0, I and L for 1.
func normalizeCode(code string) string {
	code = strings.Map(func(r rune) rune {
		switch r {
		case '-', ' ', '\t':
			return -1
		}
		return r
	}, strings.ToUpper(code))
	code = strings.NewReplacer("O", "0", "I", "1", "L", "1").Replace(code)
	if len(code) != 8 {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// Enroll starts host asking to join an account.
func (s *Store) Enroll(ctx context.Context, host string) (Enrollment, error) {
	if !hostclient.ValidName(host) {
		return Enrollment{}, &BadInputError{"bad host name: use lowercase letters, digits, '.', '_' and '-'"}
	}
	now := s.Now()
	e := Enrollment{Host: host, Code: newCode(), Secret: randomToken(), ExpiresAt: now.Add(EnrollmentLife)}
	err := pgx.BeginFunc(ctx, s.p, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM host_enrollments WHERE expires_at < $1", now); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO host_enrollments (host, code_hash, secret_hash, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`, host, sha(e.Code), sha(e.Secret), now, e.ExpiresAt).Scan(&e.ID)
	})
	return e, err
}

// Claim gives the host waiting with code to a, and returns its name. A host whose name is another
// account's cannot be claimed.
func (s *Store) Claim(ctx context.Context, a Account, code string) (string, error) {
	var host string
	err := pgx.BeginFunc(ctx, s.p, func(tx pgx.Tx) error {
		var id string
		var claimedBy *int64
		err := tx.QueryRow(ctx, `SELECT id, host, account_id FROM host_enrollments WHERE code_hash = $1 AND expires_at > $2 FOR UPDATE`,
			sha(normalizeCode(code)), s.Now()).Scan(&id, &host, &claimedBy)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && claimedBy != nil && *claimedBy != a.ID) {
			return ErrNoSuchCode
		}
		if err != nil {
			return err
		}
		var owner int64
		err = tx.QueryRow(ctx, "SELECT account_id FROM host_keys WHERE host = $1", host).Scan(&owner)
		if err == nil && owner != a.ID {
			return ErrHostTaken
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE host_enrollments SET account_id = $2, claimed_at = $3 WHERE id = $1", id, a.ID, s.Now()); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, fleet.NotifySQL, fleet.EnrollmentTopic(id))
		return err
	})
	return host, err
}

// Collect is the key for the host that enrolled as id, once claimed: ErrNotClaimed until then. The
// key replaces any the host had, and is handed out once.
func (s *Store) Collect(ctx context.Context, id, secret string) (key, host string, err error) {
	err = pgx.BeginFunc(ctx, s.p, func(tx pgx.Tx) error {
		var secretHash string
		var account *int64
		err := tx.QueryRow(ctx, `SELECT host, secret_hash, account_id FROM host_enrollments WHERE id::text = $1 AND expires_at > $2 FOR UPDATE`,
			id, s.Now()).Scan(&host, &secretHash, &account)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && subtle.ConstantTimeCompare([]byte(secretHash), []byte(sha(secret))) != 1) {
			return ErrNoSuchEnrollment
		}
		if err != nil {
			return err
		}
		if account == nil {
			return ErrNotClaimed
		}
		key = "ibk_" + randomToken()
		tag, err := tx.Exec(ctx, `INSERT INTO host_keys (host, account_id, key_hash, created_at) VALUES ($1, $2, $3, $4)
			ON CONFLICT (host) DO UPDATE SET key_hash = excluded.key_hash, created_at = excluded.created_at, last_used_at = NULL
			WHERE host_keys.account_id = excluded.account_id`, host, *account, sha(key), s.Now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 { // another account took the name since the claim
			return ErrHostTaken
		}
		_, err = tx.Exec(ctx, "DELETE FROM host_enrollments WHERE id = $1::uuid", id)
		return err
	})
	if err != nil {
		return "", "", err
	}
	return key, host, nil
}

// HostKey says whether key is host's, and notes its use.
func (s *Store) HostKey(ctx context.Context, host, key string) (bool, error) {
	tag, err := s.p.Exec(ctx, "UPDATE host_keys SET last_used_at = $3 WHERE host = $1 AND key_hash = $2", host, sha(key), s.Now())
	return err == nil && tag.RowsAffected() == 1, err
}

// HostOfKey is the host key is good for, or "" when it is no host's key. It records the key as used.
func (s *Store) HostOfKey(ctx context.Context, key string) (string, error) {
	if key == "" {
		return "", nil
	}
	var host string
	err := s.p.QueryRow(ctx, "UPDATE host_keys SET last_used_at = $2 WHERE key_hash = $1 RETURNING host", sha(key), s.Now()).Scan(&host)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return host, err
}

// Owns says whether host is account id's.
func (s *Store) Owns(ctx context.Context, id int64, host string) (bool, error) {
	var owns bool
	err := s.p.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM host_keys WHERE host = $1 AND account_id = $2)", host, id).Scan(&owns)
	return owns, err
}

// RemoveHost takes host from account id, with everything it reported: its key stops working, and
// the host asks to join again. Its name is free for any account.
func (s *Store) RemoveHost(ctx context.Context, id int64, host string) error {
	return pgx.BeginFunc(ctx, s.p, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "DELETE FROM host_keys WHERE host = $1 AND account_id = $2", host, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNoSuchHost
		}
		return fleet.Forget(ctx, tx, host)
	})
}
