// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
)

// ---- console-wide settings -------------------------------------------------

// settingRequireTwoFactor: "1" sends everyone without a second factor to
// enrol one after their password (internal/server, sign-in policy).
const settingRequireTwoFactor = "require_two_factor"

func (s *Store) setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE org_id = ? AND key = ?`, DefaultOrg, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) setSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (org_id, key, value) VALUES (?, ?, ?)
		ON CONFLICT (org_id, key) DO UPDATE SET value = excluded.value`, DefaultOrg, key, value)
	return err
}

// RequireTwoFactor reports whether every member must have a second factor.
func (s *Store) RequireTwoFactor(ctx context.Context) (bool, error) {
	v, err := s.setting(ctx, settingRequireTwoFactor)
	return v == "1", err
}

func (s *Store) SetRequireTwoFactor(ctx context.Context, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	return s.setSetting(ctx, settingRequireTwoFactor, v)
}

// ---- resetting someone's second factors -------------------------------------

// ResetSecondFactors removes a user's authenticator app, passkeys and
// recovery codes and ends all their sessions, in one transaction; they sign
// in with their password next and set up a factor again. check works as in
// SetRole. It returns the user, the factors they had and how many sessions
// ended.
func (s *Store) ResetSecondFactors(ctx context.Context, id string, check func(current *User) error) (*User, Factors, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, Factors{}, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
	if err != nil {
		return nil, Factors{}, 0, err
	}
	if check != nil {
		if err := check(u); err != nil {
			return nil, Factors{}, 0, err
		}
	}
	var f Factors
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM totp WHERE user_id = ?1 AND confirmed = 1),
		(SELECT COUNT(*) FROM passkeys WHERE user_id = ?1),
		(SELECT COUNT(*) FROM recovery_codes WHERE user_id = ?1 AND used_at IS NULL)`, id).
		Scan(&f.TOTP, &f.Passkeys, &f.RecoveryCodes); err != nil {
		return nil, Factors{}, 0, err
	}
	for _, q := range []string{
		`DELETE FROM totp WHERE user_id = ?`,
		`DELETE FROM passkeys WHERE user_id = ?`,
		`DELETE FROM recovery_codes WHERE user_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return nil, Factors{}, 0, err
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id)
	if err != nil {
		return nil, Factors{}, 0, err
	}
	sessions, _ := res.RowsAffected()
	return u, f, sessions, tx.Commit()
}
