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
	ErrLastOwner     = errors.New("the last owner cannot be removed or demoted")
	ErrInvitePending = errors.New("an invite for this email is already open")
	ErrInviteUsed    = errors.New("invite already accepted")
	ErrInviteRevoked = errors.New("invite revoked")
	ErrInviteExpired = errors.New("invite expired")
)

// ---- members ---------------------------------------------------------------

// Member is a user with what the Access page shows about them.
type Member struct {
	User
	TOTP       bool
	Passkeys   int
	LastActive time.Time // newest session activity; zero without a session
}

// Members lists every user, most powerful role first, then by name.
func (s *Store) Members(ctx context.Context) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+userColumns+`,
		       (SELECT COUNT(*) FROM totp t WHERE t.user_id = u.id AND t.confirmed = 1),
		       (SELECT COUNT(*) FROM passkeys p WHERE p.user_id = u.id),
		       COALESCE((SELECT MAX(last_seen_at) FROM sessions s WHERE s.user_id = u.id), 0)
		FROM users u
		ORDER BY CASE role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 WHEN 'developer' THEN 2 ELSE 3 END, name COLLATE NOCASE, email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		var created, last int64
		if err := rows.Scan(&m.ID, &m.OrgID, &m.Email, &m.Name, &m.PasswordHash, &m.Role, &created, &m.TOTP, &m.Passkeys, &last); err != nil {
			return nil, err
		}
		m.CreatedAt = time.Unix(created, 0)
		if last > 0 {
			m.LastActive = time.Unix(last, 0)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountOwners reports how many owners exist.
func (s *Store) CountOwners(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'owner'`).Scan(&n)
	return n, err
}

