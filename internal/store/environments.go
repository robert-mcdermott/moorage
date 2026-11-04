package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/names"
)

// The kinds of environment.
const (
	KindProduction = "production"
	KindNamed      = "named"
	KindPreview    = "preview"
)

// Environment is where an app runs: production, a named environment, or a preview of a
// branch.
type Environment struct {
	ID, App, Name, Kind string
	// Branch is the branch that deploys to it, if one does.
	Branch string
	// Pinned keeps a preview from expiring.
	Pinned bool
	// LockedBy, if set, is who stopped deployments to it, and LockedAt when.
	LockedBy string
	LockedAt time.Time
	// LastActive is when it last served a request, as far as the server has noted.
	LastActive time.Time
	// AsleepAt, if set, is when its instances were stopped until a request wakes it.
	AsleepAt  time.Time
	CreatedAt time.Time
	// Version, ReleasedAt and ReleasedBy describe its current release, if any.
	Version    int
	ReleasedAt time.Time
	ReleasedBy string
}

const environmentColumns = `e.id, a.name, e.name, e.kind, e.branch, e.pinned, e.locked_by, e.locked_at, e.last_active_at, e.asleep_at, e.created_at,
	r.version, r.created_at, r.created_by
	FROM environments e JOIN apps a ON a.id = e.app_id LEFT JOIN releases r ON r.id = e.current_release_id`

func scanEnvironment(row interface{ Scan(...any) error }) (Environment, error) {
	var e Environment
	var branch, lockedBy, releasedBy sql.NullString
	var pinned int
	var lockedAt, lastActive, asleepAt, version, releasedAt sql.NullInt64
	var created int64
	if err := row.Scan(&e.ID, &e.App, &e.Name, &e.Kind, &branch, &pinned, &lockedBy, &lockedAt, &lastActive, &asleepAt, &created, &version, &releasedAt, &releasedBy); err != nil {
		return e, err
	}
	e.Branch, e.Pinned, e.LockedBy, e.ReleasedBy = branch.String, pinned != 0, lockedBy.String, releasedBy.String
	e.CreatedAt, e.Version = time.UnixMilli(created).UTC(), int(version.Int64)
	if lockedAt.Valid {
		e.LockedAt = time.UnixMilli(lockedAt.Int64).UTC()
	}
	if lastActive.Valid {
		e.LastActive = time.UnixMilli(lastActive.Int64).UTC()
	}
	if asleepAt.Valid {
		e.AsleepAt = time.UnixMilli(asleepAt.Int64).UTC()
	}
	if releasedAt.Valid {
		e.ReleasedAt = time.UnixMilli(releasedAt.Int64).UTC()
	}
	return e, nil
}

