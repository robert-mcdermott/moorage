package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The states of a pull request.
const (
	PullWaiting  = "waiting"  // for someone to approve it
	PullBuilding = "building" // on its way to its preview
	PullLive     = "live"
	PullFailed   = "failed"
	PullSkipped  = "skipped" // the app's manifest says that it has no preview
	PullClosed   = "closed"
)

// ForgeConnection says where an app's code is kept, on a forge.
type ForgeConnection struct {
	AppID, App string
	Kind, URL  string
	// APIURL overrides where the forge's API is, and Repo is the repository's path there.
	APIURL, Repo  string
	DefaultBranch string
	// HookID is the address a forge sends events to.
	HookID                    string
	SealedSecret, SealedToken []byte
	Previews, Poll            bool
	LastEventAt               time.Time
	LastError                 string
	CreatedBy                 string
	CreatedAt                 time.Time
}

// ForgePull is a pull request that a forge has told of.
type ForgePull struct {
	AppID                   string
	Number                  int
	Title, Author           string
	FromFork                bool
	HeadSHA, State          string
	Env, Error, CommentID   string
	ApprovedSHA, ApprovedBy string
	UpdatedAt               time.Time
}

const forgeColumns = `f.app_id, a.name, f.kind, f.url, f.api_url, f.repo, f.default_branch, f.hook_id, f.sealed_secret, f.sealed_token,
	f.previews, f.poll, f.last_event_at, f.last_error, f.created_by, f.created_at`

func scanForge(row interface{ Scan(...any) error }) (ForgeConnection, error) {
	var c ForgeConnection
	var last, created int64
	err := row.Scan(&c.AppID, &c.App, &c.Kind, &c.URL, &c.APIURL, &c.Repo, &c.DefaultBranch, &c.HookID, &c.SealedSecret, &c.SealedToken,
		&c.Previews, &c.Poll, &last, &c.LastError, &c.CreatedBy, &created)
	if err != nil {
		return c, err
	}
	if last != 0 {
		c.LastEventAt = time.UnixMilli(last).UTC()
	}
	c.CreatedAt = time.UnixMilli(created).UTC()
	return c, nil
}

// AppID returns an app's id, or ErrNotFound.
func (db *DB) AppID(ctx context.Context, name string) (string, error) {
	var id string
	err := db.Reader.QueryRowContext(ctx, `SELECT id FROM apps WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// ForgeOf returns an app's connection, or ErrNotFound.
func (db *DB) ForgeOf(ctx context.Context, app string) (ForgeConnection, error) {
	c, err := scanForge(db.Reader.QueryRowContext(ctx, `SELECT `+forgeColumns+` FROM forge_connections f JOIN apps a ON a.id = f.app_id WHERE a.name = ?`, app))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// ForgeByHook returns the connection that a hook's address is for, or ErrNotFound.
func (db *DB) ForgeByHook(ctx context.Context, hook string) (ForgeConnection, error) {
	c, err := scanForge(db.Reader.QueryRowContext(ctx, `SELECT `+forgeColumns+` FROM forge_connections f JOIN apps a ON a.id = f.app_id WHERE f.hook_id = ?`, hook))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// ForgeOfAppID returns an app's connection, by the app's id, or ErrNotFound.
func (db *DB) ForgeOfAppID(ctx context.Context, appID string) (ForgeConnection, error) {
	c, err := scanForge(db.Reader.QueryRowContext(ctx, `SELECT `+forgeColumns+` FROM forge_connections f JOIN apps a ON a.id = f.app_id WHERE f.app_id = ?`, appID))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// PolledForges lists the connections that are looked at, rather than told of changes.
func (db *DB) PolledForges(ctx context.Context) ([]ForgeConnection, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+forgeColumns+` FROM forge_connections f JOIN apps a ON a.id = f.app_id WHERE f.poll = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ForgeConnection
	for rows.Next() {
		c, err := scanForge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetForge keeps an app's connection.
func (db *DB) SetForge(ctx context.Context, c ForgeConnection) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO forge_connections (app_id, kind, url, api_url, repo, default_branch, hook_id, sealed_secret, sealed_token, previews, poll, last_event_at, last_error, created_by, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)
			ON CONFLICT (app_id) DO UPDATE SET kind = excluded.kind, url = excluded.url, api_url = excluded.api_url, repo = excluded.repo, default_branch = excluded.default_branch,
				sealed_token = excluded.sealed_token, previews = excluded.previews, poll = excluded.poll, last_error = excluded.last_error`,
			c.AppID, c.Kind, c.URL, c.APIURL, c.Repo, c.DefaultBranch, c.HookID, c.SealedSecret, c.SealedToken, c.Previews, c.Poll, c.LastError, c.CreatedBy, c.CreatedAt.UnixMilli())
		return err
	})
}

// DeleteForge lets go of an app's connection, and what it knew of pull requests.
func (db *DB) DeleteForge(ctx context.Context, appID string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM forge_pulls WHERE app_id = ?`, appID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM forge_connections WHERE app_id = ?`, appID)
		return err
	})
}

// SetForgeDefaultBranch notes what a repository's default branch is now.
func (db *DB) SetForgeDefaultBranch(ctx context.Context, appID, branch string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE forge_connections SET default_branch = ? WHERE app_id = ?`, branch, appID)
		return err
	})
}

