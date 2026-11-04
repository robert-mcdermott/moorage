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
	// Collected reports that garbage collection removed the build's files.
	Collected bool
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
	// Files and SizeBytes describe the build, and Kind what it is; BuildCollected says that it has
	// been removed to save space.
	Files          int
	SizeBytes      int64
	Kind           string
	BuildCollected bool
	// AppID and Config are set when the release is created or is the
	// current one: the variables it runs with, and the app they belong to.
	AppID  string
	Config []ConfigVar
}

// AppSummary describes an app and its production environment.
type AppSummary struct {
	ID        string
	Name      string
	CreatedAt time.Time
	// Project is the name of the project it is in.
	Project string
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
	// Asleep says that its instances are stopped until a request wakes it.
	Asleep bool
	// EnvID and Access say who may reach it.
	EnvID  string
	Access EnvAccess
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO apps (id, name, project_id, created_at) VALUES (?, ?, ?, ?)`, id, name, defaultProjectID, now.UnixMilli()); err != nil {
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
	buildID, _, err := db.RollbackBuild(ctx, app, env, version)
	if err != nil {
		return Release{}, err
	}
	return db.Publish(ctx, app, env, buildID, "rollback", by)
}

// RollbackBuild finds the build a rollback publishes again: that of
// release version, or when version is 0, of the release before the current
// one. It returns the build and the version it came from.
func (db *DB) RollbackBuild(ctx context.Context, app, env string, version int) (string, int, error) {
	var buildID string
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
		var collected sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT r.build_id, b.collected_at FROM releases r JOIN builds b ON b.id = r.build_id
			WHERE r.env_id = ? AND r.version = ?`, envID, version).Scan(&buildID, &collected)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%s has no release v%d: %w", app, version, ErrNotFound)
		}
		if err == nil && collected.Valid {
			return fmt.Errorf("the build of v%d was removed to save space ([deploy].keep_releases keeps the last releases); deploy it again instead", version)
		}
		return err
	})
	return buildID, version, err
}

// CreateRelease creates the next release of an environment without making
// it current: a release of a process app becomes current only once its
// rollout succeeds.
func (db *DB) CreateRelease(ctx context.Context, app, env, buildID, reason, by string) (Release, error) {
	return db.CreateReleaseWith(ctx, app, env, buildID, reason, by, nil)
}

// CreateReleaseWith is CreateRelease with the given variables in place of
// the environment's current ones, when cfg is not nil (a rollback that
// restores an earlier release's configuration).
func (db *DB) CreateReleaseWith(ctx context.Context, app, env, buildID, reason, by string, cfg []ConfigVar) (Release, error) {
	var rel Release
	err := db.write(ctx, func(tx *sql.Tx) error {
		var err error
		rel, err = createRelease(ctx, tx, app, env, buildID, reason, by, time.Now(), cfg)
		return err
	})
	return rel, err
}

// MakeCurrent makes a release its environment's current one.
func (db *DB) MakeCurrent(ctx context.Context, releaseID string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE environments SET current_release_id = ?
			WHERE id = (SELECT env_id FROM releases WHERE id = ?)`, releaseID, releaseID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("release %s: %w", releaseID, ErrNotFound)
		}
		return nil
	})
}

// DeleteRelease removes a release that never became current, such as one
// whose rollout failed, with the instances it had.
func (db *DB) DeleteRelease(ctx context.Context, releaseID string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM releases WHERE id = ?
			AND id NOT IN (SELECT current_release_id FROM environments WHERE current_release_id IS NOT NULL)`, releaseID)
		return err
	})
}

// CurrentRelease returns an environment's current release and its build.
func (db *DB) CurrentRelease(ctx context.Context, app, env string) (Release, Build, error) {
	r, b, err := db.release(ctx, `r.id = e.current_release_id`, app, env)
	if errors.Is(err, sql.ErrNoRows) {
		return r, b, fmt.Errorf("%s has nothing deployed to %s: %w", app, env, ErrNotFound)
	}
	return r, b, err
}

