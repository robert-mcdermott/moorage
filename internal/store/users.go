package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Server roles. Until projects arrive, a member may do everything with apps,
// and only an admin manages users.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// ErrEmailTaken is returned for an email address that already has an account.
var ErrEmailTaken = errors.New("that email address already has an account")

// ErrLastAdmin is returned for a change that would leave no enabled admin.
var ErrLastAdmin = errors.New("this is the last admin: make another user an admin first")

// User is an account.
type User struct {
	ID    string
	Email string
	Name  string
	// PasswordHash is empty until the user accepts their invitation.
	PasswordHash string
	Role         string
	DisabledAt   time.Time
	CreatedAt    time.Time
}

// Disabled reports whether the user may no longer sign in.
func (u User) Disabled() bool { return !u.DisabledAt.IsZero() }

const userColumns = `id, email, name, password_hash, role, disabled_at, created_at`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var hash sql.NullString
	var disabled sql.NullInt64
	var created int64
	if err := row.Scan(&u.ID, &u.Email, &u.Name, &hash, &u.Role, &disabled, &created); err != nil {
		return User{}, err
	}
	u.PasswordHash = hash.String
	u.CreatedAt = time.UnixMilli(created).UTC()
	if disabled.Valid {
		u.DisabledAt = time.UnixMilli(disabled.Int64).UTC()
	}
	return u, nil
}

// CreateUser adds an account.
func (db *DB) CreateUser(ctx context.Context, u User) error {
	return db.write(ctx, func(tx *sql.Tx) error { return createUser(ctx, tx, u) })
}

func createUser(ctx context.Context, tx *sql.Tx, u User) error {
	var existing string
	err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE email = ?`, u.Email).Scan(&existing)
	if err == nil {
		return ErrEmailTaken
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO users (id, email, name, password_hash, role, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		u.ID, u.Email, u.Name, nullString(u.PasswordHash), u.Role, u.CreatedAt.UnixMilli())
	return err
}

// ClaimServer adds the server's first user. It fails with ErrAlreadyClaimed
// once there is any user, so that two claims cannot both succeed.
func (db *DB) ClaimServer(ctx context.Context, u User) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrAlreadyClaimed
		}
		return createUser(ctx, tx, u)
	})
}

// ErrAlreadyClaimed is returned when a server that has users is claimed.
var ErrAlreadyClaimed = errors.New("this server has been claimed")

// CountUsers returns how many accounts there are.
func (db *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := db.Reader.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

// UserByID returns a user, or ErrNotFound.
func (db *DB) UserByID(ctx context.Context, id string) (User, error) {
	u, err := scanUser(db.Reader.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// UserByEmail returns a user, or ErrNotFound.
func (db *DB) UserByEmail(ctx context.Context, email string) (User, error) {
	u, err := scanUser(db.Reader.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE email = ?`, email))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// Users lists up to limit accounts by email, after the email address after.
func (db *DB) Users(ctx context.Context, after string, limit int) ([]User, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+userColumns+` FROM users WHERE email > ? ORDER BY email LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// lastAdmin reports whether id is the only enabled admin.
func lastAdmin(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var others int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role = 'admin' AND disabled_at IS NULL AND id != ?`, id).Scan(&others)
	return others == 0, err
}

// SetUserRole changes a user's role. An enabled admin who is the last one
// stays an admin.
func (db *DB) SetUserRole(ctx context.Context, id, role string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if u.Role == RoleAdmin && role != RoleAdmin && !u.Disabled() {
			last, err := lastAdmin(ctx, tx, id)
			if err != nil {
				return err
			}
			if last {
				return ErrLastAdmin
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, role, id)
		return err
	})
}

