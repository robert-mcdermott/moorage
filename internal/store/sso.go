package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SSOConfig says how people sign in through an OpenID Connect provider.
type SSOConfig struct {
	Issuer, ClientID string
	SealedSecret     []byte
	// Label is what the button says. Scopes are asked for beyond openid, email and profile.
	Label, Scopes string
	// EmailDomains lists whose addresses may make an account, one per line: empty is anyone's.
	EmailDomains string
	// Provision makes an account for someone who has none, the first time they sign in.
	Provision bool
	// GroupClaim names the claim that lists someone's groups, and GroupRules says what those mean.
	GroupClaim, GroupRules string
	// PasswordsOff leaves signing in to the provider.
	PasswordsOff bool
	// TrustEmails lets an address join someone to the account that has it, when the provider does not say that
	// it is verified. A provider that says that it is not is never believed.
	TrustEmails bool
	UpdatedBy    string
	UpdatedAt    time.Time
}

// Configured says whether it has been set up.
func (c SSOConfig) Configured() bool { return c.Issuer != "" && c.ClientID != "" }

// SSOConfig reads the configuration. Nothing having been said is not an error.
func (db *DB) SSOConfig(ctx context.Context) (SSOConfig, error) {
	var c SSOConfig
	var updated int64
	err := db.Reader.QueryRowContext(ctx, `SELECT issuer, client_id, sealed_secret, label, scopes, email_domains, provision, group_claim, group_rules, passwords_off, trust_emails, updated_by, updated_at FROM sso_config WHERE id = 1`).
		Scan(&c.Issuer, &c.ClientID, &c.SealedSecret, &c.Label, &c.Scopes, &c.EmailDomains, &c.Provision, &c.GroupClaim, &c.GroupRules, &c.PasswordsOff, &c.TrustEmails, &c.UpdatedBy, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return SSOConfig{}, nil
	}
	if err != nil {
		return c, err
	}
	c.UpdatedAt = time.UnixMilli(updated).UTC()
	return c, nil
}

// SetSSOConfig keeps the configuration.
func (db *DB) SetSSOConfig(ctx context.Context, c SSOConfig) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO sso_config (id, issuer, client_id, sealed_secret, label, scopes, email_domains, provision, group_claim, group_rules, passwords_off, trust_emails, updated_by, updated_at)
			VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET issuer = excluded.issuer, client_id = excluded.client_id, sealed_secret = excluded.sealed_secret, label = excluded.label, scopes = excluded.scopes,
				email_domains = excluded.email_domains, provision = excluded.provision, group_claim = excluded.group_claim, group_rules = excluded.group_rules, passwords_off = excluded.passwords_off, trust_emails = excluded.trust_emails,
				updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
			c.Issuer, c.ClientID, c.SealedSecret, c.Label, c.Scopes, c.EmailDomains, c.Provision, c.GroupClaim, c.GroupRules, c.PasswordsOff, c.TrustEmails, c.UpdatedBy, c.UpdatedAt.UnixMilli())
		return err
	})
}

// DeleteSSOConfig turns it off.
func (db *DB) DeleteSSOConfig(ctx context.Context) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM sso_config`)
		return err
	})
}

// UserByIdentity finds who a provider's subject is.
func (db *DB) UserByIdentity(ctx context.Context, issuer, subject string) (User, error) {
	var id string
	err := db.Reader.QueryRowContext(ctx, `SELECT user_id FROM user_identities WHERE issuer = ? AND subject = ?`, issuer, subject).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("no such identity: %w", ErrNotFound)
	}
	if err != nil {
		return User{}, err
	}
	return db.UserByID(ctx, id)
}

// LinkIdentity says who a provider's subject is.
func (db *DB) LinkIdentity(ctx context.Context, issuer, subject, userID string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO user_identities (issuer, subject, user_id) VALUES (?, ?, ?)`, issuer, subject, userID)
		return err
	})
}

// UsersWithIdentity says who signs in through a provider.
func (db *DB) UsersWithIdentity(ctx context.Context) (map[string]bool, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT DISTINCT user_id FROM user_identities`)
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
