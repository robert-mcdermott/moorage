package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The statuses of a backup.
const (
	BackupRunning = "running"
	BackupDone    = "done"
	BackupFailed  = "failed"
)

// BackupConfig says where backups go and how often.
type BackupConfig struct {
	// Destination is a path, or s3://bucket/prefix.
	Destination string
	// Endpoint, Region and AccessKey are an S3-compatible service's, and SealedSecret its secret key.
	Endpoint, Region, AccessKey string
	SealedSecret                []byte
	// Schedule is a cron expression: empty is only when asked.
	Schedule string
	// Keep is how many to keep when a scheduled one has finished.
	Keep      int
	UpdatedBy string
	UpdatedAt time.Time
}

// BackupConfig reads the configuration. Nothing having been said is not an error, and has no destination.
func (db *DB) BackupConfig(ctx context.Context) (BackupConfig, error) {
	var c BackupConfig
	var updated int64
	err := db.Reader.QueryRowContext(ctx, `SELECT destination, endpoint, region, access_key, sealed_secret, schedule, keep, updated_by, updated_at FROM backup_config WHERE id = 1`).
		Scan(&c.Destination, &c.Endpoint, &c.Region, &c.AccessKey, &c.SealedSecret, &c.Schedule, &c.Keep, &c.UpdatedBy, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return BackupConfig{Keep: 7}, nil
	}
	if err != nil {
		return c, err
	}
	c.UpdatedAt = time.UnixMilli(updated).UTC()
	return c, nil
}

// SetBackupConfig keeps the configuration.
func (db *DB) SetBackupConfig(ctx context.Context, c BackupConfig) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO backup_config (id, destination, endpoint, region, access_key, sealed_secret, schedule, keep, updated_by, updated_at)
			VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET destination = excluded.destination, endpoint = excluded.endpoint, region = excluded.region, access_key = excluded.access_key,
				sealed_secret = excluded.sealed_secret, schedule = excluded.schedule, keep = excluded.keep, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
			c.Destination, c.Endpoint, c.Region, c.AccessKey, c.SealedSecret, c.Schedule, c.Keep, c.UpdatedBy, c.UpdatedAt.UnixMilli())
		return err
	})
}

// Backup is one backup, or an attempt at one.
type Backup struct {
	ID                 string
	StartedAt          time.Time
	FinishedAt         time.Time
	Status             string
	Trigger, CreatedBy string
	Destination, Name  string
	SizeBytes          int64
	Error              string
}

// AddBackup notes that one has started.
func (db *DB) AddBackup(ctx context.Context, b Backup) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO backups (id, started_at, status, trigger, created_by, destination) VALUES (?, ?, ?, ?, ?, ?)`,
			b.ID, b.StartedAt.UnixMilli(), b.Status, b.Trigger, b.CreatedBy, b.Destination)
		return err
	})
}

// FinishBackup notes how one ended.
func (db *DB) FinishBackup(ctx context.Context, b Backup) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE backups SET finished_at = ?, status = ?, name = ?, size_bytes = ?, error = ? WHERE id = ?`,
			b.FinishedAt.UnixMilli(), b.Status, b.Name, b.SizeBytes, b.Error, b.ID)
		return err
	})
}

// Backups lists the latest backups, newest first.
func (db *DB) Backups(ctx context.Context, limit int) ([]Backup, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT id, started_at, finished_at, status, trigger, created_by, destination, name, size_bytes, error FROM backups ORDER BY started_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Backup
	for rows.Next() {
		var b Backup
		var started int64
		var finished sql.NullInt64
		if err := rows.Scan(&b.ID, &started, &finished, &b.Status, &b.Trigger, &b.CreatedBy, &b.Destination, &b.Name, &b.SizeBytes, &b.Error); err != nil {
			return nil, err
		}
		b.StartedAt = time.UnixMilli(started).UTC()
		if finished.Valid {
			b.FinishedAt = time.UnixMilli(finished.Int64).UTC()
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// EndInterruptedBackups marks what a server that stopped left going as having failed.
func (db *DB) EndInterruptedBackups(ctx context.Context, at time.Time) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE backups SET status = ?, finished_at = ?, error = 'the server stopped' WHERE status = ?`, BackupFailed, at.UnixMilli(), BackupRunning)
		return err
	})
}

// PruneBackupRecords keeps the record of the latest backups.
func (db *DB) PruneBackupRecords(ctx context.Context, keep int) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM backups WHERE id NOT IN (SELECT id FROM backups ORDER BY started_at DESC, id DESC LIMIT ?)`, keep)
		return err
	})
}

// SnapshotDatabase copies a database, in use or not, into a new file, and says which version of the
// schema it has. It has a connection of its own, so that writers go on meanwhile.
func SnapshotDatabase(ctx context.Context, path, into string) (int, error) {
	conn, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `VACUUM INTO ?`, into); err != nil {
		return 0, err
	}
	var version int
	err = conn.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&version)
	return version, err
}

// MarkBackupInSnapshot notes in a copy of the database that the backup holding it worked: once
// restored, that is true, and it would otherwise say that it was interrupted by the server stopping.
func MarkBackupInSnapshot(ctx context.Context, path, name string, at time.Time) error {
	conn, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.ExecContext(ctx, `UPDATE backups SET status = ?, name = ?, finished_at = ? WHERE status = ?`, BackupDone, name, at.UnixMilli(), BackupRunning)
	return err
}

// LatestSchema is the newest version of the schema that this program knows.
func LatestSchema() (int, error) {
	all, err := embeddedMigrations()
	if err != nil {
		return 0, err
	}
	latest := 0
	for _, m := range all {
		latest = max(latest, m.Version)
	}
	return latest, nil
}
