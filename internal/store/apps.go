package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/names"
)

// ErrNotFound is returned when an app, environment or release does not exist.
var ErrNotFound = errors.New("not found")

// Build is an immutable artifact of an app.
type Build struct {
	ID           string
	AppID        string
	SourceType   string
	ArtifactKind string
	ArtifactRef  string
	Plan         json.RawMessage
	Files        int
	SizeBytes    int64
	CreatedAt    time.Time
}

// Release puts a build into an environment.
type Release struct {
	ID        string
	Version   int
	BuildID   string
	Reason    string
	CreatedBy string
	CreatedAt time.Time
	// Current reports whether the environment serves this release.
	Current bool
	// Files and SizeBytes describe the build.
	Files     int
	SizeBytes int64
}

// AppSummary describes an app and its production environment.
type AppSummary struct {
	ID        string
	Name      string
	CreatedAt time.Time
	// Production is the current production release, if any.
	Production *Release
}

// Route is an environment that serves a release, with that release's build.
type Route struct {
	App     string
	Env     string
	Kind    string
	Version int
	Build   Build
}

// EnsureApp creates the app if it does not exist yet, with its production
// environment, and reports whether it did.
func (db *DB) EnsureApp(ctx context.Context, name string) (id string, created bool, err error) {
	if err := names.ValidateApp(name); err != nil {
		return "", false, err
	}
	err = db.write(ctx, func(tx *sql.Tx) error {
		id, created, err = ensureApp(ctx, tx, name, time.Now())
		return err
	})
	return id, created, err
}

func ensureApp(ctx context.Context, tx *sql.Tx, name string, now time.Time) (string, bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM apps WHERE name = ?`, name).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	id = ids.New(ids.App)
	if _, err := tx.ExecContext(ctx, `INSERT INTO apps (id, name, created_at) VALUES (?, ?, ?)`, id, name, now.UnixMilli()); err != nil {
		return "", false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO environments (id, app_id, name, kind, created_at) VALUES (?, ?, ?, 'production', ?)`,
		ids.New(ids.Env), id, names.Production, now.UnixMilli()); err != nil {
		return "", false, err
	}
	return id, true, nil
}

// CreateBuild records a build.
func (db *DB) CreateBuild(ctx context.Context, b Build) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO builds
			(id, app_id, source_type, artifact_kind, artifact_ref, plan, files, size_bytes, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			b.ID, b.AppID, b.SourceType, b.ArtifactKind, b.ArtifactRef, string(b.Plan), b.Files, b.SizeBytes, b.CreatedAt.UnixMilli())
		return err
	})
}

// Publish creates the next release of an app's environment with buildID
// and makes it current.
func (db *DB) Publish(ctx context.Context, app, env, buildID, reason, by string) (Release, error) {
	var rel Release
	err := db.write(ctx, func(tx *sql.Tx) error {
		var err error
		rel, err = publish(ctx, tx, app, env, buildID, reason, by, time.Now())
		return err
	})
	return rel, err
}

// Rollback publishes again the build of an earlier release: version, or
// when version is 0, the release before the current one.
func (db *DB) Rollback(ctx context.Context, app, env string, version int, by string) (Release, error) {
	var rel Release
	err := db.write(ctx, func(tx *sql.Tx) error {
		envID, current, err := envState(ctx, tx, app, env)
		if err != nil {
			return err
		}
		if version == 0 {
			if current == 0 {
				return fmt.Errorf("%s has nothing deployed to roll back", app)
			}
			version = current - 1
			if version == 0 {
				return fmt.Errorf("%s has no release before v%d", app, current)
			}
		}
		if version == current {
			return fmt.Errorf("v%d is already the current release of %s", version, app)
		}
		var buildID string
		err = tx.QueryRowContext(ctx, `SELECT build_id FROM releases WHERE env_id = ? AND version = ?`, envID, version).Scan(&buildID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%s has no release v%d: %w", app, version, ErrNotFound)
		}
		if err != nil {
			return err
		}
		rel, err = publish(ctx, tx, app, env, buildID, "rollback", by, time.Now())
		return err
	})
	return rel, err
}

