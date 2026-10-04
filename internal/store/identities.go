package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Single sign-on links. A user signs in through an OpenID Connect provider
// as (issuer, subject); the first sign-in matches a verified email to an
// existing user (or an open invite) and records the link, later ones follow
// the link. A user has at most one subject per issuer, so a different
// account at the provider that later claims the same email cannot take the
// user over.

var ErrIdentityConflict = errors.New("the user is linked to another account at this provider")

// Identity is one link between a provider account and a user.
type Identity struct {
	Issuer      string
	Subject     string
	UserID      string
	Email       string // the email the provider asserted when the link was made
	CreatedAt   time.Time
	LastLoginAt time.Time
}

// UserByIdentity follows a link; ErrNotFound when there is none.
func (s *Store) UserByIdentity(ctx context.Context, issuer, subject string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+prefixed("u.", userColumns)+`
		FROM user_identities i JOIN users u ON u.id = i.user_id WHERE i.issuer = ? AND i.subject = ?`, issuer, subject))
}

// LinkIdentity records that (issuer, subject) signs in as userID. It fails
// with ErrIdentityConflict if the user is already linked to another subject
// at this issuer.
func (s *Store) LinkIdentity(ctx context.Context, id Identity) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO user_identities (issuer, subject, user_id, email, created_at, last_login_at)
		VALUES (?, ?, ?, ?, ?, ?)`, id.Issuer, id.Subject, id.UserID, id.Email, id.CreatedAt.Unix(), id.CreatedAt.Unix())
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrIdentityConflict
	}
	return err
}

// IdentityUsed records a sign-in through a link.
func (s *Store) IdentityUsed(ctx context.Context, issuer, subject string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE user_identities SET last_login_at = ? WHERE issuer = ? AND subject = ?`, at.Unix(), issuer, subject)
	return err
}

// Identities lists a user's links.
func (s *Store) Identities(ctx context.Context, userID string) ([]Identity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT issuer, subject, user_id, email, created_at, last_login_at
		FROM user_identities WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var i Identity
		var c, l int64
		if err := rows.Scan(&i.Issuer, &i.Subject, &i.UserID, &i.Email, &c, &l); err != nil {
			return nil, err
		}
		i.CreatedAt, i.LastLoginAt = time.Unix(c, 0), time.Unix(l, 0)
		out = append(out, i)
	}
	return out, rows.Err()
}

// UnlinkIdentity removes a user's link to an issuer.
func (s *Store) UnlinkIdentity(ctx context.Context, userID, issuer string) error {
	return s.found(s.db.ExecContext(ctx, `DELETE FROM user_identities WHERE user_id = ? AND issuer = ?`, userID, issuer))
}

// AcceptInviteByEmail is AcceptInvite for a single sign-on: the provider
// vouched for the email, which stands in for the invite link. The new user
// gets the invite's role, u's name, no password and the link id, all in one
// transaction. ErrNotFound when no open, unexpired invite exists.
func (s *Store) AcceptInviteByEmail(ctx context.Context, email string, u *User, id Identity, now time.Time) (*Invite, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	inv, err := scanInvite(tx.QueryRowContext(ctx, `SELECT `+inviteColumns+` FROM invites
		WHERE email = ? AND accepted_at IS NULL AND revoked_at IS NULL`, strings.TrimSpace(email)))
	if err != nil {
		return nil, err
	}
	if err := inv.Err(now); err != nil {
		return nil, ErrNotFound
	}
	u.Email, u.Role, u.OrgID, u.CreatedAt, u.PasswordHash = inv.Email, inv.Role, inv.OrgID, now, ""
	if err := insertUser(ctx, tx, u); err != nil {
		return inv, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE invites SET accepted_at = ?, accepted_user_id = ? WHERE id = ?`, now.Unix(), u.ID, inv.ID); err != nil {
		return nil, err
	}
	if err := linkTx(ctx, tx, u.ID, id, now); err != nil {
		return nil, err
	}
	return inv, tx.Commit()
}

// CreateUserWithIdentity adds a user without a password (auto-join through
// single sign-on) together with its link.
func (s *Store) CreateUserWithIdentity(ctx context.Context, u *User, id Identity, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	u.PasswordHash, u.CreatedAt = "", now
	if err := insertUser(ctx, tx, u); err != nil {
		return err
	}
	if err := linkTx(ctx, tx, u.ID, id, now); err != nil {
		return err
	}
	return tx.Commit()
}

func linkTx(ctx context.Context, tx *sql.Tx, userID string, id Identity, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO user_identities (issuer, subject, user_id, email, created_at, last_login_at)
		VALUES (?, ?, ?, ?, ?, ?)`, id.Issuer, id.Subject, userID, id.Email, now.Unix(), now.Unix())
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrIdentityConflict
	}
	return err
}

// ---- secrets sealed with the data key ---------------------------------------

// SealedKind names what a sealed value is, for the context it is bound to.
const SealedTOTP = "totp"

// ResealFunc re-seals one value: kind and owner (the user ID) give its
// context. It returns the value unchanged and false when it is current.
type ResealFunc func(kind, owner, sealed string) (string, bool, error)

// ResealSecrets passes every value sealed with the data key through fn and
// stores what changed, in one transaction: after a key rotation either all
// values move to the new key or none do. It returns how many changed.
func (s *Store) ResealSecrets(ctx context.Context, fn ResealFunc) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT user_id, secret FROM totp`)
	if err != nil {
		return 0, err
	}
	type row struct{ user, secret string }
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.user, &r.secret); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, r := range all {
		out, changed, err := fn(SealedTOTP, r.user, r.secret)
		if err != nil {
			return 0, err
		}
		if !changed {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE totp SET secret = ? WHERE user_id = ?`, out, r.user); err != nil {
			return 0, err
		}
		n++
	}
	return n, tx.Commit()
}

// SealedValues counts the values sealed with the data key and how many of
// them current reports as sealed with the newest key.
func (s *Store) SealedValues(ctx context.Context, current func(sealed string) bool) (total, upToDate int, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT secret FROM totp`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return 0, 0, err
		}
		total++
		if current(v) {
			upToDate++
		}
	}
	return total, upToDate, rows.Err()
}
