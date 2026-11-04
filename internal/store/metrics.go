package store

import (
	"context"
	"database/sql"
	"time"
)

// MetricRow is packed records of one series for one bucket of time.
type MetricRow struct {
	EnvID, Name, Part string
	Tier              int
	// Bucket is when it starts, in Unix seconds: an hour for tier 1, a day for tier 2.
	Bucket int64
	Data   []byte
}

// AppendMetrics adds records to their rows, in one transaction: a roll-up is written whole.
func (db *DB) AppendMetrics(ctx context.Context, rows []MetricRow) error {
	if len(rows) == 0 {
		return nil
	}
	return db.write(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO metric_points (env_id, name, part, tier, bucket, data) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (env_id, name, part, tier, bucket) DO UPDATE SET data = data || excluded.data`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range rows {
			if _, err := stmt.ExecContext(ctx, r.EnvID, r.Name, r.Part, r.Tier, r.Bucket, r.Data); err != nil {
				return err
			}
		}
		return nil
	})
}

// MetricRows reads a series' rows of a tier, for a name of an environment, with every part of it.
func (db *DB) MetricRows(ctx context.Context, envID, name string, tier int, from, to int64) ([]MetricRow, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT part, bucket, data FROM metric_points
		WHERE env_id = ? AND name = ? AND tier = ? AND bucket >= ? AND bucket <= ? ORDER BY part, bucket`, envID, name, tier, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetricRow
	for rows.Next() {
		r := MetricRow{EnvID: envID, Name: name, Tier: tier}
		if err := rows.Scan(&r.Part, &r.Bucket, &r.Data); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MetricRowsOfBucket reads all the rows of a tier for one bucket, to roll them up.
func (db *DB) MetricRowsOfBucket(ctx context.Context, tier int, bucket int64) ([]MetricRow, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT env_id, name, part, data FROM metric_points WHERE tier = ? AND bucket = ?`, tier, bucket)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetricRow
	for rows.Next() {
		r := MetricRow{Tier: tier, Bucket: bucket}
		if err := rows.Scan(&r.EnvID, &r.Name, &r.Part, &r.Data); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneMetrics lets go of a tier's rows older than a bucket.
func (db *DB) PruneMetrics(ctx context.Context, tier int, before int64) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM metric_points WHERE tier = ? AND bucket < ?`, tier, before)
		return err
	})
}

// ReleaseMark is a release that went out, for a chart to mark.
type ReleaseMark struct {
	At      time.Time
	Version int
	By      string
}

// ReleaseMarks lists an environment's releases over a range, the oldest first.
func (db *DB) ReleaseMarks(ctx context.Context, app, env string, from, to time.Time) ([]ReleaseMark, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT r.created_at, r.version, r.created_by FROM releases r
		JOIN environments e ON e.id = r.env_id JOIN apps a ON a.id = e.app_id
		WHERE a.name = ? AND e.name = ? AND r.created_at >= ? AND r.created_at <= ? ORDER BY r.created_at`, app, env, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReleaseMark
	for rows.Next() {
		var m ReleaseMark
		var at int64
		var by sql.NullString
		if err := rows.Scan(&at, &m.Version, &by); err != nil {
			return nil, err
		}
		m.At, m.By = time.UnixMilli(at).UTC(), by.String
		out = append(out, m)
	}
	return out, rows.Err()
}