// lastOwner reports whether demoting or removing u would leave no owner.
func lastOwner(ctx context.Context, tx *sql.Tx, u *User) (bool, error) {
	if u.Role != RoleOwner {
		return false, nil
	}
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'owner'`).Scan(&n)
	return n <= 1, err
}

// SetRole changes a user's role. check sees the user as they are inside the
// transaction and may refuse (the caller's own rules, such as "admins don't
// touch owners"); demoting the last owner fails with ErrLastOwner. It returns
// the user as they were before.
func (s *Store) SetRole(ctx context.Context, id, role string, check func(current *User) error) (*User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if check != nil {
		if err := check(u); err != nil {
			return nil, err
		}
	}
	if u.Role == role {
		return u, nil
	}
	if role != RoleOwner {
		if last, err := lastOwner(ctx, tx, u); err != nil {
			return nil, err
		} else if last {
			return nil, ErrLastOwner
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, role, id); err != nil {
		return nil, err
	}
	return u, tx.Commit()
}

// DeleteUser removes a user with everything that hangs off them: sessions
// (so they are signed out at once), second factors and recovery codes. check
// works as in SetRole; removing the last owner fails with ErrLastOwner. It
// returns the removed user and how many sessions ended.
func (s *Store) DeleteUser(ctx context.Context, id string, check func(current *User) error) (*User, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
	if err != nil {
		return nil, 0, err
	}
	if check != nil {
		if err := check(u); err != nil {
			return nil, 0, err
		}
	}
	if last, err := lastOwner(ctx, tx, u); err != nil {
		return nil, 0, err
	} else if last {
		return nil, 0, ErrLastOwner
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id)
	if err != nil {
		return nil, 0, err
	}
	sessions, _ := res.RowsAffected()
	// Foreign keys cascade to totp, recovery codes and passkeys.
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id); err != nil {
		return nil, 0, err
	}
	return u, sessions, tx.Commit()
}

// ---- invites ---------------------------------------------------------------

// Invite is a single-use link that creates an account with a role. Only the
// token's hash is stored; the link is shown once to whoever created it (or,
// once an email sender exists, mailed and marked with SentAt).
type Invite struct {
	ID             string
	OrgID          string
	Email          string
	Role           string
	TokenHash      string
	InvitedByEmail string
	InvitedByName  string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	SentAt         time.Time // zero: delivered by hand (copied link)
	AcceptedAt     time.Time
	RevokedAt      time.Time
}

// Invite states.
const (
	InviteOpen     = "open"
	InviteExpired  = "expired"
	InviteAccepted = "accepted"
	InviteRevoked  = "revoked"
)

// State is the invite's state at now.
func (i *Invite) State(now time.Time) string {
	switch {
	case !i.AcceptedAt.IsZero():
		return InviteAccepted
	case !i.RevokedAt.IsZero():
		return InviteRevoked
	case !now.Before(i.ExpiresAt):
		return InviteExpired
	}
	return InviteOpen
}

// Err is the error accepting the invite at now would fail with, or nil.
func (i *Invite) Err(now time.Time) error {
	switch i.State(now) {
	case InviteAccepted:
		return ErrInviteUsed
	case InviteRevoked:
		return ErrInviteRevoked
	case InviteExpired:
		return ErrInviteExpired
	}
	return nil
}

const inviteColumns = `id, org_id, email, role, token_hash, invited_by_email, invited_by_name, created_at, expires_at,
	COALESCE(sent_at, 0), COALESCE(accepted_at, 0), COALESCE(revoked_at, 0)`

func scanInvite(row interface{ Scan(...any) error }) (*Invite, error) {
	var i Invite
	var created, expires, sent, accepted, revoked int64
	if err := row.Scan(&i.ID, &i.OrgID, &i.Email, &i.Role, &i.TokenHash, &i.InvitedByEmail, &i.InvitedByName,
		&created, &expires, &sent, &accepted, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	i.CreatedAt, i.ExpiresAt = time.Unix(created, 0), time.Unix(expires, 0)
	unix := func(v int64) time.Time {
		if v == 0 {
			return time.Time{}
		}
		return time.Unix(v, 0)
	}
	i.SentAt, i.AcceptedAt, i.RevokedAt = unix(sent), unix(accepted), unix(revoked)
	return &i, nil
}

// CreateInvite stores a new invite. It fails with ErrEmailTaken if the email
// already has an account and with ErrInvitePending if an unexpired invite for
// it is open; an expired open one is revoked and replaced.
func (s *Store) CreateInvite(ctx context.Context, inv *Invite) error {
	inv.ID = newID()
	if inv.OrgID == "" {
		inv.OrgID = DefaultOrg
	}
	inv.Email = strings.TrimSpace(inv.Email)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE email = ?`, inv.Email).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrEmailTaken
	}
	open, err := scanInvite(tx.QueryRowContext(ctx, `SELECT `+inviteColumns+` FROM invites
		WHERE email = ? AND accepted_at IS NULL AND revoked_at IS NULL`, inv.Email))
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return err
	case open.State(inv.CreatedAt) == InviteOpen:
		return ErrInvitePending
	default:
		if _, err := tx.ExecContext(ctx, `UPDATE invites SET revoked_at = ? WHERE id = ?`, inv.CreatedAt.Unix(), open.ID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO invites
		(id, org_id, email, role, token_hash, invited_by_email, invited_by_name, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inv.ID, inv.OrgID, inv.Email, inv.Role, inv.TokenHash, inv.InvitedByEmail, inv.InvitedByName,
		inv.CreatedAt.Unix(), inv.ExpiresAt.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// OpenInvites lists invites that were neither accepted nor revoked, expired
// ones included, newest first.
func (s *Store) OpenInvites(ctx context.Context) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+inviteColumns+` FROM invites
		WHERE accepted_at IS NULL AND revoked_at IS NULL ORDER BY created_at DESC, rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		i, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *i)
	}
	return out, rows.Err()
}

func (s *Store) InviteByID(ctx context.Context, id string) (*Invite, error) {
	return scanInvite(s.db.QueryRowContext(ctx, `SELECT `+inviteColumns+` FROM invites WHERE id = ?`, id))
}

// InviteByTokenHash finds an invite in any state; see Invite.Err.
func (s *Store) InviteByTokenHash(ctx context.Context, hash string) (*Invite, error) {
	return scanInvite(s.db.QueryRowContext(ctx, `SELECT `+inviteColumns+` FROM invites WHERE token_hash = ?`, hash))
}

// ReissueInvite gives an open (possibly expired) invite a new token and
// expiry; the old link stops working. check works as in SetRole.
func (s *Store) ReissueInvite(ctx context.Context, id, tokenHash string, created, expires time.Time, check func(*Invite) error) (*Invite, error) {
	return s.changeOpenInvite(ctx, id, check, `UPDATE invites SET token_hash = ?, created_at = ?, expires_at = ?, sent_at = NULL WHERE id = ?`,
		tokenHash, created.Unix(), expires.Unix(), id)
}

// RevokeInvite withdraws an open invite. check works as in SetRole.
func (s *Store) RevokeInvite(ctx context.Context, id string, now time.Time, check func(*Invite) error) (*Invite, error) {
	return s.changeOpenInvite(ctx, id, check, `UPDATE invites SET revoked_at = ? WHERE id = ?`, now.Unix(), id)
}

func (s *Store) changeOpenInvite(ctx context.Context, id string, check func(*Invite) error, query string, args ...any) (*Invite, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	inv, err := scanInvite(tx.QueryRowContext(ctx, `SELECT `+inviteColumns+` FROM invites
		WHERE id = ? AND accepted_at IS NULL AND revoked_at IS NULL`, id))
	if err != nil {
		return nil, err
	}
	if check != nil {
		if err := check(inv); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return nil, err
	}
	return inv, tx.Commit()
}

// MarkInviteSent records that an email sender delivered the invite. Nothing
// calls it until such a sender exists (see server.InviteSender).
func (s *Store) MarkInviteSent(ctx context.Context, id string, at time.Time) error {
	return s.found(s.db.ExecContext(ctx, `UPDATE invites SET sent_at = ? WHERE id = ?`, at.Unix(), id))
}

// AcceptInvite consumes the open invite with this token hash and creates its
// user (email and role from the invite; name and password hash from u), in one
// transaction, so an invite works exactly once. It fails with ErrNotFound for
// an unknown token, the Invite.Err errors for a spent one and ErrEmailTaken if
// the email got an account in the meantime.
func (s *Store) AcceptInvite(ctx context.Context, tokenHash string, u *User, now time.Time) (*Invite, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	inv, err := scanInvite(tx.QueryRowContext(ctx, `SELECT `+inviteColumns+` FROM invites WHERE token_hash = ?`, tokenHash))
	if err != nil {
		return nil, err
	}
	if err := inv.Err(now); err != nil {
		return inv, err
	}
	u.Email, u.Role, u.OrgID, u.CreatedAt = inv.Email, inv.Role, inv.OrgID, now
	if err := insertUser(ctx, tx, u); err != nil {
		if errors.Is(err, ErrEmailTaken) {
			// Of two racing accepts of this invite, the other one created
			// the account (SQLite serialises the writes): say the invite
			// is used, not that the address has an account.
			_ = tx.Rollback()
			if again, err2 := scanInvite(s.db.QueryRowContext(ctx, `SELECT `+inviteColumns+` FROM invites WHERE token_hash = ?`, tokenHash)); err2 == nil {
				if used := again.Err(now); used != nil {
					return again, used
				}
			}
		}
		return inv, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE invites SET accepted_at = ?, accepted_user_id = ? WHERE id = ?`, now.Unix(), u.ID, inv.ID); err != nil {
		return nil, err
	}
	return inv, tx.Commit()
}

