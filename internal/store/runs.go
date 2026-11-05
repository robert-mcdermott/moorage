package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// A run's states.
const (
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	RunCanceled  = "canceled"
	// RunSkipped says that it was due, and the one before it was still going.
	RunSkipped = "skipped"
)

// Run is one running of a command of a release: a cron job's, or one someone asked for.
type Run struct {
	ID, App, Env string
	// Job is the cron job it belongs to, if any.
	Job     string
	Command string
	// Trigger is schedule or manual, and CreatedBy who asked, if someone did.
	Trigger   string
	CreatedBy string
	Version   int
	Status    string
	// ExitCode is what it exited with, if it did. HasExit says that it did.
	ExitCode   int
	HasExit    bool
	Error      string
	StartedAt  time.Time
	FinishedAt time.Time
}

// Finished says whether a run is over.
func (r Run) Finished() bool { return r.Status != RunRunning }

const runColumns = `r.id, a.name, e.name, r.job, r.command, r.trigger, r.created_by, r.version, r.status, r.exit_code, r.error, r.started_at, r.finished_at
	FROM runs r JOIN environments e ON e.id = r.env_id JOIN apps a ON a.id = e.app_id`

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var exit, finished sql.NullInt64
	var failure sql.NullString
	var started int64
	if err := row.Scan(&r.ID, &r.App, &r.Env, &r.Job, &r.Command, &r.Trigger, &r.CreatedBy, &r.Version, &r.Status, &exit, &failure, &started, &finished); err != nil {
		return r, err
	}
	r.ExitCode, r.HasExit, r.Error, r.StartedAt = int(exit.Int64), exit.Valid, failure.String, time.UnixMilli(started).UTC()
	if finished.Valid {
		r.FinishedAt = time.UnixMilli(finished.Int64).UTC()
	}
	return r, nil
}

// CreateRun records a run.
func (db *DB) CreateRun(ctx context.Context, r Run) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		var envID string
		err := tx.QueryRowContext(ctx, `SELECT e.id FROM environments e JOIN apps a ON a.id = e.app_id WHERE a.name = ? AND e.name = ?`, r.App, r.Env).Scan(&envID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no environment %q of app %q: %w", r.Env, r.App, ErrNotFound)
		}
		if err != nil {
			return err
		}
		var finished any
		if !r.FinishedAt.IsZero() {
			finished = r.FinishedAt.UnixMilli()
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO runs (id, env_id, job, command, trigger, created_by, version, status, started_at, finished_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, envID, r.Job, r.Command, r.Trigger, r.CreatedBy, r.Version, r.Status, r.StartedAt.UnixMilli(), finished)
		return err
	})
}

// FinishRun records how a run ended. exit is nil if it did not get as far as exiting.
func (db *DB) FinishRun(ctx context.Context, id, status string, exit *int, failure string, at time.Time) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		var code any
		if exit != nil {
			code = *exit
		}
		_, err := tx.ExecContext(ctx, `UPDATE runs SET status = ?, exit_code = ?, error = ?, finished_at = ? WHERE id = ?`, status, code, nullable(failure), at.UnixMilli(), id)
		return err
	})
}

// EndInterruptedRuns records that the runs still going when the server stopped are over.
func (db *DB) EndInterruptedRuns(ctx context.Context, at time.Time) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE runs SET status = ?, error = ?, finished_at = ? WHERE status = ?`, RunFailed, "the server stopped while it was running", at.UnixMilli(), RunRunning)
		return err
	})
}

// Run returns a run of an app.
func (db *DB) Run(ctx context.Context, app, id string) (Run, error) {
	r, err := scanRun(db.Reader.QueryRowContext(ctx, `SELECT `+runColumns+` WHERE r.id = ? AND a.name = ?`, id, app))
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("no run %q of app %q: %w", id, app, ErrNotFound)
	}
	return r, err
}

// Runs lists an environment's runs, newest first. An empty job is all of them.
func (db *DB) Runs(ctx context.Context, app, env, job string, limit int) ([]Run, error) {
	where, args := `a.name = ? AND e.name = ?`, []any{app, env}
	if job != "" {
		where += ` AND r.job = ?`
		args = append(args, job)
	}
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+runColumns+` WHERE `+where+` ORDER BY r.started_at DESC, r.id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunIDs lists the IDs of all an environment's runs, for what goes with it.
func (db *DB) RunIDs(ctx context.Context, app, env string) ([]string, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT r.id FROM runs r JOIN environments e ON e.id = r.env_id JOIN apps a ON a.id = e.app_id WHERE a.name = ? AND e.name = ?`, app, env)
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

// PruneRuns keeps a job's newest runs and, apart from those, its newest skipped ones. It returns the
// IDs of those it let go.
func (db *DB) PruneRuns(ctx context.Context, app, env, job string, keep, keepSkipped int) ([]string, error) {
	var gone []string
	err := db.write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT r.id, r.status FROM runs r JOIN environments e ON e.id = r.env_id JOIN apps a ON a.id = e.app_id
			WHERE a.name = ? AND e.name = ? AND r.job = ? AND r.status <> ? ORDER BY r.started_at DESC, r.id DESC`, app, env, job, RunRunning)
		if err != nil {
			return err
		}
		kept, keptSkipped := 0, 0
		for rows.Next() {
			var id, status string
			if err := rows.Scan(&id, &status); err != nil {
				rows.Close()
				return err
			}
			if status == RunSkipped {
				if keptSkipped++; keptSkipped > keepSkipped {
					gone = append(gone, id)
				}
			} else if kept++; kept > keep {
				gone = append(gone, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range gone {
			if _, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE id = ?`, id); err != nil {
				return err
			}
		}
		return nil
	})
	return gone, err
}

// LogIDs lists the deployments and runs of an environment, which is what the logs kept for an app are named by.
func (db *DB) LogIDs(ctx context.Context, app, env string) (map[string]bool, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT d.id FROM deployments d JOIN environments e ON e.id = d.env_id JOIN apps a ON a.id = e.app_id WHERE a.name = ? AND e.name = ?
		UNION ALL SELECT r.id FROM runs r JOIN environments e ON e.id = r.env_id JOIN apps a ON a.id = e.app_id WHERE a.name = ? AND e.name = ?`, app, env, app, env)
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
