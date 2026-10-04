package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Kinds of API token. A kubeconfig token is an API token made by the
// "Download kubeconfig" button; it works the same, the kind only labels it.
const (
	TokenKindAPI        = "api"
	TokenKindKubeconfig = "kubeconfig"
)

// MaxTokensPerUser bounds how many unexpired tokens one account may hold.
const MaxTokensPerUser = 50

var ErrTooManyTokens = errors.New("too many API tokens")

// APIToken is a long-lived credential for scripts, CI and kubectl. Only the
// SHA-256 of the token is stored (TokenHash); Hint is its recognisable start.
type APIToken struct {
	ID        string
	UserID    string
	Name      string
	Kind      string
	TokenHash string
	Hint      string
	// Role caps what the token may do: requests act with the lower of this
	// and the user's current role.
	Role string
	// Projects restricts the token to these projects; nil means every
	// project the user reaches.
	Projects   []string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastUsedAt time.Time // zero: never used
	LastUsedIP string
}

const tokenColumns = `id, user_id, name, kind, token_hash, hint, role, projects, created_at, expires_at, last_used_at, last_used_ip`

func scanToken(row interface{ Scan(...any) error }) (*APIToken, error) {
	var t APIToken
	var projects string
	var created, expires, used int64
	if err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.Kind, &t.TokenHash, &t.Hint, &t.Role, &projects,
		&created, &expires, &used, &t.LastUsedIP); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if projects != "" {
		if err := json.Unmarshal([]byte(projects), &t.Projects); err != nil {
			return nil, err
		}
	}
	t.CreatedAt, t.ExpiresAt = time.Unix(created, 0), time.Unix(expires, 0)
	if used > 0 {
		t.LastUsedAt = time.Unix(used, 0)
	}
	return &t, nil
}

// CreateAPIToken stores a token. It fails with ErrTooManyTokens when the user
// already holds MaxTokensPerUser unexpired ones.
func (s *Store) CreateAPIToken(ctx context.Context, t *APIToken) error {
	t.ID = newID()
	projects := ""
	if t.Projects != nil {
		b, err := json.Marshal(t.Projects)
		if err != nil {
			return err
		}
		projects = string(b)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_tokens WHERE user_id = ? AND expires_at > ?`,
		t.UserID, t.CreatedAt.Unix()).Scan(&n); err != nil {
		return err
	}
	if n >= MaxTokensPerUser {
		return ErrTooManyTokens
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO api_tokens (`+tokenColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '')`,
		t.ID, t.UserID, t.Name, t.Kind, t.TokenHash, t.Hint, t.Role, projects, t.CreatedAt.Unix(), t.ExpiresAt.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// APITokenByHash returns a token and its user. An unknown (or revoked)
// token is ErrNotFound; an expired one ErrTokenExpired, with token and user
// for the audit log. The user's role is read here, with the token, so a role
// change takes effect on the next request.
func (s *Store) APITokenByHash(ctx context.Context, hash string, now time.Time) (*APIToken, *User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT t.id, t.user_id, t.name, t.kind, t.token_hash, t.hint, t.role, t.projects,
		       t.created_at, t.expires_at, t.last_used_at, t.last_used_ip, `+prefixed("u.", userColumns)+`
		FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ?`, hash)
	var t APIToken
	var u User
	var projects string
	var created, expires, used, uc int64
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.Kind, &t.TokenHash, &t.Hint, &t.Role, &projects,
		&created, &expires, &used, &t.LastUsedIP,
		&u.ID, &u.OrgID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, &uc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	if projects != "" {
		if err := json.Unmarshal([]byte(projects), &t.Projects); err != nil {
			return nil, nil, err
		}
	}
	t.CreatedAt, t.ExpiresAt = time.Unix(created, 0), time.Unix(expires, 0)
	if used > 0 {
		t.LastUsedAt = time.Unix(used, 0)
	}
	u.CreatedAt = time.Unix(uc, 0)
	if !now.Before(t.ExpiresAt) {
		return &t, &u, ErrTokenExpired
	}
	return &t, &u, nil
}

// ErrTokenExpired comes with the token and user, so the refusal can be
// audited under the user's name.
var ErrTokenExpired = errors.New("API token expired")

// TokenUsed records the last use. Callers throttle it.
func (s *Store) TokenUsed(ctx context.Context, id string, at time.Time, ip string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ?, last_used_ip = ? WHERE id = ?`, at.Unix(), ip, id)
	return err
}

// APITokens lists a user's tokens, expired ones included, newest first.
func (s *Store) APITokens(ctx context.Context, userID string) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tokenColumns+` FROM api_tokens WHERE user_id = ? ORDER BY created_at DESC, rowid DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// DeleteAPIToken revokes one of the user's tokens and returns it.
func (s *Store) DeleteAPIToken(ctx context.Context, userID, id string) (*APIToken, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	t, err := scanToken(tx.QueryRowContext(ctx, `SELECT `+tokenColumns+` FROM api_tokens WHERE id = ? AND user_id = ?`, id, userID))
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ?`, id); err != nil {
		return nil, err
	}
	return t, tx.Commit()
}

// DeleteExpiredTokens removes tokens that expired more than keep ago (they
// stay listed for a while so their owner sees why a script stopped).
func (s *Store) DeleteExpiredTokens(ctx context.Context, now time.Time, keep time.Duration) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE expires_at <= ?`, now.Add(-keep).Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// prefixed qualifies a column list: prefixed("u.", "a, b") is "u.a, u.b".
func prefixed(p, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = p + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}
