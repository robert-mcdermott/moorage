package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// UserTOTP is someone's second factor.
type UserTOTP struct {
	UserID      string
	Sealed      []byte
	CreatedAt   time.Time
	ConfirmedAt time.Time
	LastStep    int64
}

// Confirmed says that they have shown that they can use it, and so that it counts.
func (t UserTOTP) Confirmed() bool { return !t.ConfirmedAt.IsZero() }

// SetUserTOTP keeps a new secret, not yet confirmed, in place of what there was, unless it is confirmed.
func (db *DB) SetUserTOTP(ctx context.Context, t UserTOTP) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO user_totp (user_id, sealed, created_at) VALUES (?, ?, ?)
			ON CONFLICT (user_id) DO UPDATE SET sealed = excluded.sealed, created_at = excluded.created_at WHERE confirmed_at IS NULL`,
			t.UserID, t.Sealed, t.CreatedAt.UnixMilli())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errors.New("two-factor sign-in is already on")
		}
		return nil
	})
}

// GetUserTOTP reads someone's second factor. Having none is store.ErrNotFound.
func (db *DB) GetUserTOTP(ctx context.Context, userID string) (UserTOTP, error) {
	t := UserTOTP{UserID: userID}
	var created int64
	var confirmed sql.NullInt64
	err := db.Reader.QueryRowContext(ctx, `SELECT sealed, created_at, confirmed_at, last_step FROM user_totp WHERE user_id = ?`, userID).
		Scan(&t.Sealed, &created, &confirmed, &t.LastStep)
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("no second factor: %w", ErrNotFound)
	}
	if err != nil {
		return t, err
	}
	t.CreatedAt = time.UnixMilli(created).UTC()
	if confirmed.Valid {
		t.ConfirmedAt = time.UnixMilli(confirmed.Int64).UTC()
	}
	return t, nil
}

// ConfirmUserTOTP makes it count, with the codes that get them in if they lose it.
func (db *DB) ConfirmUserTOTP(ctx context.Context, userID string, step int64, at time.Time, recoveryHashes []string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE user_totp SET confirmed_at = ?, last_step = ? WHERE user_id = ?`, at.UnixMilli(), step, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_recovery_codes WHERE user_id = ?`, userID); err != nil {
			return err
		}
		for _, h := range recoveryHashes {
			if _, err := tx.ExecContext(ctx, `INSERT INTO user_recovery_codes (user_id, code_hash) VALUES (?, ?)`, userID, h); err != nil {
				return err
			}
		}
		return nil
	})
}

// UseTOTPStep notes that a step of time has been used, and says false if it or a later one was: a code
// works once, however quickly someone else sees it.
func (db *DB) UseTOTPStep(ctx context.Context, userID string, step int64) (bool, error) {
	used := false
	err := db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE user_totp SET last_step = ? WHERE user_id = ? AND last_step < ?`, step, userID, step)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		used = n == 1
		return err
	})
	return used, err
}

// UseRecoveryCode spends a code, and says false if there is no such code left.
func (db *DB) UseRecoveryCode(ctx context.Context, userID, hash string, at time.Time) (bool, error) {
	used := false
	err := db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE user_recovery_codes SET used_at = ? WHERE user_id = ? AND code_hash = ? AND used_at IS NULL`, at.UnixMilli(), userID, hash)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		used = n == 1
		return err
	})
	return used, err
}

// RecoveryCodesLeft says how many someone has left.
func (db *DB) RecoveryCodesLeft(ctx context.Context, userID string) (int, error) {
	var n int
	err := db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_recovery_codes WHERE user_id = ? AND used_at IS NULL`, userID).Scan(&n)
	return n, err
}

// DeleteUserTOTP turns it off.
func (db *DB) DeleteUserTOTP(ctx context.Context, userID string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_recovery_codes WHERE user_id = ?`, userID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM user_totp WHERE user_id = ?`, userID)
		return err
	})
}

// UsersWithTOTP says who has a second factor that counts.
func (db *DB) UsersWithTOTP(ctx context.Context) (map[string]bool, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT user_id FROM user_totp WHERE confirmed_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