func publish(ctx context.Context, tx *sql.Tx, app, env, buildID, reason, by string, now time.Time) (Release, error) {
	envID, _, err := envState(ctx, tx, app, env)
	if err != nil {
		return Release{}, err
	}
	var last int
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM releases WHERE env_id = ?`, envID).Scan(&last); err != nil {
		return Release{}, err
	}
	rel := Release{
		ID:        ids.New(ids.Release),
		Version:   last + 1,
		BuildID:   buildID,
		Reason:    reason,
		CreatedBy: by,
		CreatedAt: now,
		Current:   true,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO releases (id, env_id, version, build_id, reason, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, rel.ID, envID, rel.Version, buildID, reason, nullString(by), now.UnixMilli()); err != nil {
		return Release{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE environments SET current_release_id = ? WHERE id = ?`, rel.ID, envID); err != nil {
		return Release{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT files, size_bytes FROM builds WHERE id = ?`, buildID).Scan(&rel.Files, &rel.SizeBytes); err != nil {
		return Release{}, err
	}
	return rel, nil
}

// envState returns an environment's ID and its current release version
// (0 if none).
func envState(ctx context.Context, tx *sql.Tx, app, env string) (string, int, error) {
	var envID string
	var current sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT e.id, r.version
		FROM environments e JOIN apps a ON a.id = e.app_id
		LEFT JOIN releases r ON r.id = e.current_release_id
		WHERE a.name = ? AND e.name = ?`, app, env).Scan(&envID, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, fmt.Errorf("no environment %q of app %q: %w", env, app, ErrNotFound)
	}
	return envID, int(current.Int64), err
}

// Apps lists the apps by name, with their current production release.
func (db *DB) Apps(ctx context.Context) ([]AppSummary, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT a.id, a.name, a.created_at,
			r.id, r.version, r.build_id, r.reason, r.created_by, r.created_at, b.files, b.size_bytes
		FROM apps a
		JOIN environments e ON e.app_id = a.id AND e.kind = 'production'
		LEFT JOIN releases r ON r.id = e.current_release_id
		LEFT JOIN builds b ON b.id = r.build_id
		ORDER BY a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppSummary
	for rows.Next() {
		var a AppSummary
		var created int64
		var relID, buildID, reason, by sql.NullString
		var version, relCreated, files, size sql.NullInt64
		if err := rows.Scan(&a.ID, &a.Name, &created, &relID, &version, &buildID, &reason, &by, &relCreated, &files, &size); err != nil {
			return nil, err
		}
		a.CreatedAt = time.UnixMilli(created).UTC()
		if relID.Valid {
			a.Production = &Release{
				ID: relID.String, Version: int(version.Int64), BuildID: buildID.String, Reason: reason.String,
				CreatedBy: by.String, CreatedAt: time.UnixMilli(relCreated.Int64).UTC(), Current: true,
				Files: int(files.Int64), SizeBytes: size.Int64,
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Releases lists an environment's releases, newest first.
func (db *DB) Releases(ctx context.Context, app, env string) ([]Release, error) {
	var envID string
	var currentID sql.NullString
	err := db.Reader.QueryRowContext(ctx, `SELECT e.id, e.current_release_id
		FROM environments e JOIN apps a ON a.id = e.app_id
		WHERE a.name = ? AND e.name = ?`, app, env).Scan(&envID, &currentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no environment %q of app %q: %w", env, app, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	rows, err := db.Reader.QueryContext(ctx, `SELECT r.id, r.version, r.build_id, r.reason, r.created_by, r.created_at, b.files, b.size_bytes
		FROM releases r JOIN builds b ON b.id = r.build_id
		WHERE r.env_id = ? ORDER BY r.version DESC`, envID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		var r Release
		var by sql.NullString
		var created int64
		if err := rows.Scan(&r.ID, &r.Version, &r.BuildID, &r.Reason, &by, &created, &r.Files, &r.SizeBytes); err != nil {
			return nil, err
		}
		r.CreatedBy = by.String
		r.CreatedAt = time.UnixMilli(created).UTC()
		r.Current = r.ID == currentID.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// Routes lists every environment that serves a release, with its build.
func (db *DB) Routes(ctx context.Context) ([]Route, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT a.name, e.name, e.kind, r.version,
			b.id, b.app_id, b.source_type, b.artifact_kind, b.artifact_ref, b.plan, b.files, b.size_bytes, b.created_at
		FROM environments e
		JOIN apps a ON a.id = e.app_id
		JOIN releases r ON r.id = e.current_release_id
		JOIN builds b ON b.id = r.build_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Route
	for rows.Next() {
		var rt Route
		var plan string
		var created int64
		b := &rt.Build
		if err := rows.Scan(&rt.App, &rt.Env, &rt.Kind, &rt.Version,
			&b.ID, &b.AppID, &b.SourceType, &b.ArtifactKind, &b.ArtifactRef, &plan, &b.Files, &b.SizeBytes, &created); err != nil {
			return nil, err
		}
		b.Plan = json.RawMessage(plan)
		b.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, rt)
	}
	return out, rows.Err()
}

// AppNames returns the names of all apps.
func (db *DB) AppNames(ctx context.Context) ([]string, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT name FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// write runs fn in a write transaction on the writer connection.
func (db *DB) write(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
