package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/robert-mcdermott/moorage/internal/ids"
)

// Var is a variable set on the server for an app.
type Var struct {
	Name string
	// Value is the value of a variable that is not secret.
	Value string
	// Sealed is a secret's value, encrypted.
	Sealed []byte
	Secret bool
	// Build makes the variable available to builds too.
	Build bool
	// AllEnvs reports a variable set for every environment of the app,
	// which an environment's own variable of the same name overrides.
	AllEnvs   bool
	UpdatedBy string
	UpdatedAt time.Time
}

// ConfigVar is a variable as a release runs with it.
type ConfigVar struct {
	Name   string `json:"name"`
	Value  string `json:"value,omitempty"`
	Sealed []byte `json:"sealed,omitempty"`
	Secret bool   `json:"secret,omitempty"`
	Build  bool   `json:"build,omitempty"`
	// Scope is the ID of the environment the variable was set for, or ""
	// for every environment; a sealed value is bound to it.
	Scope string `json:"scope,omitempty"`
}

// VarChange sets and unsets variables of one scope: an environment, or
// with AllEnvs, every environment of the app.
type VarChange struct {
	Set     []Var
	Unset   []string
	AllEnvs bool
	By      string
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Scope returns the IDs of an app and of one of its environments.
func (db *DB) Scope(ctx context.Context, app, env string) (appID, envID string, err error) {
	err = db.Reader.QueryRowContext(ctx, `SELECT a.id, e.id FROM apps a JOIN environments e ON e.app_id = a.id
		WHERE a.name = ? AND e.name = ?`, app, env).Scan(&appID, &envID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("no environment %q of app %q: %w", env, app, ErrNotFound)
	}
	return appID, envID, err
}

// Vars lists the variables set for an environment: its own and those set
// for every environment, sorted by name (an environment's own first).
func (db *DB) Vars(ctx context.Context, app, env string) ([]Var, error) {
	appID, envID, err := db.Scope(ctx, app, env)
	if err != nil {
		return nil, err
	}
	rows, err := db.Reader.QueryContext(ctx, `SELECT name, value, value_enc, secret, build, env_id IS NULL, updated_by, updated_at
		FROM vars WHERE app_id = ? AND (env_id = ? OR env_id IS NULL) ORDER BY name, env_id IS NULL`, appID, envID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Var
	for rows.Next() {
		var v Var
		var value, by sql.NullString
		var updated int64
		if err := rows.Scan(&v.Name, &value, &v.Sealed, &v.Secret, &v.Build, &v.AllEnvs, &by, &updated); err != nil {
			return nil, err
		}
		v.Value, v.UpdatedBy, v.UpdatedAt = value.String, by.String, time.UnixMilli(updated).UTC()
		out = append(out, v)
	}
	return out, rows.Err()
}

// ChangeVars applies a change to an environment's variables (or, with
// AllEnvs, the app's) at once. Secrets in Set must be sealed already. It
// returns the names to unset that were not set.
func (db *DB) ChangeVars(ctx context.Context, app, env string, ch VarChange) ([]string, error) {
	appID, envID, err := db.Scope(ctx, app, env)
	if err != nil {
		return nil, err
	}
	var scope any = envID
	if ch.AllEnvs {
		scope = nil
	}
	var missing []string
	err = db.write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		del := func(name string) (int64, error) {
			res, err := tx.ExecContext(ctx, `DELETE FROM vars WHERE app_id = ? AND env_id IS ? AND name = ?`, appID, scope, name)
			if err != nil {
				return 0, err
			}
			return res.RowsAffected()
		}
		for _, name := range ch.Unset {
			n, err := del(name)
			if err != nil {
				return err
			}
			if n == 0 {
				missing = append(missing, name)
			}
		}
		for _, v := range ch.Set {
			if _, err := del(v.Name); err != nil {
				return err
			}
			var value any = v.Value
			var sealed any
			if v.Secret {
				value, sealed = nil, v.Sealed
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO vars (id, app_id, env_id, name, value, value_enc, secret, build, updated_by, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, ids.New(ids.Var), appID, scope, v.Name, value, sealed, v.Secret, v.Build,
				nullString(ch.By), now); err != nil {
				return err
			}
		}
		return nil
	})
	return missing, err
}

// Config returns the variables an environment's next release would run
// with: those set for every environment, overridden by its own.
func (db *DB) Config(ctx context.Context, app, env string) ([]ConfigVar, error) {
	appID, envID, err := db.Scope(ctx, app, env)
	if err != nil {
		return nil, err
	}
	return config(ctx, db.Reader, appID, envID)
}

func config(ctx context.Context, q querier, appID, envID string) ([]ConfigVar, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, value, value_enc, secret, build, coalesce(env_id, '')
		FROM vars WHERE app_id = ? AND (env_id = ? OR env_id IS NULL)`, appID, envID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byName := map[string]ConfigVar{}
	for rows.Next() {
		var v ConfigVar
		var value sql.NullString
		if err := rows.Scan(&v.Name, &value, &v.Sealed, &v.Secret, &v.Build, &v.Scope); err != nil {
			return nil, err
		}
		v.Value = value.String
		if prev, ok := byName[v.Name]; ok && prev.Scope != "" {
			continue // the environment's own variable wins
		}
		byName[v.Name] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ConfigVar, 0, len(byName))
	for _, v := range byName {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ReleaseConfig returns the variables a release ran with.
func (db *DB) ReleaseConfig(ctx context.Context, app, env string, version int) ([]ConfigVar, error) {
	var data string
	err := db.Reader.QueryRowContext(ctx, `SELECT r.config FROM releases r JOIN environments e ON e.id = r.env_id
		JOIN apps a ON a.id = e.app_id WHERE a.name = ? AND e.name = ? AND r.version = ?`, app, env, version).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s has no release v%d: %w", app, version, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	var cfg []ConfigVar
	return cfg, json.Unmarshal([]byte(data), &cfg)
}

// DataKeys returns every app's sealed data key, by app ID.
func (db *DB) DataKeys(ctx context.Context) (map[string][]byte, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT app_id, sealed FROM data_keys`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var id string
		var sealed []byte
		if err := rows.Scan(&id, &sealed); err != nil {
			return nil, err
		}
		out[id] = sealed
	}
	return out, rows.Err()
}

// AddDataKey stores an app's sealed data key unless it has one, and
// returns the one stored.
func (db *DB) AddDataKey(ctx context.Context, appID string, sealed []byte) ([]byte, error) {
	var stored []byte
	err := db.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO data_keys (app_id, sealed, created_at) VALUES (?, ?, ?)
			ON CONFLICT (app_id) DO NOTHING`, appID, sealed, time.Now().UnixMilli()); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT sealed FROM data_keys WHERE app_id = ?`, appID).Scan(&stored)
	})
	return stored, err
}

// HasSealed reports whether anything has been sealed with the master key: a
// secret's data key, or a registry's password.
func (db *DB) HasSealed(ctx context.Context) (bool, error) {
	var n int
	err := db.Reader.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM data_keys) + (SELECT count(*) FROM registries)`).Scan(&n)
	return n > 0, err
}
