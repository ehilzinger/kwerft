package store

import "context"

// PasskeyHolder is a user with passkeys and their other second factors.
// Passkeys are bound to the console's hostname, so moving the console leaves
// these users with whatever else they have.
type PasskeyHolder struct {
	Email string
	Name  string
	Factors
}

// StrandedByMove reports whether the user could not sign in once their
// passkeys stop working: no authenticator app and no unused recovery code.
func (h PasskeyHolder) StrandedByMove() bool { return !h.TOTP && h.RecoveryCodes == 0 }

// PasskeyHolders lists every user with at least one passkey, by email.
func (s *Store) PasskeyHolders(ctx context.Context) ([]PasskeyHolder, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.email, u.name,
		(SELECT COUNT(*) FROM totp t WHERE t.user_id = u.id AND t.confirmed = 1),
		(SELECT COUNT(*) FROM passkeys p WHERE p.user_id = u.id),
		(SELECT COUNT(*) FROM recovery_codes r WHERE r.user_id = u.id AND r.used_at IS NULL)
		FROM users u
		WHERE EXISTS (SELECT 1 FROM passkeys p WHERE p.user_id = u.id)
		ORDER BY u.email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PasskeyHolder
	for rows.Next() {
		var h PasskeyHolder
		if err := rows.Scan(&h.Email, &h.Name, &h.TOTP, &h.Passkeys, &h.RecoveryCodes); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