func (db *DB) listEnvironments(ctx context.Context, where string, args ...any) ([]Environment, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+environmentColumns+` WHERE `+where+` ORDER BY a.name, e.kind <> 'production', e.kind <> 'named', e.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Environment
	for rows.Next() {
		e, err := scanEnvironment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Environments lists an app's environments: production first, then those named, then
// previews.
func (db *DB) Environments(ctx context.Context, app string) ([]Environment, error) {
	return db.listEnvironments(ctx, `a.name = ?`, app)
}

// AllEnvironments lists the environments of every app.
func (db *DB) AllEnvironments(ctx context.Context) ([]Environment, error) {
	return db.listEnvironments(ctx, `1 = 1`)
}

// Previews lists the previews of every app.
func (db *DB) Previews(ctx context.Context) ([]Environment, error) {
	return db.listEnvironments(ctx, `e.kind = 'preview'`)
}

// EnvironmentCounts says how many environments each app has.
func (db *DB) EnvironmentCounts(ctx context.Context) (map[string]int, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT a.name, COUNT(*) FROM environments e JOIN apps a ON a.id = e.app_id GROUP BY a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		counts[name] = n
	}
	return counts, rows.Err()
}

// Environment returns one of an app's environments.
func (db *DB) Environment(ctx context.Context, app, name string) (Environment, error) {
	e, err := scanEnvironment(db.Reader.QueryRowContext(ctx, `SELECT `+environmentColumns+` WHERE a.name = ? AND e.name = ?`, app, name))
	if errors.Is(err, sql.ErrNoRows) {
		return e, fmt.Errorf("no environment %q of app %q: %w", name, app, ErrNotFound)
	}
	return e, err
}

// EnvironmentOfBranch returns the environment a branch deploys to, if one does.
func (db *DB) EnvironmentOfBranch(ctx context.Context, app, branch string) (Environment, error) {
	e, err := scanEnvironment(db.Reader.QueryRowContext(ctx, `SELECT `+environmentColumns+` WHERE a.name = ? AND e.branch = ?`, app, branch))
	if errors.Is(err, sql.ErrNoRows) {
		return e, fmt.Errorf("no environment of the branch %q of app %q: %w", branch, app, ErrNotFound)
	}
	return e, err
}

// EnvKind says what kind an environment is. One that does not exist is named, as
// deploying to it would make it.
func (db *DB) EnvKind(ctx context.Context, app, name string) (string, error) {
	var kind string
	err := db.Reader.QueryRowContext(ctx, `SELECT e.kind FROM environments e JOIN apps a ON a.id = e.app_id WHERE a.name = ? AND e.name = ?`, app, name).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return KindNamed, nil
	}
	return kind, err
}

// CreateEnvironment makes an environment of an app, unless it has one of that name, and
// reports whether it did.
func (db *DB) CreateEnvironment(ctx context.Context, app, name, kind, branch string) (bool, error) {
	if err := names.ValidateEnv(name); err != nil {
		return false, &InvalidNameError{err}
	}
	created := false
	err := db.write(ctx, func(tx *sql.Tx) error {
		var appID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM apps WHERE name = ?`, app).Scan(&appID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no app %q: %w", app, ErrNotFound)
		}
		if err != nil {
			return err
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM environments WHERE app_id = ? AND name = ?`, appID, name).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			return nil
		}
		var br any
		if branch != "" {
			br = branch
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO environments (id, app_id, name, kind, branch, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			ids.New(ids.Env), appID, name, kind, br, time.Now().UnixMilli())
		created = err == nil
		return err
	})
	return created, err
}

// SetEnvironmentBranch says which branch deploys to an environment.
// SetAsleep says whether an environment's instances are stopped until a request wakes it.
func (db *DB) SetAsleep(ctx context.Context, app, name string, asleep bool) error {
	var at any
	if asleep {
		at = time.Now().UnixMilli()
	}
	return db.updateEnvironment(ctx, app, name, `asleep_at = ?`, at)
}

// SetEnvironmentBranch says which branch deploys to an environment.
func (db *DB) SetEnvironmentBranch(ctx context.Context, app, name, branch string) error {
	return db.updateEnvironment(ctx, app, name, `branch = ?`, branch)
}

func (db *DB) updateEnvironment(ctx context.Context, app, name, set string, args ...any) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE environments SET `+set+` WHERE name = ? AND app_id = (SELECT id FROM apps WHERE name = ?)`, append(args, name, app)...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("no environment %q of app %q: %w", name, app, ErrNotFound)
		}
		return nil
	})
}

// SetPinned keeps a preview from expiring, or lets it.
func (db *DB) SetPinned(ctx context.Context, app, name string, pinned bool) error {
	return db.updateEnvironment(ctx, app, name, `pinned = ?`, boolInt(pinned))
}

// SetLocked stops deployments to an environment, or lets them go on: by "" unlocks it.
func (db *DB) SetLocked(ctx context.Context, app, name, by string) error {
	if by == "" {
		return db.updateEnvironment(ctx, app, name, `locked_by = NULL, locked_at = NULL`)
	}
	return db.updateEnvironment(ctx, app, name, `locked_by = ?, locked_at = ?`, by, time.Now().UnixMilli())
}

// SetLastActive notes when an environment last served a request.
func (db *DB) SetLastActive(ctx context.Context, app, name string, at time.Time) error {
	return db.updateEnvironment(ctx, app, name, `last_active_at = ?`, at.UnixMilli())
}

// EnvironmentDeployments lists the IDs of an environment's deployments.
func (db *DB) EnvironmentDeployments(ctx context.Context, app, name string) ([]string, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT d.id FROM deployments d JOIN environments e ON e.id = d.env_id JOIN apps a ON a.id = e.app_id
		WHERE a.name = ? AND e.name = ?`, app, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeleteEnvironment removes an environment, its variables, releases and deployments.
func (db *DB) DeleteEnvironment(ctx context.Context, app, name string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		envID, _, err := envState(ctx, tx, app, name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE environments SET current_release_id = NULL WHERE id = ?`, envID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM environments WHERE id = ?`, envID)
		return err
	})
}
