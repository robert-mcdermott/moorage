package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SSHKey is a public key that may push over SSH.
type SSHKey struct {
	ID          string
	Name        string
	Fingerprint string // SHA256:…
	PublicKey   string // authorized_keys format
	CreatedAt   time.Time
	LastUsedAt  time.Time
}

// KeyExistsError is returned when a key's name or the key itself is
// already registered.
type KeyExistsError struct{ Name string }

func (e *KeyExistsError) Error() string {
	return fmt.Sprintf("that key is already registered, as %s", e.Name)
}

// AddSSHKey registers a key.
func (db *DB) AddSSHKey(ctx context.Context, k SSHKey) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT name FROM ssh_keys WHERE name = ? OR fingerprint = ?`, k.Name, k.Fingerprint).Scan(&existing)
		if err == nil {
			return &KeyExistsError{existing}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO ssh_keys (id, name, fingerprint, public_key, created_at) VALUES (?, ?, ?, ?, ?)`,
			k.ID, k.Name, k.Fingerprint, k.PublicKey, k.CreatedAt.UnixMilli())
		return err
	})
}

// SSHKeys lists the registered keys, oldest first.
func (db *DB) SSHKeys(ctx context.Context) ([]SSHKey, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT id, name, fingerprint, public_key, created_at, last_used_at FROM ssh_keys ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SSHKey
	for rows.Next() {
		k, err := scanSSHKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// SSHKeyByFingerprint returns the key with fingerprint, or ErrNotFound.
func (db *DB) SSHKeyByFingerprint(ctx context.Context, fingerprint string) (SSHKey, error) {
	k, err := scanSSHKey(db.Reader.QueryRowContext(ctx, `SELECT id, name, fingerprint, public_key, created_at, last_used_at
		FROM ssh_keys WHERE fingerprint = ?`, fingerprint))
	if errors.Is(err, sql.ErrNoRows) {
		return SSHKey{}, ErrNotFound
	}
	return k, err
}

// RemoveSSHKey removes the key named, or with the fingerprint, which, and
// returns it; ErrNotFound if there is none.
func (db *DB) RemoveSSHKey(ctx context.Context, which string) (SSHKey, error) {
	var k SSHKey
	err := db.write(ctx, func(tx *sql.Tx) error {
		var err error
		k, err = scanSSHKey(tx.QueryRowContext(ctx, `SELECT id, name, fingerprint, public_key, created_at, last_used_at
			FROM ssh_keys WHERE name = ? OR fingerprint = ? OR id = ?`, which, which, which))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM ssh_keys WHERE id = ?`, k.ID)
		return err
	})
	return k, err
}

// TouchSSHKey records that a key was just used.
func (db *DB) TouchSSHKey(ctx context.Context, id string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE ssh_keys SET last_used_at = ? WHERE id = ?`, time.Now().UnixMilli(), id)
		return err
	})
}

func scanSSHKey(row interface{ Scan(...any) error }) (SSHKey, error) {
	var k SSHKey
	var created int64
	var used sql.NullInt64
	if err := row.Scan(&k.ID, &k.Name, &k.Fingerprint, &k.PublicKey, &created, &used); err != nil {
		return SSHKey{}, err
	}
	k.CreatedAt = time.UnixMilli(created).UTC()
	if used.Valid {
		k.LastUsedAt = time.UnixMilli(used.Int64).UTC()
	}
	return k, nil
}