// ReleaseVersion returns a release of an environment and its build.
func (db *DB) ReleaseVersion(ctx context.Context, app, env string, version int) (Release, Build, error) {
	r, b, err := db.release(ctx, `r.env_id = e.id AND r.version = ?`, version, app, env)
	if errors.Is(err, sql.ErrNoRows) {
		return r, b, fmt.Errorf("%s has no release v%d: %w", app, version, ErrNotFound)
	}
	return r, b, err
}

// release reads the release that a condition on it joins to its environment, with
// its build. The arguments are the condition's, then the app and environment.
func (db *DB) release(ctx context.Context, on string, args ...any) (Release, Build, error) {
	var r Release
	var b Build
	var by, current sql.NullString
	var plan, cfg string
	var created, bcreated int64
	var collected sql.NullInt64
	err := db.Reader.QueryRowContext(ctx, `SELECT r.id, r.version, r.build_id, r.reason, r.created_by, r.created_at, e.app_id, r.config, e.current_release_id,
			b.id, b.app_id, b.source_type, b.artifact_kind, b.artifact_ref, b.plan, b.files, b.size_bytes, b.created_at, b.collected_at
		FROM environments e JOIN apps a ON a.id = e.app_id
		JOIN releases r ON `+on+`
		JOIN builds b ON b.id = r.build_id
		WHERE a.name = ? AND e.name = ?`, args...).Scan(&r.ID, &r.Version, &r.BuildID, &r.Reason, &by, &created, &r.AppID, &cfg, &current,
		&b.ID, &b.AppID, &b.SourceType, &b.ArtifactKind, &b.ArtifactRef, &plan, &b.Files, &b.SizeBytes, &bcreated, &collected)
	if err != nil {
		return r, b, err
	}
	r.CreatedBy, r.CreatedAt, r.Current = by.String, time.UnixMilli(created).UTC(), r.ID == current.String
	r.Files, r.SizeBytes = b.Files, b.SizeBytes
	if err := json.Unmarshal([]byte(cfg), &r.Config); err != nil {
		return r, b, err
	}
	b.Plan, b.CreatedAt, b.Collected = json.RawMessage(plan), time.UnixMilli(bcreated).UTC(), collected.Valid
	return r, b, nil
}

// Build returns a build.
func (db *DB) Build(ctx context.Context, id string) (Build, error) {
	var b Build
	var plan string
	var created int64
	var collected sql.NullInt64
	err := db.Reader.QueryRowContext(ctx, `SELECT id, app_id, source_type, artifact_kind, artifact_ref, plan, files, size_bytes, created_at, collected_at
		FROM builds WHERE id = ?`, id).Scan(&b.ID, &b.AppID, &b.SourceType, &b.ArtifactKind, &b.ArtifactRef, &plan, &b.Files, &b.SizeBytes, &created, &collected)
	if errors.Is(err, sql.ErrNoRows) {
		return b, fmt.Errorf("build %s: %w", id, ErrNotFound)
	}
	b.Plan, b.CreatedAt, b.Collected = json.RawMessage(plan), time.UnixMilli(created).UTC(), collected.Valid
	return b, err
}

