// Package store keeps the console's own data — users, sessions and the audit
// log — in SQLite. Everything about workloads lives in Kubernetes instead.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, static binary
)

var (
	ErrNotFound      = errors.New("not found")
	ErrEmailTaken    = errors.New("email already in use")
	ErrSetupComplete = errors.New("setup is already complete")
)

// DefaultOrg is the single organization of v1. Records carry it so
// multi-team tenancy can be added later without a migration.
const DefaultOrg = "default"

// Roles, in decreasing power. They map to Kubernetes RBAC in Phase 4.
const (
	RoleOwner     = "owner"
	RoleAdmin     = "admin"
	RoleDeveloper = "developer"
	RoleViewer    = "viewer"
)

type User struct {
	ID           string
	OrgID        string
	Email        string
	Name         string
	PasswordHash string
	Role         string
	CreatedAt    time.Time
}

type Session struct {
	IDHash     string // sha256 of the cookie token; the token itself is never stored
	UserID     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	IP         string
	UserAgent  string
}

type AuditEntry struct {
	At     time.Time
	Actor  string // user email, "setup" or "anonymous"
	Action string // e.g. setup.owner_created, session.login_failed
	Target string
	IP     string
	Detail string
}

type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and migrates it.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One writer at a time is plenty for a console and avoids SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Snapshot writes a consistent copy of the database to path (VACUUM INTO),
// while the console keeps serving: what upgrades keep for a rollback and
// backups carry (docs/phase6.md). path must not exist yet.
func (s *Store) Snapshot(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("snapshot %s: file exists", path)
	}
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

// migrations run in order; append only, never edit a released one.
var migrations = []string{
	`CREATE TABLE users (
		id            TEXT PRIMARY KEY,
		org_id        TEXT NOT NULL,
		email         TEXT NOT NULL UNIQUE COLLATE NOCASE,
		name          TEXT NOT NULL,
		password_hash TEXT NOT NULL,
		role          TEXT NOT NULL CHECK (role IN ('owner','admin','developer','viewer')),
		created_at    INTEGER NOT NULL
	);
	CREATE TABLE sessions (
		id_hash      TEXT PRIMARY KEY,
		user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		created_at   INTEGER NOT NULL,
		expires_at   INTEGER NOT NULL,
		last_seen_at INTEGER NOT NULL,
		ip           TEXT NOT NULL,
		user_agent   TEXT NOT NULL
	);
	CREATE INDEX sessions_user ON sessions(user_id);
	CREATE TABLE audit (
		id     INTEGER PRIMARY KEY AUTOINCREMENT,
		at     INTEGER NOT NULL,
		actor  TEXT NOT NULL,
		action TEXT NOT NULL,
		target TEXT NOT NULL,
		ip     TEXT NOT NULL,
		detail TEXT NOT NULL
	);`,
	// Second factors (see mfa.go). TOTP secrets are sealed with the data key;
	// last_step is the newest accepted time step, so a code works only once.
	`CREATE TABLE totp (
		user_id    TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
		secret     TEXT NOT NULL,
		confirmed  INTEGER NOT NULL DEFAULT 0,
		last_step  INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE recovery_codes (
		user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		code_hash TEXT NOT NULL,
		used_at   INTEGER,
		PRIMARY KEY (user_id, code_hash)
	);
	CREATE TABLE passkeys (
		id            TEXT PRIMARY KEY,
		user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		credential_id BLOB NOT NULL UNIQUE,
		name          TEXT NOT NULL,
		credential    TEXT NOT NULL,
		created_at    INTEGER NOT NULL,
		last_used_at  INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX passkeys_user ON passkeys(user_id);`,
	// Invites and audit filters (see members.go). An invite is open while
	// accepted_at and revoked_at are NULL; at most one open invite per email.
	// sent_at stays NULL until an email sender delivers invites.
	`CREATE TABLE invites (
		id               TEXT PRIMARY KEY,
		org_id           TEXT NOT NULL,
		email            TEXT NOT NULL COLLATE NOCASE,
		role             TEXT NOT NULL CHECK (role IN ('owner','admin','developer','viewer')),
		token_hash       TEXT NOT NULL UNIQUE,
		invited_by_email TEXT NOT NULL,
		invited_by_name  TEXT NOT NULL,
		created_at       INTEGER NOT NULL,
		expires_at       INTEGER NOT NULL,
		sent_at          INTEGER,
		accepted_at      INTEGER,
		accepted_user_id TEXT,
		revoked_at       INTEGER
	);
	CREATE UNIQUE INDEX invites_open_email ON invites(email) WHERE accepted_at IS NULL AND revoked_at IS NULL;
	CREATE INDEX audit_actor ON audit(actor, id);
	CREATE INDEX audit_action ON audit(action, id);`,
	// Phase 4 identity (see tokens.go, identities.go). API tokens are stored
	// as the SHA-256 of the token; role is the cap (never above the user's
	// role at use), projects a JSON array ('' = every project the user
	// reaches). user_identities links single sign-on subjects to users.
	`CREATE TABLE api_tokens (
		id           TEXT PRIMARY KEY,
		user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name         TEXT NOT NULL,
		kind         TEXT NOT NULL CHECK (kind IN ('api','kubeconfig')),
		token_hash   TEXT NOT NULL UNIQUE,
		hint         TEXT NOT NULL,
		role         TEXT NOT NULL CHECK (role IN ('owner','admin','developer','viewer')),
		projects     TEXT NOT NULL DEFAULT '',
		created_at   INTEGER NOT NULL,
		expires_at   INTEGER NOT NULL,
		last_used_at INTEGER NOT NULL DEFAULT 0,
		last_used_ip TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX api_tokens_user ON api_tokens(user_id);
	CREATE TABLE user_identities (
		issuer        TEXT NOT NULL,
		subject       TEXT NOT NULL,
		user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		email         TEXT NOT NULL,
		created_at    INTEGER NOT NULL,
		last_login_at INTEGER NOT NULL,
		PRIMARY KEY (issuer, subject)
	);
	CREATE UNIQUE INDEX user_identities_user ON user_identities(user_id, issuer);`,
	// Console-wide settings that are about sign-in rather than the cluster,
	// such as "require two-factor sign-in" (see policy.go).
	`CREATE TABLE settings (
		org_id TEXT NOT NULL,
		key    TEXT NOT NULL,
		value  TEXT NOT NULL,
		PRIMARY KEY (org_id, key)
	);`,
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, i+1); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CountUsers reports how many users exist; zero means setup is pending.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateOwner creates the first user, atomically: if any user exists it
// returns ErrSetupComplete, so two racing setup requests cannot both win.
func (s *Store) CreateOwner(ctx context.Context, u *User) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrSetupComplete
	}
	u.Role = RoleOwner
	if err := insertUser(ctx, tx, u); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateUser adds a user with the given role.
