package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrations embed.FS

// keepSnapshots is how many pre-migration snapshots are kept.
const keepSnapshots = 3

// Migration is one numbered schema change, from migrations/NNNN_name.sql.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// MigrateResult reports what Migrate did.
type MigrateResult struct {
	Applied []Migration
	// Snapshot is the copy of the database taken before migrating, if any.
	Snapshot string
}

// Migrate applies the migrations the database has not seen yet, each in its
// own transaction. Before migrating a database that already has a schema, it
// writes a snapshot of it into backupDir (with VACUUM INTO) so that a failed
// upgrade can be undone. It refuses a database migrated by a newer version of
// Moorage.
func (db *DB) Migrate(ctx context.Context, backupDir string) (MigrateResult, error) {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return MigrateResult{}, err
	}
	return db.migrate(ctx, sub, backupDir, time.Now)
}

// SchemaVersion returns the highest migration applied, or 0.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := db.Reader.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

func (db *DB) migrate(ctx context.Context, fsys fs.FS, backupDir string, now func() time.Time) (MigrateResult, error) {
	all, err := loadMigrations(fsys)
	if err != nil {
		return MigrateResult{}, err
	}
	if _, err := db.Writer.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at INTEGER NOT NULL
	) STRICT`); err != nil {
		return MigrateResult{}, fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[int]bool{}
	newest := 0
	rows, err := db.Writer.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return MigrateResult{}, err
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return MigrateResult{}, err
		}
		applied[v] = true
		newest = max(newest, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MigrateResult{}, err
	}

	known := 0
	if len(all) > 0 {
		known = all[len(all)-1].Version
	}
	if newest > known {
		return MigrateResult{}, fmt.Errorf("the database %s has schema version %d, newer than this moor supports (%d): upgrade moor, or restore the snapshot taken before the upgrade", db.path, newest, known)
	}

	var pending []Migration
	for _, m := range all {
		if !applied[m.Version] {
			pending = append(pending, m)
		}
	}
	var res MigrateResult
	if len(pending) == 0 {
		return res, nil
	}

	if len(applied) > 0 {
		snap, err := db.snapshot(ctx, backupDir, pending[0].Version, now())
		if err != nil {
			return res, fmt.Errorf("snapshot before migrating: %w", err)
		}
		res.Snapshot = snap
	}

	for _, m := range pending {
		if err := db.apply(ctx, m, now()); err != nil {
			return res, err
		}
		res.Applied = append(res.Applied, m)
	}
	return res, nil
}

func (db *DB) apply(ctx context.Context, m Migration, at time.Time) error {
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("migration %04d_%s: %w", m.Version, m.Name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		m.Version, m.Name, at.UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// snapshot copies the database into backupDir and keeps only the newest
// keepSnapshots snapshots.
func (db *DB) snapshot(ctx context.Context, backupDir string, nextVersion int, at time.Time) (string, error) {
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("moorage-%s-before-%04d.db", at.UTC().Format("20060102T150405.000Z"), nextVersion)
	path := filepath.Join(backupDir, name)
	if _, err := db.Writer.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return "", err
	}
	old, err := filepath.Glob(filepath.Join(backupDir, "moorage-*-before-*.db"))
	if err == nil && len(old) > keepSnapshots {
		sort.Strings(old) // names start with the time, so oldest first
		for _, p := range old[:len(old)-keepSnapshots] {
			os.Remove(p)
		}
	}
	return path, nil
}

// loadMigrations reads NNNN_name.sql files, sorted by version.
func loadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var out []Migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		num, name, ok := strings.Cut(strings.TrimSuffix(e.Name(), ".sql"), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 || name == "" {
			return nil, fmt.Errorf("migration file %q is not named NNNN_name.sql", e.Name())
		}
		if other, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", other, e.Name(), v)
		}
		seen[v] = e.Name()
		data, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: v, Name: name, SQL: string(data)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}
