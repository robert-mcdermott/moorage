package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Deployment statuses (spec/04-build-and-deploy.md, "Pipeline").
const (
	Queued     = "queued"
	Building   = "building"
	Releasing  = "releasing"
	RollingOut = "rolling_out"
	Live       = "live"
	Failed     = "failed"
	Superseded = "superseded"
	Canceled   = "canceled"
)

// Final reports whether a deployment in this status has finished.
func Final(status string) bool {
	return status == Live || status == Failed || status == Superseded || status == Canceled
}

// Deployment is the job that produces a release.
type Deployment struct {
	ID         string
	App        string
	Env        string
	SourceType string
	// SourceRef, SourceCommit and SourceMessage say what a push deployed:
	// its ref, its commit and the first line of the commit's message.
	SourceRef     string
	SourceCommit  string
	SourceMessage string
	Status        string
	BuildID       string
	ReleaseID     string
	// Version is the release's number, once there is a release.
	Version int
	// BuildCollected reports that the build was removed to save space, so that
	// its release can no longer be served again.
	BuildCollected bool
	// BuildKind is what the build is: static or process.
	BuildKind  string
	Error      string
	Output     []string
	CreatedBy  string
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
}

// CreateDeployment records a new deployment of an existing environment.
func (db *DB) CreateDeployment(ctx context.Context, d Deployment) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		envID, _, err := envState(ctx, tx, d.App, d.Env)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO deployments (id, env_id, source_type, source_ref, source_commit, source_message,
			status, build_id, created_by, created_at, started_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.ID, envID, d.SourceType, nullString(d.SourceRef), nullString(d.SourceCommit), nullString(d.SourceMessage),
			d.Status, nullString(d.BuildID), nullString(d.CreatedBy), d.CreatedAt.UnixMilli(), nullTime(d.StartedAt))
		return err
	})
}

// SetDeploymentStatus moves a deployment to a status that is not final.
func (db *DB) SetDeploymentStatus(ctx context.Context, id, status string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE deployments SET status = ?, started_at = coalesce(started_at, ?) WHERE id = ?`,
			status, time.Now().UnixMilli(), id)
		return err
	})
}

// SetDeploymentRelease records the release a deployment created.
func (db *DB) SetDeploymentRelease(ctx context.Context, id, releaseID string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE deployments SET release_id = ? WHERE id = ?`, releaseID, id)
		return err
	})
}

// FinishDeployment records how a deployment ended.
func (db *DB) FinishDeployment(ctx context.Context, id, status, errMsg string, output []string) error {
	if !Final(status) {
		return fmt.Errorf("%s is not a final status", status)
	}
	var out any
	if len(output) > 0 {
		data, err := json.Marshal(output)
		if err != nil {
			return err
		}
		out = string(data)
	}
	return db.write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		_, err := tx.ExecContext(ctx, `UPDATE deployments SET status = ?, error = ?, output = ?,
			started_at = coalesce(started_at, ?), finished_at = ? WHERE id = ?`, status, nullString(errMsg), out, now, now, id)
		return err
	})
}

// InterruptDeployments fails the deployments a stopped server left
// unfinished, and returns how many there were.
func (db *DB) InterruptDeployments(ctx context.Context) (int, error) {
	var n int64
	err := db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE deployments SET status = 'failed', error = 'interrupted: the server stopped during the deployment', finished_at = ?
			WHERE status NOT IN ('live', 'failed', 'superseded', 'canceled')`, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return int(n), err
}

const deploymentColumns = `SELECT d.id, a.name, e.name, d.source_type, d.source_ref, d.source_commit, d.source_message, d.status, d.build_id, d.release_id, r.version,
	d.error, d.output, d.created_by, d.created_at, d.started_at, d.finished_at, b.collected_at, COALESCE(json_extract(b.plan, '$.kind'), '')
	FROM deployments d JOIN environments e ON e.id = d.env_id JOIN apps a ON a.id = e.app_id
	LEFT JOIN releases r ON r.id = d.release_id
	LEFT JOIN builds b ON b.id = d.build_id`

func scanDeployment(row interface{ Scan(...any) error }) (Deployment, error) {
	var d Deployment
	var buildID, releaseID, errMsg, output, by, ref, commit, message sql.NullString
	var version, started, finished, collected sql.NullInt64
	var created int64
	if err := row.Scan(&d.ID, &d.App, &d.Env, &d.SourceType, &ref, &commit, &message, &d.Status, &buildID, &releaseID, &version,
		&errMsg, &output, &by, &created, &started, &finished, &collected, &d.BuildKind); err != nil {
		return Deployment{}, err
	}
	d.SourceRef, d.SourceCommit, d.SourceMessage = ref.String, commit.String, message.String
	d.BuildCollected = collected.Valid
	d.BuildID, d.ReleaseID, d.Version, d.Error, d.CreatedBy = buildID.String, releaseID.String, int(version.Int64), errMsg.String, by.String
	if output.Valid {
		json.Unmarshal([]byte(output.String), &d.Output)
	}
	d.CreatedAt = time.UnixMilli(created).UTC()
	if started.Valid {
		d.StartedAt = time.UnixMilli(started.Int64).UTC()
	}
	if finished.Valid {
		d.FinishedAt = time.UnixMilli(finished.Int64).UTC()
	}
	return d, nil
}

// Deployment returns one deployment.
func (db *DB) Deployment(ctx context.Context, id string) (Deployment, error) {
	d, err := scanDeployment(db.Reader.QueryRowContext(ctx, deploymentColumns+` WHERE d.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, fmt.Errorf("no deployment %s: %w", id, ErrNotFound)
	}
	return d, err
}

