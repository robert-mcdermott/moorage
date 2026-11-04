package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Domain is a hostname of the user's own, or a path on one, attached to an environment.
type Domain struct {
	ID, App, Env, EnvID string
	Hostname            string
	// Prefix, if set, is the path it serves, such as /reports. StripPrefix takes it away from
	// what the app hears.
	Prefix      string
	StripPrefix bool
	// VerifiedAt says when its DNS was seen to point here: until then it serves nothing.
	VerifiedAt time.Time
	CheckedAt  time.Time
	CheckError string
	CreatedBy  string
	CreatedAt  time.Time
}

// Verified says whether a domain serves.
func (d Domain) Verified() bool { return !d.VerifiedAt.IsZero() }

// Address is a domain as someone would say it.
func (d Domain) Address() string { return d.Hostname + d.Prefix }

// ErrDomainTaken says that a hostname and path are attached already.
var ErrDomainTaken = errors.New("that domain is attached already")

const domainColumns = `d.id, a.name, e.name, e.id, d.hostname, d.path_prefix, d.strip_prefix, d.verified_at, d.checked_at, d.check_error, d.created_by, d.created_at
	FROM domains d JOIN environments e ON e.id = d.env_id JOIN apps a ON a.id = e.app_id`

func scanDomain(row interface{ Scan(...any) error }) (Domain, error) {
	var d Domain
	var strip int
	var created int64
	var verified, checked sql.NullInt64
	var checkError sql.NullString
	if err := row.Scan(&d.ID, &d.App, &d.Env, &d.EnvID, &d.Hostname, &d.Prefix, &strip, &verified, &checked, &checkError, &d.CreatedBy, &created); err != nil {
		return d, err
	}
	d.StripPrefix, d.CheckError, d.CreatedAt = strip != 0, checkError.String, time.UnixMilli(created).UTC()
	if verified.Valid {
		d.VerifiedAt = time.UnixMilli(verified.Int64).UTC()
	}
	if checked.Valid {
		d.CheckedAt = time.UnixMilli(checked.Int64).UTC()
	}
	return d, nil
}

func (db *DB) listDomains(ctx context.Context, where string, args ...any) ([]Domain, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+domainColumns+` WHERE `+where+` ORDER BY d.hostname, d.path_prefix`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Domain
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Domains lists an environment's domains.
func (db *DB) Domains(ctx context.Context, app, env string) ([]Domain, error) {
	return db.listDomains(ctx, `a.name = ? AND e.name = ?`, app, env)
}

// AllDomains lists the domains of every environment.
func (db *DB) AllDomains(ctx context.Context) ([]Domain, error) {
	return db.listDomains(ctx, `1 = 1`)
}

// VerifiedHostname says whether a hostname has a domain that serves.
func (db *DB) VerifiedHostname(ctx context.Context, hostname string) (bool, error) {
	var n int
	err := db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM domains WHERE hostname = ? AND verified_at IS NOT NULL`, hostname).Scan(&n)
	return n > 0, err
}

// AddDomain attaches a domain to an environment.
func (db *DB) AddDomain(ctx context.Context, app, env string, d Domain) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		var envID string
		err := tx.QueryRowContext(ctx, `SELECT e.id FROM environments e JOIN apps a ON a.id = e.app_id WHERE a.name = ? AND e.name = ?`, app, env).Scan(&envID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no environment %q of app %q: %w", env, app, ErrNotFound)
		}
		if err != nil {
			return err
		}
		strip := 0
		if d.StripPrefix {
			strip = 1
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO domains (id, env_id, hostname, path_prefix, strip_prefix, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			d.ID, envID, d.Hostname, d.Prefix, strip, d.CreatedBy, d.CreatedAt.UnixMilli())
		if err != nil && strings.Contains(err.Error(), "UNIQUE") {
			return ErrDomainTaken
		}
		return err
	})
}

// SetDomainChecked notes what looking at a domain's DNS found: empty is that it points here.
func (db *DB) SetDomainChecked(ctx context.Context, id string, at time.Time, problem string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		if problem == "" {
			_, err := tx.ExecContext(ctx, `UPDATE domains SET checked_at = ?, check_error = NULL, verified_at = COALESCE(verified_at, ?) WHERE id = ?`, at.UnixMilli(), at.UnixMilli(), id)
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE domains SET checked_at = ?, check_error = ? WHERE id = ?`, at.UnixMilli(), problem, id)
		return err
	})
}

// RemoveDomain detaches a domain from an environment.
func (db *DB) RemoveDomain(ctx context.Context, app, env, hostname, prefix string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM domains WHERE hostname = ? AND path_prefix = ? AND env_id =
			(SELECT e.id FROM environments e JOIN apps a ON a.id = e.app_id WHERE a.name = ? AND e.name = ?)`, hostname, prefix, app, env)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("no domain %s%s of %s: %w", hostname, prefix, env, ErrNotFound)
		}
		return nil
	})
}
