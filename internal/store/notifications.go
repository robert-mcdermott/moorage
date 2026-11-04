package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// NotificationChannel is somewhere word of what happened can go.
type NotificationChannel struct {
	ID, Name, Kind string
	// Sealed is its address and any secret, as the master key sealed them.
	Sealed    []byte
	CreatedBy string
	CreatedAt time.Time
	// SentAt is when something last went, and Error why the last attempt did not.
	SentAt time.Time
	Error  string
}

const channelColumns = `id, name, kind, sealed, created_by, created_at, sent_at, error FROM notification_channels`

func scanChannel(row interface{ Scan(...any) error }) (NotificationChannel, error) {
	var c NotificationChannel
	var created int64
	var sent sql.NullInt64
	var failure sql.NullString
	if err := row.Scan(&c.ID, &c.Name, &c.Kind, &c.Sealed, &c.CreatedBy, &created, &sent, &failure); err != nil {
		return c, err
	}
	c.CreatedAt, c.Error = time.UnixMilli(created).UTC(), failure.String
	if sent.Valid {
		c.SentAt = time.UnixMilli(sent.Int64).UTC()
	}
	return c, nil
}

// ErrChannelTaken says that a channel of that name exists.
var ErrChannelTaken = errors.New("there is already a channel of that name")

// CreateNotificationChannel keeps a channel.
func (db *DB) CreateNotificationChannel(ctx context.Context, c NotificationChannel) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO notification_channels (id, name, kind, sealed, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			c.ID, c.Name, c.Kind, c.Sealed, c.CreatedBy, c.CreatedAt.UnixMilli())
		if err != nil && strings.Contains(err.Error(), "UNIQUE") {
			return ErrChannelTaken
		}
		return err
	})
}

// NotificationChannels lists the channels.
func (db *DB) NotificationChannels(ctx context.Context) ([]NotificationChannel, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+channelColumns+` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotificationChannel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotificationChannel returns a channel, by its ID or its name.
func (db *DB) NotificationChannel(ctx context.Context, ref string) (NotificationChannel, error) {
	c, err := scanChannel(db.Reader.QueryRowContext(ctx, `SELECT `+channelColumns+` WHERE id = ? OR name = ?`, ref, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return c, fmt.Errorf("no channel %q: %w", ref, ErrNotFound)
	}
	return c, err
}

// DeleteNotificationChannel removes a channel, and its rules.
func (db *DB) DeleteNotificationChannel(ctx context.Context, id string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM notification_channels WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("no channel %q: %w", id, ErrNotFound)
		}
		return nil
	})
}

// SetChannelResult notes how sending to a channel went.
func (db *DB) SetChannelResult(ctx context.Context, id string, at time.Time, failure string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		if failure == "" {
			_, err := tx.ExecContext(ctx, `UPDATE notification_channels SET sent_at = ?, error = NULL WHERE id = ?`, at.UnixMilli(), id)
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE notification_channels SET error = ? WHERE id = ?`, failure, id)
		return err
	})
}

// NotificationRule says which events go to which channel.
type NotificationRule struct {
	ID, ChannelID, ChannelName string
	// Project and App say what it is for, by name; neither is the whole server.
	Project, App string
	Events       []string
	CreatedBy    string
	CreatedAt    time.Time
}

// CreateNotificationRule keeps a rule, for a project or an app of those names, or the whole server.
func (db *DB) CreateNotificationRule(ctx context.Context, r NotificationRule) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		var project, app any
		if r.Project != "" {
			var id string
			if err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE name = ?`, r.Project).Scan(&id); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("no project %q: %w", r.Project, ErrNotFound)
				}
				return err
			}
			project = id
		}
		if r.App != "" {
			var id string
			if err := tx.QueryRowContext(ctx, `SELECT id FROM apps WHERE name = ?`, r.App).Scan(&id); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("no app %q: %w", r.App, ErrNotFound)
				}
				return err
			}
			app = id
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO notification_rules (id, channel_id, project_id, app_id, events, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.ChannelID, project, app, strings.Join(r.Events, ","), r.CreatedBy, r.CreatedAt.UnixMilli())
		return err
	})
}

// NotificationRules lists the rules.
func (db *DB) NotificationRules(ctx context.Context) ([]NotificationRule, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT r.id, r.channel_id, c.name, COALESCE(p.name, ''), COALESCE(a.name, ''), r.events, r.created_by, r.created_at
		FROM notification_rules r JOIN notification_channels c ON c.id = r.channel_id
		LEFT JOIN projects p ON p.id = r.project_id LEFT JOIN apps a ON a.id = r.app_id ORDER BY r.created_at, r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotificationRule
	for rows.Next() {
		var r NotificationRule
		var events string
		var created int64
		if err := rows.Scan(&r.ID, &r.ChannelID, &r.ChannelName, &r.Project, &r.App, &events, &r.CreatedBy, &created); err != nil {
			return nil, err
		}
		r.Events, r.CreatedAt = strings.Split(events, ","), time.UnixMilli(created).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// NotificationRule returns a rule.
func (db *DB) NotificationRule(ctx context.Context, id string) (NotificationRule, error) {
	rules, err := db.NotificationRules(ctx)
	if err != nil {
		return NotificationRule{}, err
	}
	for _, r := range rules {
		if r.ID == id {
			return r, nil
		}
	}
	return NotificationRule{}, fmt.Errorf("no rule %q: %w", id, ErrNotFound)
}

// DeleteNotificationRule removes a rule.
func (db *DB) DeleteNotificationRule(ctx context.Context, id string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM notification_rules WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("no rule %q: %w", id, ErrNotFound)
		}
		return nil
	})
}

// Notification is something someone is told in the console.
type Notification struct {
	ID, UserID, Kind, App, Env, Summary, Link string
	CreatedAt, ReadAt                         time.Time
}

// AddNotifications tells people something.
func (db *DB) AddNotifications(ctx context.Context, list []Notification) error {
	if len(list) == 0 {
		return nil
	}
	return db.write(ctx, func(tx *sql.Tx) error {
		for _, n := range list {
			if _, err := tx.ExecContext(ctx, `INSERT INTO notifications (id, user_id, kind, app, env, summary, link, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				n.ID, n.UserID, n.Kind, n.App, n.Env, n.Summary, n.Link, n.CreatedAt.UnixMilli()); err != nil {
				return err
			}
		}
		return nil
	})
}

// Notifications lists what someone has been told, newest first.
func (db *DB) Notifications(ctx context.Context, userID string, limit int) ([]Notification, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT id, user_id, kind, app, env, summary, link, created_at, read_at FROM notifications WHERE user_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notification
	for rows.Next() {
		var n Notification
		var created int64
		var read sql.NullInt64
		if err := rows.Scan(&n.ID, &n.UserID, &n.Kind, &n.App, &n.Env, &n.Summary, &n.Link, &created, &read); err != nil {
			return nil, err
		}
		n.CreatedAt = time.UnixMilli(created).UTC()
		if read.Valid {
			n.ReadAt = time.UnixMilli(read.Int64).UTC()
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ReadNotifications marks all of what someone has been told as read.
func (db *DB) ReadNotifications(ctx context.Context, userID string, at time.Time) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE notifications SET read_at = ? WHERE user_id = ? AND read_at IS NULL`, at.UnixMilli(), userID)
		return err
	})
}

// PruneNotifications lets go of what is older than a time.
func (db *DB) PruneNotifications(ctx context.Context, before time.Time) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM notifications WHERE created_at < ?`, before.UnixMilli())
		return err
	})
}
