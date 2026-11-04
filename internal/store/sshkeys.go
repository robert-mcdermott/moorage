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
	ID string
	// UserID is empty for a key of the server's local administrator.
	UserID      string
	Name        string
	Fingerprint string // SHA256:…
	PublicKey   string // authorized_keys format
	CreatedAt   time.Time
	LastUsedAt  time.Time
}

// KeyExistsError is returned for a key that is registered already (by
// anyone: a key identifies one owner), or a name its owner already uses.
type KeyExistsError struct{ Name string }

func (e *KeyExistsError) Error() string {
	if e.Name == "" {
		return "that key is already registered, to another account"
	}
	return fmt.Sprintf("that key or that name is already registered, as %s", e.Name)
}

// AddSSHKey registers a key.
func (db *DB) AddSSHKey(ctx context.Context, k SSHKey) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		var owner, existing string
		err := tx.QueryRowContext(ctx, `SELECT coalesce(user_id, ''), name FROM ssh_keys WHERE fingerprint = ?
			OR (coalesce(user_id, '') = ? AND name = ?)`, k.Fingerprint, k.UserID, k.Name).Scan(&owner, &existing)
		if err == nil {
			if owner != k.UserID {
				existing = "" // not this owner's to know
			}
			return &KeyExistsError{existing}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO ssh_keys (id, user_id, name, fingerprint, public_key, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			k.ID, nullString(k.UserID), k.Name, k.Fingerprint, k.PublicKey, k.CreatedAt.UnixMilli())
		return err
	})
}

// SSHKeys lists the keys of a user (or, with "", of the local administrator),
// oldest first.
func (db *DB) SSHKeys(ctx context.Context, userID string) ([]SSHKey, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT id, user_id, name, fingerprint, public_key, created_at, last_used_at FROM ssh_keys
		WHERE coalesce(user_id, '') = ? ORDER BY created_at, name`, userID)
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
	k, err := scanSSHKey(db.Reader.QueryRowContext(ctx, `SELECT id, user_id, name, fingerprint, public_key, created_at, last_used_at
		FROM ssh_keys WHERE fingerprint = ?`, fingerprint))
	if errors.Is(err, sql.ErrNoRows) {
		return SSHKey{}, ErrNotFound
	}
	return k, err
}

// RemoveSSHKey removes the owner's key named, or with the fingerprint, which,
// and returns it; ErrNotFound if they have none.
func (db *DB) RemoveSSHKey(ctx context.Context, userID, which string) (SSHKey, error) {
	var k SSHKey
	err := db.write(ctx, func(tx *sql.Tx) error {
		var err error
		k, err = scanSSHKey(tx.QueryRowContext(ctx, `SELECT id, user_id, name, fingerprint, public_key, created_at, last_used_at
			FROM ssh_keys WHERE coalesce(user_id, '') = ? AND (name = ? OR fingerprint = ? OR id = ?)`, userID, which, which, which))
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
	var user sql.NullString
	var created int64
	var used sql.NullInt64
	if err := row.Scan(&k.ID, &user, &k.Name, &k.Fingerprint, &k.PublicKey, &created, &used); err != nil {
		return SSHKey{}, err
	}
	k.UserID = user.String
	k.CreatedAt = time.UnixMilli(created).UTC()
	if used.Valid {
		k.LastUsedAt = time.UnixMilli(used.Int64).UTC()
	}
	return k, nil
}