func (s *Store) CreateUser(ctx context.Context, u *User) error {
	return insertUser(ctx, s.db, u)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertUser(ctx context.Context, db execer, u *User) error {
	u.ID = newID()
	if u.OrgID == "" {
		u.OrgID = DefaultOrg
	}
	u.Email = strings.TrimSpace(u.Email)
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO users (id, org_id, email, name, password_hash, role, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.OrgID, u.Email, u.Name, u.PasswordHash, u.Role, u.CreatedAt.Unix())
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: users.email") {
		return ErrEmailTaken
	}
	return err
}

const userColumns = `id, org_id, email, name, password_hash, role, created_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var created int64
	if err := row.Scan(&u.ID, &u.OrgID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.CreatedAt = time.Unix(created, 0)
	return &u, nil
}

func (s *Store) UserByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE email = ?`, strings.TrimSpace(email)))
}

func (s *Store) UserByID(ctx context.Context, id string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id_hash, user_id, created_at, expires_at, last_seen_at, ip, user_agent) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sess.IDHash, sess.UserID, sess.CreatedAt.Unix(), sess.ExpiresAt.Unix(), sess.LastSeenAt.Unix(), sess.IP, sess.UserAgent)
	return err
}

// SessionByHash returns an unexpired session and its user.
func (s *Store) SessionByHash(ctx context.Context, idHash string, now time.Time) (*Session, *User, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT s.id_hash, s.user_id, s.created_at, s.expires_at, s.last_seen_at, s.ip, s.user_agent,
		       u.id, u.org_id, u.email, u.name, u.password_hash, u.role, u.created_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id_hash = ? AND s.expires_at > ?`, idHash, now.Unix())
	var sess Session
	var u User
	var sc, se, sl, uc int64
	err := row.Scan(&sess.IDHash, &sess.UserID, &sc, &se, &sl, &sess.IP, &sess.UserAgent,
		&u.ID, &u.OrgID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, &uc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	sess.CreatedAt, sess.ExpiresAt, sess.LastSeenAt = time.Unix(sc, 0), time.Unix(se, 0), time.Unix(sl, 0)
	u.CreatedAt = time.Unix(uc, 0)
	return &sess, &u, nil
}

// TouchSession records activity and slides the expiry.
func (s *Store) TouchSession(ctx context.Context, idHash string, seen, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id_hash = ?`,
		seen.Unix(), expires.Unix(), idHash)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, idHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, idHash)
	return err
}

// DeleteExpiredSessions removes sessions that ended before now.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Audit appends to the append-only audit log.
func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit (at, actor, action, target, ip, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		e.At.Unix(), e.Actor, e.Action, e.Target, e.IP, e.Detail)
	return err
}

// RecentAudit returns the newest entries first.
func (s *Store) RecentAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, actor, action, target, ip, detail FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		if err := rows.Scan(&at, &e.Actor, &e.Action, &e.Target, &e.IP, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}