// SetUserDisabled disables a user, ending their sessions, or enables them
// again. Their tokens and keys stop working while they are disabled. The last
// enabled admin cannot be disabled.
func (db *DB) SetUserDisabled(ctx context.Context, id string, disabled bool) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !disabled {
			_, err = tx.ExecContext(ctx, `UPDATE users SET disabled_at = NULL WHERE id = ?`, id)
			return err
		}
		if u.Role == RoleAdmin && !u.Disabled() {
			last, err := lastAdmin(ctx, tx, id)
			if err != nil {
				return err
			}
			if last {
				return ErrLastAdmin
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET disabled_at = ? WHERE id = ?`, time.Now().UnixMilli(), id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id)
		return err
	})
}

// Session is a browser's sign-in.
type Session struct {
	ID         string
	IDHash     string
	UserID     string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	IP         string
	UserAgent  string
}

const sessionColumns = `id, id_hash, user_id, created_at, last_seen_at, expires_at, ip, user_agent`

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var s Session
	var created, seen, expires int64
	var ip, agent sql.NullString
	if err := row.Scan(&s.ID, &s.IDHash, &s.UserID, &created, &seen, &expires, &ip, &agent); err != nil {
		return Session{}, err
	}
	s.CreatedAt, s.LastSeenAt, s.ExpiresAt = time.UnixMilli(created).UTC(), time.UnixMilli(seen).UTC(), time.UnixMilli(expires).UTC()
	s.IP, s.UserAgent = ip.String, agent.String
	return s, nil
}

// CreateSession records a sign-in, and removes sessions that have expired.
func (db *DB) CreateSession(ctx context.Context, s Session) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, time.Now().UnixMilli()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, id_hash, user_id, created_at, last_seen_at, expires_at, ip, user_agent)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, s.ID, s.IDHash, s.UserID, s.CreatedAt.UnixMilli(), s.LastSeenAt.UnixMilli(), s.ExpiresAt.UnixMilli(),
			nullString(s.IP), nullString(s.UserAgent))
		return err
	})
}

// SessionByHash returns a session, or ErrNotFound.
func (db *DB) SessionByHash(ctx context.Context, hash string) (Session, error) {
	s, err := scanSession(db.Reader.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id_hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return s, err
}

// TouchSession records that a session was just used.
func (db *DB) TouchSession(ctx context.Context, id string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id = ?`, time.Now().UnixMilli(), id)
		return err
	})
}

// UserSessions lists a user's sessions, the most recently used first.
func (db *DB) UserSessions(ctx context.Context, userID string) ([]Session, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE user_id = ? AND expires_at >= ? ORDER BY last_seen_at DESC, id`,
		userID, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteSession ends one of a user's sessions; ErrNotFound if it has none
// with that ID.
func (db *DB) DeleteSession(ctx context.Context, userID, id string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ? AND user_id = ?`, id, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteSessionByHash ends a session, if it exists.
func (db *DB) DeleteSessionByHash(ctx context.Context, hash string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, hash)
		return err
	})
}

// APIToken is a token for the API and for git over HTTPS.
type APIToken struct {
	ID string
	// UserID is empty for a token of the server's local administrator.
	UserID     string
	Name       string
	TokenHash  string
	Scopes     []string
	App        string // the app it is restricted to, if any
	ExpiresAt  time.Time
	LastUsedAt time.Time
	CreatedAt  time.Time
}

// ErrTokenNameTaken is returned for a token name its owner already uses.
var ErrTokenNameTaken = errors.New("you already have a token with that name")

const tokenColumns = `t.id, t.user_id, t.name, t.token_hash, t.scopes, a.name, t.expires_at, t.last_used_at, t.created_at
	FROM api_tokens t LEFT JOIN apps a ON a.id = t.app_id`

func scanToken(row interface{ Scan(...any) error }) (APIToken, error) {
	var t APIToken
	var user, app sql.NullString
	var scopes string
	var expires, used sql.NullInt64
	var created int64
	if err := row.Scan(&t.ID, &user, &t.Name, &t.TokenHash, &scopes, &app, &expires, &used, &created); err != nil {
		return APIToken{}, err
	}
	t.UserID, t.App = user.String, app.String
	if err := json.Unmarshal([]byte(scopes), &t.Scopes); err != nil {
		return APIToken{}, fmt.Errorf("token %s: %w", t.ID, err)
	}
	t.CreatedAt = time.UnixMilli(created).UTC()
	if expires.Valid {
		t.ExpiresAt = time.UnixMilli(expires.Int64).UTC()
	}
	if used.Valid {
		t.LastUsedAt = time.UnixMilli(used.Int64).UTC()
	}
	return t, nil
}

