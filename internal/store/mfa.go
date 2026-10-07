// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var (
	ErrTOTPEnabled   = errors.New("an authenticator app is already set up")
	ErrPasskeyExists = errors.New("passkey already registered")
)

// TOTP is a user's authenticator app. Until Confirmed it is an enrollment in
// progress and does not count as a second factor.
type TOTP struct {
	UserID    string
	Secret    string // sealed with the data key, see auth.Sealer
	Confirmed bool
	LastStep  int64
	CreatedAt time.Time
}

// Passkey is a WebAuthn credential. Credential is the JSON of the library's
// credential record (public key, sign count, flags); the store does not
// interpret it.
type Passkey struct {
	ID           string
	UserID       string
	CredentialID []byte
	Name         string
	Credential   []byte
	CreatedAt    time.Time
	LastUsedAt   time.Time // zero if never used to sign in
}

// Factors summarizes a user's second factors.
type Factors struct {
	TOTP          bool // confirmed authenticator app
	Passkeys      int
	RecoveryCodes int // unused ones
}

// Any reports whether sign-in needs a second factor.
func (f Factors) Any() bool { return f.TOTP || f.Passkeys > 0 }

func (s *Store) Factors(ctx context.Context, userID string) (Factors, error) {
	var f Factors
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM totp WHERE user_id = ?1 AND confirmed = 1),
		(SELECT COUNT(*) FROM passkeys WHERE user_id = ?1),
		(SELECT COUNT(*) FROM recovery_codes WHERE user_id = ?1 AND used_at IS NULL)`, userID).
		Scan(&f.TOTP, &f.Passkeys, &f.RecoveryCodes)
	return f, err
}

// ---- TOTP ------------------------------------------------------------------

func (s *Store) TOTPFor(ctx context.Context, userID string) (*TOTP, error) {
	var t TOTP
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id, secret, confirmed, last_step, created_at FROM totp WHERE user_id = ?`, userID).
		Scan(&t.UserID, &t.Secret, &t.Confirmed, &t.LastStep, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = time.Unix(created, 0)
	return &t, nil
}

// StartTOTP stores a new, unconfirmed secret, replacing an earlier enrollment
// that was never confirmed. It refuses with ErrTOTPEnabled if one is active.
func (s *Store) StartTOTP(ctx context.Context, userID, sealedSecret string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO totp (user_id, secret, confirmed, last_step, created_at) VALUES (?, ?, 0, 0, ?)
		ON CONFLICT (user_id) DO UPDATE SET secret = excluded.secret, last_step = 0, created_at = excluded.created_at
		WHERE totp.confirmed = 0`, userID, sealedSecret, now.Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTOTPEnabled
	}
	return nil
}

// ConfirmTOTP turns a pending enrollment on, recording the step of the code
// that confirmed it. It reports false if there was nothing to confirm.
func (s *Store) ConfirmTOTP(ctx context.Context, userID string, step int64) (bool, error) {
	return s.affected(s.db.ExecContext(ctx, `UPDATE totp SET confirmed = 1, last_step = ? WHERE user_id = ? AND confirmed = 0`, step, userID))
}

// UseTOTPStep records a sign-in with the code of the given step. It reports
// false if that step (or a later one) was used before: each code works once.
func (s *Store) UseTOTPStep(ctx context.Context, userID string, step int64) (bool, error) {
	return s.affected(s.db.ExecContext(ctx, `UPDATE totp SET last_step = ?1 WHERE user_id = ?2 AND confirmed = 1 AND last_step < ?1`, step, userID))
}

func (s *Store) DeleteTOTP(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM totp WHERE user_id = ?`, userID)
	return err
}

// ---- recovery codes --------------------------------------------------------

