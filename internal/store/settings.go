package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Setting reads the setting key into v and reports whether it exists.
func (db *DB) Setting(ctx context.Context, key string, v any) (bool, error) {
	var raw string
	err := db.Reader.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), v)
}

// PutSetting stores v as the setting key.
func (db *DB) PutSetting(ctx context.Context, key string, v any, by string) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value, updated_by, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
			key, string(raw), nullString(by), time.Now().UnixMilli())
		return err
	})
}