// NoteForgeEvent records how the last thing a forge told of went.
func (db *DB) NoteForgeEvent(ctx context.Context, appID, problem string, at time.Time) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE forge_connections SET last_event_at = ?, last_error = ? WHERE app_id = ?`, at.UnixMilli(), problem, appID)
		return err
	})
}

// NoteForgeHeard records that a forge was heard from, with nothing to say about how that went.
func (db *DB) NoteForgeHeard(ctx context.Context, appID string, at time.Time) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE forge_connections SET last_event_at = ? WHERE app_id = ?`, at.UnixMilli(), appID)
		return err
	})
}

func scanPull(row interface{ Scan(...any) error }) (ForgePull, error) {
	var p ForgePull
	var updated int64
	err := row.Scan(&p.AppID, &p.Number, &p.Title, &p.Author, &p.FromFork, &p.HeadSHA, &p.State, &p.Env, &p.Error, &p.CommentID, &p.ApprovedSHA, &p.ApprovedBy, &updated)
	if err != nil {
		return p, err
	}
	p.UpdatedAt = time.UnixMilli(updated).UTC()
	return p, nil
}

const pullColumns = `app_id, number, title, author, from_fork, head_sha, state, env, error, comment_id, approved_sha, approved_by, updated_at`

// ForgePull returns a pull request, or ErrNotFound.
func (db *DB) ForgePull(ctx context.Context, appID string, number int) (ForgePull, error) {
	p, err := scanPull(db.Reader.QueryRowContext(ctx, `SELECT `+pullColumns+` FROM forge_pulls WHERE app_id = ? AND number = ?`, appID, number))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// ForgePulls lists an app's open pull requests, newest first.
func (db *DB) ForgePulls(ctx context.Context, appID string) ([]ForgePull, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+pullColumns+` FROM forge_pulls WHERE app_id = ? AND state <> ? ORDER BY number DESC LIMIT 100`, appID, PullClosed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ForgePull
	for rows.Next() {
		p, err := scanPull(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ForgePullsBuilding lists the pull requests that are being built, as far as anyone has said.
func (db *DB) ForgePullsBuilding(ctx context.Context) ([]ForgePull, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+pullColumns+` FROM forge_pulls WHERE state = ?`, PullBuilding)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ForgePull
	for rows.Next() {
		p, err := scanPull(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetForgePull keeps a pull request as it is now.
func (db *DB) SetForgePull(ctx context.Context, p ForgePull) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO forge_pulls (`+pullColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (app_id, number) DO UPDATE SET title = excluded.title, author = excluded.author, from_fork = excluded.from_fork, head_sha = excluded.head_sha, state = excluded.state,
				env = excluded.env, error = excluded.error, comment_id = excluded.comment_id, approved_sha = excluded.approved_sha, approved_by = excluded.approved_by, updated_at = excluded.updated_at`,
			p.AppID, p.Number, p.Title, p.Author, p.FromFork, p.HeadSHA, p.State, p.Env, p.Error, p.CommentID, p.ApprovedSHA, p.ApprovedBy, p.UpdatedAt.UnixMilli())
		return err
	})
}
