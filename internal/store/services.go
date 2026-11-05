// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// What a service should be doing.
const (
	ServiceRunning = "running"
	ServiceStopped = "stopped"
)

// Service is something that an app declares, which Moorage runs and connects for it. Each environment has its own.
type Service struct {
	ID, EnvID, App, Env string
	Name, Type, Version string
	Image, Digest       string
	// Engine is the path of the program that holds its image and volume.
	Engine, Volume string
	// OldVolume is what it held before an upgrade, until someone says that the upgrade was good.
	OldVolume string
	Port      int
	// Sealed is its password. Its address is made from this when an instance starts.
	Sealed []byte
	// EnvVar is the variable that gives an app its address.
	EnvVar    string
	Memory    int64
	State     string
	CreatedBy string
	CreatedAt time.Time
}

const serviceColumns = `s.id, s.env_id, a.name, e.name, s.name, s.type, s.version, s.image, s.digest, s.engine, s.volume, s.old_volume, s.port,
	s.password_enc, s.env_var, s.memory, s.state, s.created_by, s.created_at`

const serviceFrom = ` FROM services s JOIN environments e ON e.id = s.env_id JOIN apps a ON a.id = e.app_id`

func scanService(row interface{ Scan(...any) error }) (Service, error) {
	var s Service
	var created int64
	if err := row.Scan(&s.ID, &s.EnvID, &s.App, &s.Env, &s.Name, &s.Type, &s.Version, &s.Image, &s.Digest, &s.Engine, &s.Volume, &s.OldVolume, &s.Port,
		&s.Sealed, &s.EnvVar, &s.Memory, &s.State, &s.CreatedBy, &created); err != nil {
		return s, err
	}
	s.CreatedAt = time.UnixMilli(created).UTC()
	return s, nil
}

// CreateService records a service of an environment. It is an error if the environment has one of that name.
func (db *DB) CreateService(ctx context.Context, s Service) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		envID, _, err := envState(ctx, tx, s.App, s.Env)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO services (id, env_id, name, type, version, image, digest, engine, volume, port, password_enc, env_var, memory, state, created_by, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.ID, envID, s.Name, s.Type, s.Version, s.Image, s.Digest, s.Engine, s.Volume, s.Port, s.Sealed, s.EnvVar, s.Memory, s.State, s.CreatedBy, s.CreatedAt.UnixMilli())
		return err
	})
}

// Service reads one service of an environment.
func (db *DB) Service(ctx context.Context, app, env, name string) (Service, error) {
	s, err := scanService(db.Reader.QueryRowContext(ctx, `SELECT `+serviceColumns+serviceFrom+` WHERE a.name = ? AND e.name = ? AND s.name = ?`, app, env, name))
	if errors.Is(err, sql.ErrNoRows) {
		return s, fmt.Errorf("no service %q in %s of %s: %w", name, env, app, ErrNotFound)
	}
	return s, err
}

// Services lists the services of an environment, by name.
func (db *DB) Services(ctx context.Context, app, env string) ([]Service, error) {
	return db.queryServices(ctx, ` WHERE a.name = ? AND e.name = ? ORDER BY s.name`, app, env)
}

// AppServices lists the services of every environment of an app.
func (db *DB) AppServices(ctx context.Context, app string) ([]Service, error) {
	return db.queryServices(ctx, ` WHERE a.name = ? ORDER BY e.name, s.name`, app)
}

// AllServices lists every service on this server.
func (db *DB) AllServices(ctx context.Context) ([]Service, error) {
	return db.queryServices(ctx, ` ORDER BY a.name, e.name, s.name`)
}

func (db *DB) queryServices(ctx context.Context, where string, args ...any) ([]Service, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+serviceColumns+serviceFrom+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Service
	for rows.Next() {
		s, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SetServiceState says what a service should be doing.
func (db *DB) SetServiceState(ctx context.Context, id, state string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE services SET state = ? WHERE id = ?`, state, id)
		return err
	})
}

// SetServiceDigest records which image a service runs.
func (db *DB) SetServiceDigest(ctx context.Context, id, digest string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE services SET digest = ? WHERE id = ?`, digest, id)
		return err
	})
}

// DeleteService forgets a service.
func (db *DB) DeleteService(ctx context.Context, id string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM services WHERE id = ?`, id)
		return err
	})
}

// SetServiceImage records what a service runs now, and what it holds, after an update or an upgrade.
func (db *DB) SetServiceImage(ctx context.Context, id, version, image, digest, volume, oldVolume string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE services SET version = ?, image = ?, digest = ?, volume = ?, old_volume = ? WHERE id = ?`, version, image, digest, volume, oldVolume, id)
		return err
	})
}

// EnvironmentFromFork says that an environment is the preview of a pull request from a fork, whose code is not this repository's.
func (db *DB) EnvironmentFromFork(ctx context.Context, app, env string) (bool, error) {
	var fork int
	err := db.Reader.QueryRowContext(ctx, `SELECT count(*) FROM forge_pulls p JOIN apps a ON a.id = p.app_id WHERE a.name = ? AND p.env = ? AND p.from_fork = 1`, app, env).Scan(&fork)
	if err != nil {
		return false, err
	}
	return fork > 0, nil
}