// CreateToken records a token. A restriction to an app that does not exist
// fails with ErrNotFound.
func (db *DB) CreateToken(ctx context.Context, t APIToken) error {
	scopes, err := json.Marshal(t.Scopes)
	if err != nil {
		return err
	}
	return db.write(ctx, func(tx *sql.Tx) error {
		var taken int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM api_tokens WHERE coalesce(user_id, '') = ? AND name = ?`, t.UserID, t.Name).Scan(&taken); err != nil {
			return err
		}
		if taken > 0 {
			return ErrTokenNameTaken
		}
		var appID any
		if t.App != "" {
			var id string
			err := tx.QueryRowContext(ctx, `SELECT id FROM apps WHERE name = ?`, t.App).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("no app named %q: %w", t.App, ErrNotFound)
			}
			if err != nil {
				return err
			}
			appID = id
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO api_tokens (id, user_id, name, token_hash, scopes, app_id, expires_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, t.ID, nullString(t.UserID), t.Name, t.TokenHash, string(scopes), appID, nullTime(t.ExpiresAt), t.CreatedAt.UnixMilli())
		return err
	})
}

// TokenByHash returns a token, or ErrNotFound.
func (db *DB) TokenByHash(ctx context.Context, hash string) (APIToken, error) {
	t, err := scanToken(db.Reader.QueryRowContext(ctx, `SELECT `+tokenColumns+` WHERE t.token_hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return APIToken{}, ErrNotFound
	}
	return t, err
}

// Tokens lists the tokens of a user (or, with "", of the local administrator).
func (db *DB) Tokens(ctx context.Context, userID string) ([]APIToken, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+tokenColumns+` WHERE coalesce(t.user_id, '') = ? ORDER BY t.created_at, t.name`, userID)
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
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteToken revokes the owner's token with this name or ID, and returns it;
// ErrNotFound if they have none.
func (db *DB) DeleteToken(ctx context.Context, userID, which string) (APIToken, error) {
	var t APIToken
	err := db.write(ctx, func(tx *sql.Tx) error {
		var err error
		t, err = scanToken(tx.QueryRowContext(ctx, `SELECT `+tokenColumns+` WHERE coalesce(t.user_id, '') = ? AND (t.name = ? OR t.id = ?)`, userID, which, which))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ?`, t.ID)
		return err
	})
	return t, err
}

// TouchToken records that a token was just used.
func (db *DB) TouchToken(ctx context.Context, id string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, time.Now().UnixMilli(), id)
		return err
	})
}

// DeviceCode is a CLI's sign-in, waiting for approval in a browser.
type DeviceCode struct {
	CodeHash   string
	UserCode   string
	Client     string
	IP         string
	ApprovedBy string
	Denied     bool
	LastPollAt time.Time
	ExpiresAt  time.Time
}

const deviceColumns = `code_hash, user_code, client, ip, approved_by, denied, last_poll_at, expires_at`

func scanDevice(row interface{ Scan(...any) error }) (DeviceCode, error) {
	var d DeviceCode
	var ip, approved sql.NullString
	var denied int
	var polled sql.NullInt64
	var expires int64
	if err := row.Scan(&d.CodeHash, &d.UserCode, &d.Client, &ip, &approved, &denied, &polled, &expires); err != nil {
		return DeviceCode{}, err
	}
	d.IP, d.ApprovedBy, d.Denied = ip.String, approved.String, denied != 0
	d.ExpiresAt = time.UnixMilli(expires).UTC()
	if polled.Valid {
		d.LastPollAt = time.UnixMilli(polled.Int64).UTC()
	}
	return d, nil
}

// CreateDeviceCode records a CLI's sign-in, and removes those that expired.
func (db *DB) CreateDeviceCode(ctx context.Context, d DeviceCode) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM device_codes WHERE expires_at < ?`, time.Now().UnixMilli()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO device_codes (code_hash, user_code, client, ip, expires_at) VALUES (?, ?, ?, ?, ?)`,
			d.CodeHash, d.UserCode, d.Client, nullString(d.IP), d.ExpiresAt.UnixMilli())
		return err
	})
}

// DeviceByUserCode returns the unexpired sign-in with this code, or ErrNotFound.
func (db *DB) DeviceByUserCode(ctx context.Context, code string) (DeviceCode, error) {
	d, err := scanDevice(db.Reader.QueryRowContext(ctx, `SELECT `+deviceColumns+` FROM device_codes WHERE user_code = ? AND expires_at >= ?`, code, time.Now().UnixMilli()))
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceCode{}, ErrNotFound
	}
	return d, err
}

// PollDevice returns a sign-in by its secret code, as it was before this poll,
// and records the poll; ErrNotFound if there is none.
func (db *DB) PollDevice(ctx context.Context, hash string) (DeviceCode, error) {
	var d DeviceCode
	err := db.write(ctx, func(tx *sql.Tx) error {
		var err error
		d, err = scanDevice(tx.QueryRowContext(ctx, `SELECT `+deviceColumns+` FROM device_codes WHERE code_hash = ?`, hash))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE device_codes SET last_poll_at = ? WHERE code_hash = ?`, time.Now().UnixMilli(), hash)
		return err
	})
	return d, err
}