func publish(ctx context.Context, tx *sql.Tx, app, env, buildID, reason, by string, now time.Time) (Release, error) {
	rel, err := createRelease(ctx, tx, app, env, buildID, reason, by, now, nil)
	if err != nil {
		return Release{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE environments SET current_release_id = ? WHERE id = (SELECT env_id FROM releases WHERE id = ?)`, rel.ID, rel.ID); err != nil {
		return Release{}, err
	}
	rel.Current = true
	return rel, nil
}

// createRelease records a new release of buildID. It runs with cfg, or
// when cfg is nil, with the environment's variables as they are now.
func createRelease(ctx context.Context, tx *sql.Tx, app, env, buildID, reason, by string, now time.Time, cfg []ConfigVar) (Release, error) {
	envID, _, err := envState(ctx, tx, app, env)
	if err != nil {
		return Release{}, err
	}
	var appID string
	if err := tx.QueryRowContext(ctx, `SELECT app_id FROM environments WHERE id = ?`, envID).Scan(&appID); err != nil {
		return Release{}, err
	}
	if cfg == nil {
		if cfg, err = config(ctx, tx, appID, envID); err != nil {
			return Release{}, err
		}
	}
	cfgJSON, err := json.Marshal(cfg)
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
		AppID:     appID,
		Config:    cfg,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO releases (id, env_id, version, build_id, reason, created_by, created_at, config)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, rel.ID, envID, rel.Version, buildID, reason, nullString(by), now.UnixMilli(), string(cfgJSON)); err != nil {
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
	return db.AppsAfter(ctx, "", -1)
}

// AppsAfter lists up to limit apps (no limit if negative) by name, after the
// name after.
func (db *DB) AppsAfter(ctx context.Context, after string, limit int) ([]AppSummary, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT a.id, a.name, a.created_at, coalesce(p.name, '`+DefaultProject+`'),
			r.id, r.version, r.build_id, r.reason, r.created_by, r.created_at, b.files, b.size_bytes
		FROM apps a
		JOIN environments e ON e.app_id = a.id AND e.kind = 'production'
		LEFT JOIN projects p ON p.id = a.project_id
		LEFT JOIN releases r ON r.id = e.current_release_id
		LEFT JOIN builds b ON b.id = r.build_id
		WHERE a.name > ?
		ORDER BY a.name LIMIT ?`, after, limit)
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
		if err := rows.Scan(&a.ID, &a.Name, &created, &a.Project, &relID, &version, &buildID, &reason, &by, &relCreated, &files, &size); err != nil {
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
	return db.ReleasesBefore(ctx, app, env, 0, -1)
}

// ReleasesBefore lists up to limit releases (no limit if negative), newest
// first, older than the version before (0: from the newest).
func (db *DB) ReleasesBefore(ctx context.Context, app, env string, before, limit int) ([]Release, error) {
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
	rows, err := db.Reader.QueryContext(ctx, `SELECT r.id, r.version, r.build_id, r.reason, r.created_by, r.created_at, b.files, b.size_bytes,
			COALESCE(json_extract(b.plan, '$.kind'), ''), b.collected_at IS NOT NULL
		FROM releases r JOIN builds b ON b.id = r.build_id
		WHERE r.env_id = ? AND (? = 0 OR r.version < ?) ORDER BY r.version DESC LIMIT ?`, envID, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		var r Release
		var by sql.NullString
		var created int64
		if err := rows.Scan(&r.ID, &r.Version, &r.BuildID, &r.Reason, &by, &created, &r.Files, &r.SizeBytes, &r.Kind, &r.BuildCollected); err != nil {
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
	rows, err := db.Reader.QueryContext(ctx, `SELECT a.name, e.name, e.kind, r.version, e.asleep_at IS NOT NULL,
			e.id, e.access_mode, e.access_password, e.access_allow, e.access_any_user, e.access_generation,
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
		var mode, password, allow sql.NullString
		var anyUser int
		b := &rt.Build
		if err := rows.Scan(&rt.App, &rt.Env, &rt.Kind, &rt.Version, &rt.Asleep,
			&rt.EnvID, &mode, &password, &allow, &anyUser, &rt.Access.Generation,
			&b.ID, &b.AppID, &b.SourceType, &b.ArtifactKind, &b.ArtifactRef, &plan, &b.Files, &b.SizeBytes, &created); err != nil {
			return nil, err
		}
		rt.Access.Mode, rt.Access.PasswordHash, rt.Access.Allow, rt.Access.AnyUser = mode.String, password.String, splitAllow(allow.String), anyUser != 0
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

// AppUID says which user an app runs as, choosing one the first time: every other one from base, as
// the one after each is for its builds.
func (db *DB) AppUID(ctx context.Context, app string, base uint32) (uint32, error) {
	var uid uint32
	err := db.write(ctx, func(tx *sql.Tx) error {
		var appID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM apps WHERE name = ?`, app).Scan(&appID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("no app %q: %w", app, ErrNotFound)
			}
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT uid FROM app_users WHERE app_id = ?`, appID).Scan(&uid)
		if err == nil || !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var highest sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(uid) FROM app_users`).Scan(&highest); err != nil {
			return err
		}
		uid = base
		if highest.Valid && uint32(highest.Int64) >= base {
			uid = uint32(highest.Int64) + 2
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO app_users (app_id, uid) VALUES (?, ?)`, appID, uid)
		return err
	})
	return uid, err
}
