package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Formation returns how many instances of each process were asked for on the server.
// A process it does not name runs as many as its manifest says.
func (db *DB) Formation(ctx context.Context, app, env string) (map[string]int, error) {
	var data string
	err := db.Reader.QueryRowContext(ctx, `SELECT e.formation FROM environments e JOIN apps a ON a.id = e.app_id
		WHERE a.name = ? AND e.name = ?`, app, env).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no environment %q of app %q: %w", env, app, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	formation := map[string]int{}
	return formation, json.Unmarshal([]byte(data), &formation)
}

// SetFormation stores how many instances of each process were asked for.
func (db *DB) SetFormation(ctx context.Context, app, env string, formation map[string]int) error {
	data, err := json.Marshal(formation)
	if err != nil {
		return err
	}
	return db.write(ctx, func(tx *sql.Tx) error {
		envID, _, err := envState(ctx, tx, app, env)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE environments SET formation = ? WHERE id = ?`, string(data), envID)
		return err
	})
}