// ---- audit -----------------------------------------------------------------

// AuditQuery filters and pages the audit log, newest first.
type AuditQuery struct {
	Actor  string // exact, empty for all
	Action string // exact, or a prefix when it ends in "." (e.g. "member.")
	Before int64  // only entries with a smaller ID: the cursor of the previous page
	Limit  int
}

// AuditPage is one page of entries and the cursor for the next (0: no more).
type AuditPage struct {
	Entries []AuditRecord
	Next    int64
}

// AuditRecord is an audit entry with its ID, the paging cursor.
type AuditRecord struct {
	ID int64
	AuditEntry
}

func (s *Store) QueryAudit(ctx context.Context, q AuditQuery) (AuditPage, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	var where []string
	var args []any
	if q.Actor != "" {
		where = append(where, "actor = ?")
		args = append(args, q.Actor)
	}
	if q.Action != "" {
		if strings.HasSuffix(q.Action, ".") {
			// A range, not LIKE: action names contain "_", which LIKE treats as a wildcard.
			where = append(where, "action >= ? AND action < ?")
			args = append(args, q.Action, q.Action[:len(q.Action)-1]+"/") // "/" sorts right after "."
		} else {
			where = append(where, "action = ?")
			args = append(args, q.Action)
		}
	}
	if q.Before > 0 {
		where = append(where, "id < ?")
		args = append(args, q.Before)
	}
	query := `SELECT id, at, actor, action, target, ip, detail FROM audit`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, q.Limit+1) // one more tells whether a next page exists
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return AuditPage{}, err
	}
	defer rows.Close()
	var page AuditPage
	for rows.Next() {
		var e AuditRecord
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.Target, &e.IP, &e.Detail); err != nil {
			return AuditPage{}, err
		}
		e.At = time.Unix(at, 0)
		page.Entries = append(page.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return AuditPage{}, err
	}
	if len(page.Entries) > q.Limit {
		page.Entries = page.Entries[:q.Limit]
		page.Next = page.Entries[q.Limit-1].ID
	}
	return page, nil
}

// AuditFacets lists the distinct actors and actions in the log, for filters.
func (s *Store) AuditFacets(ctx context.Context) (actors, actions []string, err error) {
	list := func(col string) ([]string, error) {
		rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT `+col+` FROM audit ORDER BY `+col)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, rows.Err()
	}
	if actors, err = list("actor"); err != nil {
		return nil, nil, err
	}
	actions, err = list("action")
	return actors, actions, err
}
