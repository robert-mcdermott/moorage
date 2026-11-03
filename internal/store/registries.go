package store

import (
	"context"
	"fmt"
	"time"
)

// Registry is a container registry's credentials: the password is sealed.
type Registry struct {
	Host      string
	Username  string
	Sealed    []byte
	CreatedAt time.Time
}

// Registries lists the registries the server has credentials for.
func (db *DB) Registries(ctx context.Context) ([]Registry, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT host, username, sealed, created_at FROM registries ORDER BY host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Registry
	for rows.Next() {
		var r Registry
		var created int64
		if err := rows.Scan(&r.Host, &r.Username, &r.Sealed, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRegistry stores a registry's credentials, replacing any it has.
func (db *DB) SetRegistry(ctx context.Context, r Registry) error {
	_, err := db.Writer.ExecContext(ctx, `INSERT INTO registries (host, username, sealed, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (host) DO UPDATE SET username = excluded.username, sealed = excluded.sealed, created_at = excluded.created_at`,
		r.Host, r.Username, r.Sealed, r.CreatedAt.UnixMilli())
	return err
}

// DeleteRegistry removes a registry's credentials.
func (db *DB) DeleteRegistry(ctx context.Context, host string) error {
	res, err := db.Writer.ExecContext(ctx, `DELETE FROM registries WHERE host = ?`, host)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("the server has no credentials for %s: %w", host, ErrNotFound)
	}
	return nil
}