// ReplaceRecoveryCodes swaps all of a user's codes for new ones (hashes only).
func (s *Store) ReplaceRecoveryCodes(ctx context.Context, userID string, hashes []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_codes (user_id, code_hash) VALUES (?, ?)`, userID, h); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UseRecoveryCode marks a code used. It reports false for an unknown or
// already used code.
func (s *Store) UseRecoveryCode(ctx context.Context, userID, hash string, now time.Time) (bool, error) {
	return s.affected(s.db.ExecContext(ctx, `UPDATE recovery_codes SET used_at = ? WHERE user_id = ? AND code_hash = ? AND used_at IS NULL`,
		now.Unix(), userID, hash))
}

func (s *Store) DeleteRecoveryCodes(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, userID)
	return err
}

// ---- passkeys --------------------------------------------------------------

func (s *Store) AddPasskey(ctx context.Context, p *Passkey) error {
	p.ID = newID()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO passkeys (id, user_id, credential_id, name, credential, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		p.ID, p.UserID, p.CredentialID, p.Name, string(p.Credential), p.CreatedAt.Unix())
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: passkeys.credential_id") {
		return ErrPasskeyExists
	}
	return err
}

const passkeyColumns = `id, user_id, credential_id, name, credential, created_at, last_used_at`

func scanPasskey(row interface{ Scan(...any) error }) (*Passkey, error) {
	var p Passkey
	var cred string
	var created, used int64
	if err := row.Scan(&p.ID, &p.UserID, &p.CredentialID, &p.Name, &cred, &created, &used); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.Credential = []byte(cred)
	p.CreatedAt = time.Unix(created, 0)
	if used > 0 {
		p.LastUsedAt = time.Unix(used, 0)
	}
	return &p, nil
}

// Passkeys lists a user's passkeys, oldest first.
func (s *Store) Passkeys(ctx context.Context, userID string) ([]Passkey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+passkeyColumns+` FROM passkeys WHERE user_id = ? ORDER BY created_at, rowid`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		p, err := scanPasskey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// PasskeyByCredentialID finds the passkey an authenticator presented.
func (s *Store) PasskeyByCredentialID(ctx context.Context, credentialID []byte) (*Passkey, error) {
	return scanPasskey(s.db.QueryRowContext(ctx, `SELECT `+passkeyColumns+` FROM passkeys WHERE credential_id = ?`, credentialID))
}

// PasskeyUsed stores the updated credential record (sign count, flags) after
// a sign-in.
func (s *Store) PasskeyUsed(ctx context.Context, id string, credential []byte, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE passkeys SET credential = ?, last_used_at = ? WHERE id = ?`, string(credential), at.Unix(), id)
	return err
}

func (s *Store) RenamePasskey(ctx context.Context, userID, id, name string) error {
	return s.found(s.db.ExecContext(ctx, `UPDATE passkeys SET name = ? WHERE id = ? AND user_id = ?`, name, id, userID))
}

func (s *Store) DeletePasskey(ctx context.Context, userID, id string) error {
	return s.found(s.db.ExecContext(ctx, `DELETE FROM passkeys WHERE id = ? AND user_id = ?`, id, userID))
}

// ---- account ---------------------------------------------------------------

func (s *Store) SetUserName(ctx context.Context, userID, name string) error {
	return s.found(s.db.ExecContext(ctx, `UPDATE users SET name = ? WHERE id = ?`, name, userID))
}

func (s *Store) SetPasswordHash(ctx context.Context, userID, hash string) error {
	return s.found(s.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, userID))
}

// UserSessions lists a user's unexpired sessions, most recently used first.
func (s *Store) UserSessions(ctx context.Context, userID string, now time.Time) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id_hash, user_id, created_at, expires_at, last_seen_at, ip, user_agent
		FROM sessions WHERE user_id = ? AND expires_at > ? ORDER BY last_seen_at DESC`, userID, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var sess Session
		var c, e, l int64
		if err := rows.Scan(&sess.IDHash, &sess.UserID, &c, &e, &l, &sess.IP, &sess.UserAgent); err != nil {
			return nil, err
		}
		sess.CreatedAt, sess.ExpiresAt, sess.LastSeenAt = time.Unix(c, 0), time.Unix(e, 0), time.Unix(l, 0)
		out = append(out, sess)
	}
	return out, rows.Err()
}

// DeleteOtherSessions signs a user out everywhere except the session keep.
func (s *Store) DeleteOtherSessions(ctx context.Context, userID, keep string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id_hash <> ?`, userID, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteUserSession removes one of the user's sessions, named by the start of
// its hash (what the account page shows as the session ID).
func (s *Store) DeleteUserSession(ctx context.Context, userID, idPrefix string) error {
	if idPrefix == "" {
		return ErrNotFound
	}
	return s.found(s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND substr(id_hash, 1, ?) = ?`,
		userID, len(idPrefix), idPrefix))
}

// affected turns an UPDATE result into "did it change a row".
func (s *Store) affected(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// found is affected for callers that want ErrNotFound when nothing matched.
func (s *Store) found(res sql.Result, err error) error {
	ok, err := s.affected(res, err)
	if err == nil && !ok {
		return ErrNotFound
	}
	return err
}