// BuildDeployment returns the deployment that made a build: what it was made from.
func (db *DB) BuildDeployment(ctx context.Context, buildID string) (Deployment, error) {
	d, err := scanDeployment(db.Reader.QueryRowContext(ctx, deploymentColumns+` WHERE d.build_id = ?
		ORDER BY d.created_at, d.id LIMIT 1`, buildID))
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, fmt.Errorf("no deployment of the build %s: %w", buildID, ErrNotFound)
	}
	return d, err
}

// LatestDeployments returns the newest deployment of each app to an environment, by
// app.
func (db *DB) LatestDeployments(ctx context.Context, env string) (map[string]Deployment, error) {
	rows, err := db.Reader.QueryContext(ctx, deploymentColumns+` WHERE d.id IN (
		SELECT (SELECT d2.id FROM deployments d2 WHERE d2.env_id = e2.id ORDER BY d2.created_at DESC, d2.id DESC LIMIT 1)
		FROM environments e2 WHERE e2.name = ?)`, env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Deployment{}
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out[d.App] = d
	}
	return out, rows.Err()
}

// Deployments lists an environment's deployments, newest first.
func (db *DB) Deployments(ctx context.Context, app, env string, limit int) ([]Deployment, error) {
	return db.DeploymentsBefore(ctx, app, env, time.Time{}, "", limit)
}

// DeploymentsBefore lists up to limit deployments, newest first, older than
// the one created at created with this ID (a zero time: from the newest).
func (db *DB) DeploymentsBefore(ctx context.Context, app, env string, created time.Time, id string, limit int) ([]Deployment, error) {
	if _, err := db.EnvID(ctx, app, env); err != nil {
		return nil, err
	}
	before := int64(0)
	if !created.IsZero() {
		before = created.UnixMilli()
	}
	rows, err := db.Reader.QueryContext(ctx, deploymentColumns+` WHERE a.name = ? AND e.name = ?
		AND (? = 0 OR (d.created_at, d.id) < (?, ?))
		ORDER BY d.created_at DESC, d.id DESC LIMIT ?`, app, env, before, before, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CollectableBuilds returns the builds of an app that garbage collection may
// remove: not yet collected, older than minAge, used by none of the last
// keep releases of any environment nor by any current release, and not
// part of a deployment still running. Each build's ArtifactRef says what to
// delete.
func (db *DB) CollectableBuilds(ctx context.Context, app string, keep int, minAge time.Duration) ([]Build, error) {
	rows, err := db.Reader.QueryContext(ctx, `
		WITH ranked AS (
			SELECT r.build_id, row_number() OVER (PARTITION BY r.env_id ORDER BY r.version DESC) AS n
			FROM releases r JOIN environments e ON e.id = r.env_id JOIN apps a ON a.id = e.app_id
			WHERE a.name = ?)
		SELECT b.id, b.app_id, b.source_type, b.artifact_kind, b.artifact_ref, b.plan, b.files, b.size_bytes, b.created_at
		FROM builds b JOIN apps a ON a.id = b.app_id
		WHERE a.name = ? AND b.collected_at IS NULL AND b.created_at <= ?
		  AND b.id NOT IN (SELECT build_id FROM ranked WHERE n <= ?)
		  AND b.id NOT IN (SELECT r.build_id FROM environments e JOIN releases r ON r.id = e.current_release_id)
		  AND b.id NOT IN (SELECT build_id FROM deployments WHERE build_id IS NOT NULL
		                   AND status NOT IN ('live', 'failed', 'superseded', 'canceled'))
		ORDER BY b.created_at`, app, app, time.Now().Add(-minAge).UnixMilli(), keep)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Build
	for rows.Next() {
		var b Build
		var plan string
		var created int64
		if err := rows.Scan(&b.ID, &b.AppID, &b.SourceType, &b.ArtifactKind, &b.ArtifactRef, &plan, &b.Files, &b.SizeBytes, &created); err != nil {
			return nil, err
		}
		b.Plan = json.RawMessage(plan)
		b.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}

// MarkCollected records that a build's files were removed.
func (db *DB) MarkCollected(ctx context.Context, buildID string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE builds SET collected_at = ? WHERE id = ?`, time.Now().UnixMilli(), buildID)
		return err
	})
}

// BuildIDs returns the IDs of every build an app has a row for, collected
// or not.
func (db *DB) BuildIDs(ctx context.Context, app string) (map[string]bool, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT b.id FROM builds b JOIN apps a ON a.id = b.app_id WHERE a.name = ?`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}
