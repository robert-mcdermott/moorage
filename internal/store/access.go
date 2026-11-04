package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EnvAccess is what has been set of who may reach an environment. A Mode that is empty
// leaves it to the manifest, and then to the server.
type EnvAccess struct {
	Mode string
	// PasswordHash is the password gate's password, hashed.
	PasswordHash string
	// Allow lists the addresses it may be reached from, as CIDR prefixes; none is anywhere.
	Allow []string
	// AnyUser lets in anyone with an account, not only those with a role in its project.
	AnyUser bool
	// Generation changes when those who are in should be in no more.
	Generation int
}

func splitAllow(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// EnvironmentAccess returns what has been set of who may reach an environment.
func (db *DB) EnvironmentAccess(ctx context.Context, app, env string) (EnvAccess, error) {
	var a EnvAccess
	var mode, password, allow sql.NullString
	var anyUser int
	err := db.Reader.QueryRowContext(ctx, `SELECT e.access_mode, e.access_password, e.access_allow, e.access_any_user, e.access_generation
		FROM environments e JOIN apps ap ON ap.id = e.app_id WHERE ap.name = ? AND e.name = ?`, app, env).Scan(&mode, &password, &allow, &anyUser, &a.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("no environment %q of app %q: %w", env, app, ErrNotFound)
	}
	a.Mode, a.PasswordHash, a.Allow, a.AnyUser = mode.String, password.String, splitAllow(allow.String), anyUser != 0
	return a, err
}

// SetEnvironmentAccess changes who may reach an environment. If the change would leave someone
// in who should no longer be, the generation moves, and every gate cookie ends with it.
func (db *DB) SetEnvironmentAccess(ctx context.Context, app, env string, a EnvAccess) error {
	before, err := db.EnvironmentAccess(ctx, app, env)
	if err != nil {
		return err
	}
	generation := before.Generation
	if a.Mode != before.Mode || a.PasswordHash != before.PasswordHash || before.AnyUser && !a.AnyUser {
		generation++
	}
	anyUser := 0
	if a.AnyUser {
		anyUser = 1
	}
	return db.updateEnvironment(ctx, app, env, `access_mode = ?, access_password = ?, access_allow = ?, access_any_user = ?, access_generation = ?`,
		nullable(a.Mode), nullable(a.PasswordHash), nullable(strings.Join(a.Allow, ",")), anyUser, generation)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ShareLink lets whoever holds it reach an environment, whatever its gate.
type ShareLink struct {
	ID, App, Env, EnvID string
	CreatedBy           string
	CreatedAt           time.Time
	ExpiresAt           time.Time
	// MaxUses is how many times it may be redeemed; 0 is without limit.
	MaxUses, Uses int
	LastUsedAt    time.Time
	RevokedAt     time.Time
}

// Active says whether a link can still be used to stay in. Uses are counted when it is redeemed.
func (l ShareLink) Active(now time.Time) bool {
	return l.RevokedAt.IsZero() && now.Before(l.ExpiresAt)
}

// CreateShareLink keeps a share link, of which only the hash of the token is kept.
func (db *DB) CreateShareLink(ctx context.Context, app, env string, l ShareLink, tokenHash string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		var envID string
		err := tx.QueryRowContext(ctx, `SELECT e.id FROM environments e JOIN apps a ON a.id = e.app_id WHERE a.name = ? AND e.name = ?`, app, env).Scan(&envID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no environment %q of app %q: %w", env, app, ErrNotFound)
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO share_links (id, env_id, token_hash, created_by, created_at, expires_at, max_uses) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			l.ID, envID, tokenHash, l.CreatedBy, l.CreatedAt.UnixMilli(), l.ExpiresAt.UnixMilli(), l.MaxUses)
		return err
	})
}

const shareLinkColumns = `l.id, a.name, e.name, e.id, l.created_by, l.created_at, l.expires_at, l.max_uses, l.uses, l.last_used_at, l.revoked_at
	FROM share_links l JOIN environments e ON e.id = l.env_id JOIN apps a ON a.id = e.app_id`

func scanShareLink(row interface{ Scan(...any) error }) (ShareLink, error) {
	var l ShareLink
	var created, expires int64
	var used, revoked sql.NullInt64
	if err := row.Scan(&l.ID, &l.App, &l.Env, &l.EnvID, &l.CreatedBy, &created, &expires, &l.MaxUses, &l.Uses, &used, &revoked); err != nil {
		return l, err
	}
	l.CreatedAt, l.ExpiresAt = time.UnixMilli(created).UTC(), time.UnixMilli(expires).UTC()
	if used.Valid {
		l.LastUsedAt = time.UnixMilli(used.Int64).UTC()
	}
	if revoked.Valid {
		l.RevokedAt = time.UnixMilli(revoked.Int64).UTC()
	}
	return l, nil
}

// ShareLinks lists an environment's links, newest first.
func (db *DB) ShareLinks(ctx context.Context, app, env string) ([]ShareLink, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+shareLinkColumns+` WHERE a.name = ? AND e.name = ? ORDER BY l.created_at DESC, l.id DESC`, app, env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShareLink
	for rows.Next() {
		l, err := scanShareLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ShareLink returns a link by its ID.
func (db *DB) ShareLink(ctx context.Context, id string) (ShareLink, error) {
	l, err := scanShareLink(db.Reader.QueryRowContext(ctx, `SELECT `+shareLinkColumns+` WHERE l.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return l, fmt.Errorf("no share link %q: %w", id, ErrNotFound)
	}
	return l, err
}

// ShareLinkByToken returns a link by the hash of its token.
func (db *DB) ShareLinkByToken(ctx context.Context, tokenHash string) (ShareLink, error) {
	l, err := scanShareLink(db.Reader.QueryRowContext(ctx, `SELECT `+shareLinkColumns+` WHERE l.token_hash = ?`, tokenHash))
	if errors.Is(err, sql.ErrNoRows) {
		return l, fmt.Errorf("no such share link: %w", ErrNotFound)
	}
	return l, err
}

// UseShareLink counts a use of a link, and says whether there was one left.
func (db *DB) UseShareLink(ctx context.Context, id string, now time.Time) (bool, error) {
	used := false
	err := db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE share_links SET uses = uses + 1, last_used_at = ?
			WHERE id = ? AND revoked_at IS NULL AND expires_at > ? AND (max_uses = 0 OR uses < max_uses)`, now.UnixMilli(), id, now.UnixMilli())
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		used = n > 0
		return nil
	})
	return used, err
}

// RevokeShareLink ends a link of an environment.
func (db *DB) RevokeShareLink(ctx context.Context, app, env, id string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE share_links SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL AND env_id =
			(SELECT e.id FROM environments e JOIN apps a ON a.id = e.app_id WHERE a.name = ? AND e.name = ?)`, time.Now().UnixMilli(), id, app, env)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("no share link %q in use: %w", id, ErrNotFound)
		}
		return nil
	})
}