// DecideDevice approves a waiting sign-in for a user, or denies it (userID
// empty); ErrNotFound if it has expired or was already decided.
func (db *DB) DecideDevice(ctx context.Context, code, userID string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE device_codes SET approved_by = ?, denied = ?
			WHERE user_code = ? AND expires_at >= ? AND approved_by IS NULL AND denied = 0`,
			nullString(userID), userID == "", code, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RedeemDevice removes an approved sign-in and creates its token, atomically,
// so that the code gives one token only; ErrNotFound if it was redeemed.
func (db *DB) RedeemDevice(ctx context.Context, hash string, t APIToken) error {
	scopes, err := json.Marshal(t.Scopes)
	if err != nil {
		return err
	}
	return db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM device_codes WHERE code_hash = ? AND approved_by IS NOT NULL`, hash)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO api_tokens (id, user_id, name, token_hash, scopes, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			t.ID, nullString(t.UserID), t.Name, t.TokenHash, string(scopes), t.CreatedAt.UnixMilli())
		return err
	})
}

// DeleteDevice forgets a sign-in.
func (db *DB) DeleteDevice(ctx context.Context, hash string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM device_codes WHERE code_hash = ?`, hash)
		return err
	})
}

// CreateInvitation records a one-time link for a user to choose a password,
// replacing any earlier one of theirs.
func (db *DB) CreateInvitation(ctx context.Context, hash, userID, by string, expires time.Time) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM invitations WHERE user_id = ? OR expires_at < ?`, userID, time.Now().UnixMilli()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO invitations (code_hash, user_id, created_by, expires_at) VALUES (?, ?, ?, ?)`,
			hash, userID, nullString(by), expires.UnixMilli())
		return err
	})
}

// InvitedUser returns the user a valid invitation is for, or ErrNotFound.
func (db *DB) InvitedUser(ctx context.Context, hash string) (User, error) {
	u, err := scanUser(db.Reader.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id =
		(SELECT user_id FROM invitations WHERE code_hash = ? AND expires_at >= ?)`, hash, time.Now().UnixMilli()))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// AcceptInvitation uses an invitation once: it sets the user's password and
// ends their sessions. ErrNotFound if it was already used or has expired.
func (db *DB) AcceptInvitation(ctx context.Context, hash, passwordHash string) (User, error) {
	var u User
	err := db.write(ctx, func(tx *sql.Tx) error {
		var userID string
		err := tx.QueryRowContext(ctx, `DELETE FROM invitations WHERE code_hash = ? AND expires_at >= ? RETURNING user_id`, hash, time.Now().UnixMilli()).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
			return err
		}
		u, err = scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, userID))
		return err
	})
	return u, err
}
