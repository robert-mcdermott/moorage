package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func open(t *testing.T) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(context.Background(), filepath.Join(dir, "moorage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, dir
}

func TestOpenUsesWAL(t *testing.T) {
	db, _ := open(t)
	var mode string
	if err := db.Writer.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	var fk int
	if err := db.Reader.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Error("foreign keys are off on read connections")
	}
}

func TestReaderCannotWrite(t *testing.T) {
	db, _ := open(t)
	if _, err := db.Reader.Exec(`CREATE TABLE t (x INTEGER)`); err == nil {
		t.Error("a read connection wrote to the database")
	}
}

func TestMigrateEmbedded(t *testing.T) {
	db, dir := open(t)
	ctx := context.Background()
	res, err := db.Migrate(ctx, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) == 0 || res.Snapshot != "" {
		t.Fatalf("first Migrate: applied %d, snapshot %q; want migrations and no snapshot", len(res.Applied), res.Snapshot)
	}
	if _, err := db.Writer.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('server.name', '"test"', 0)`); err != nil {
		t.Fatalf("settings table not usable: %v", err)
	}
	if _, err := db.Writer.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('bad', 'not json', 0)`); err == nil {
		t.Error("settings accepted a value that is not JSON")
	}
	again, err := db.Migrate(ctx, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Applied) != 0 || again.Snapshot != "" {
		t.Errorf("second Migrate did something: %+v", again)
	}
	v, err := db.SchemaVersion(ctx)
	if err != nil || v != res.Applied[len(res.Applied)-1].Version {
		t.Errorf("SchemaVersion = %d, %v", v, err)
	}
}

func TestMigrateSnapshotsBeforeUpgrading(t *testing.T) {
	db, dir := open(t)
	ctx := context.Background()
	backups := filepath.Join(dir, "backups")
	v1 := fstest.MapFS{
		"0001_first.sql": {Data: []byte(`CREATE TABLE a (x INTEGER) STRICT; INSERT INTO a VALUES (1);`)},
	}
	clock := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { clock = clock.Add(time.Second); return clock }
	if res, err := db.migrate(ctx, v1, backups, now); err != nil || len(res.Applied) != 1 || res.Snapshot != "" {
		t.Fatalf("first migrate: %+v, %v", res, err)
	}

	v2 := fstest.MapFS{
		"0001_first.sql":  v1["0001_first.sql"],
		"0002_second.sql": {Data: []byte(`ALTER TABLE a ADD COLUMN y TEXT; CREATE TABLE b (z INTEGER) STRICT;`)},
	}
	res, err := db.migrate(ctx, v2, backups, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 1 || res.Applied[0].Version != 2 {
		t.Errorf("applied %+v, want version 2 only", res.Applied)
	}
	if res.Snapshot == "" {
		t.Fatal("no snapshot before migrating an existing database")
	}
	// The snapshot is a database at the old schema, with the old data.
	snap, err := Open(ctx, res.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	var n int
	if err := snap.Reader.QueryRow(`SELECT count(*) FROM a`).Scan(&n); err != nil || n != 1 {
		t.Errorf("snapshot rows = %d, %v", n, err)
	}
	if _, err := snap.Reader.Exec(`SELECT y FROM a`); err == nil {
		t.Error("snapshot already has the new column")
	}
}

func TestMigrateKeepsFewSnapshots(t *testing.T) {
	db, dir := open(t)
	ctx := context.Background()
	backups := filepath.Join(dir, "backups")
	clock := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { clock = clock.Add(time.Minute); return clock }
	fsys := fstest.MapFS{"0001_v.sql": {Data: []byte(`CREATE TABLE t1 (x INTEGER);`)}}
	if _, err := db.migrate(ctx, fsys, backups, now); err != nil {
		t.Fatal(err)
	}
	for v := 2; v <= 6; v++ {
		fsys[fmt.Sprintf("%04d_v.sql", v)] = &fstest.MapFile{Data: []byte(fmt.Sprintf("CREATE TABLE t%d (x INTEGER);", v))}
		if _, err := db.migrate(ctx, fsys, backups, now); err != nil {
			t.Fatal(err)
		}
	}
	snaps, _ := filepath.Glob(filepath.Join(backups, "*.db"))
	if len(snaps) != keepSnapshots {
		t.Errorf("%d snapshots kept, want %d: %v", len(snaps), keepSnapshots, snaps)
	}
	for _, s := range snaps {
		if strings.Contains(s, "before-0002") || strings.Contains(s, "before-0003") {
			t.Errorf("old snapshot %s not removed", s)
		}
	}
}

func TestMigrateRefusesNewerDatabase(t *testing.T) {
	db, dir := open(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx, filepath.Join(dir, "backups")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer.Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (9999, 'future', 0)`); err != nil {
		t.Fatal(err)
	}
	_, err := db.Migrate(ctx, filepath.Join(dir, "backups"))
	if err == nil || !strings.Contains(err.Error(), "newer than this moor supports") {
		t.Errorf("Migrate on a newer database: %v", err)
	}
}

func TestMigrationFailureRollsBack(t *testing.T) {
	db, dir := open(t)
	ctx := context.Background()
	fsys := fstest.MapFS{
		"0001_ok.sql":     {Data: []byte(`CREATE TABLE ok (x INTEGER);`)},
		"0002_broken.sql": {Data: []byte(`CREATE TABLE half (x INTEGER); THIS IS NOT SQL;`)},
	}
	res, err := db.migrate(ctx, fsys, filepath.Join(dir, "backups"), time.Now)
	if err == nil || !strings.Contains(err.Error(), "0002_broken") {
		t.Fatalf("broken migration: %v", err)
	}
	if len(res.Applied) != 1 {
		t.Errorf("applied %d migrations, want 1", len(res.Applied))
	}
	if _, err := db.Reader.Exec(`SELECT * FROM half`); err == nil {
		t.Error("the broken migration's first statement was not rolled back")
	}
}

func TestLoadMigrationsRejectsBadNames(t *testing.T) {
	for _, name := range []string{"first.sql", "0001.sql", "x_first.sql", "0000_zero.sql"} {
		if _, err := loadMigrations(fstest.MapFS{name: {Data: []byte("SELECT 1;")}}); err == nil {
			t.Errorf("migration file %q accepted", name)
		}
	}
	dup := fstest.MapFS{"0001_a.sql": {Data: []byte("SELECT 1;")}, "0001_b.sql": {Data: []byte("SELECT 1;")}}
	if _, err := loadMigrations(dup); err == nil {
		t.Error("duplicate versions accepted")
	}
	ok := fstest.MapFS{"README.md": {Data: []byte("notes")}, "0002_b.sql": {Data: []byte("SELECT 2;")}, "0001_a.sql": {Data: []byte("SELECT 1;")}}
	m, err := loadMigrations(ok)
	if err != nil || len(m) != 2 || m[0].Version != 1 || m[1].Name != "b" {
		t.Errorf("loadMigrations = %+v, %v", m, err)
	}
}

func TestOpenFailsOnDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "moorage.db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if db, err := Open(context.Background(), filepath.Join(dir, "moorage.db")); err == nil {
		db.Close()
		t.Error("Open succeeded on a directory")
	}
}
