// Package store is Moorage's SQLite database: one writer connection (all
// writes go through it, in short transactions), a small pool of read-only
// connections, and embedded forward-only migrations.
package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // the pure-Go SQLite driver, registered as "sqlite"
)

// maxReaders bounds the read connections; SQLite in WAL mode serves them
// concurrently with the single writer.
const maxReaders = 4

// DB is an open database.
type DB struct {
	path string
	// Writer is the only connection that writes. BEGIN takes the write lock
	// immediately, so concurrent writers wait instead of failing later.
	Writer *sql.DB
	// Reader is a pool of connections that refuse to write.
	Reader *sql.DB
}

// Open opens (creating if needed) the database at path in WAL mode.
func Open(ctx context.Context, path string) (*DB, error) {
	w, err := sql.Open("sqlite", path+
		"?_pragma=busy_timeout(5000)"+
		"&_pragma=journal_mode(WAL)"+
		"&_pragma=synchronous(NORMAL)"+
		"&_pragma=foreign_keys(1)"+
		"&_pragma=temp_store(MEMORY)"+
		"&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	if err := w.PingContext(ctx); err != nil {
		w.Close()
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}

	r, err := sql.Open("sqlite", path+
		"?_pragma=busy_timeout(5000)"+
		"&_pragma=foreign_keys(1)"+
		"&_pragma=temp_store(MEMORY)"+
		"&_pragma=query_only(1)")
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(maxReaders)
	r.SetMaxIdleConns(maxReaders)
	if err := r.PingContext(ctx); err != nil {
		r.Close()
		w.Close()
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	return &DB{path: path, Writer: w, Reader: r}, nil
}

// Path returns the database file's path.
func (db *DB) Path() string { return db.path }

// Close lets SQLite refresh its query planner statistics, then closes every
// connection.
func (db *DB) Close() error {
	_, _ = db.Writer.Exec("PRAGMA optimize")
	rerr := db.Reader.Close()
	werr := db.Writer.Close()
	if werr != nil {
		return werr
	}
	return rerr
}
