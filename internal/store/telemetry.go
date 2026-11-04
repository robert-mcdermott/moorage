package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// TelemetrySink is somewhere that logs and metrics are sent.
type TelemetrySink struct {
	ID, Name, Kind, Address string
	SealedHeaders           []byte
	Logs, Metrics           bool
	// AppID and App are the one app it is for, if it is for one.
	AppID, App string
	LastSentAt time.Time
	LastError  string
	Dropped    int64
	CreatedBy  string
	CreatedAt  time.Time
}

const sinkColumns = `s.id, s.name, s.kind, s.address, s.sealed_headers, s.logs, s.metrics, COALESCE(s.app_id, ''), COALESCE(a.name, ''), s.last_sent_at, s.last_error, s.dropped, s.created_by, s.created_at`

func scanSink(row interface{ Scan(...any) error }) (TelemetrySink, error) {
	var s TelemetrySink
	var sent, created int64
	err := row.Scan(&s.ID, &s.Name, &s.Kind, &s.Address, &s.SealedHeaders, &s.Logs, &s.Metrics, &s.AppID, &s.App, &sent, &s.LastError, &s.Dropped, &s.CreatedBy, &created)
	if err != nil {
		return s, err
	}
	if sent != 0 {
		s.LastSentAt = time.UnixMilli(sent).UTC()
	}
	s.CreatedAt = time.UnixMilli(created).UTC()
	return s, nil
}

// TelemetrySinks lists them, by name.
func (db *DB) TelemetrySinks(ctx context.Context) ([]TelemetrySink, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+sinkColumns+` FROM telemetry_sinks s LEFT JOIN apps a ON a.id = s.app_id ORDER BY s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TelemetrySink
	for rows.Next() {
		s, err := scanSink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// TelemetrySink returns one by name, or ErrNotFound.
func (db *DB) TelemetrySink(ctx context.Context, name string) (TelemetrySink, error) {
	s, err := scanSink(db.Reader.QueryRowContext(ctx, `SELECT `+sinkColumns+` FROM telemetry_sinks s LEFT JOIN apps a ON a.id = s.app_id WHERE s.name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	return s, err
}

func orNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SetTelemetrySink keeps one, by its name. What has been sent so far stays on record.
func (db *DB) SetTelemetrySink(ctx context.Context, s TelemetrySink) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO telemetry_sinks (id, name, kind, address, sealed_headers, logs, metrics, app_id, created_by, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (name) DO UPDATE SET kind = excluded.kind, address = excluded.address, sealed_headers = excluded.sealed_headers, logs = excluded.logs,
				metrics = excluded.metrics, app_id = excluded.app_id, last_error = ''`,
			s.ID, s.Name, s.Kind, s.Address, s.SealedHeaders, s.Logs, s.Metrics, orNull(s.AppID), s.CreatedBy, s.CreatedAt.UnixMilli())
		return err
	})
}

// DeleteTelemetrySink removes one, by name, or says ErrNotFound.
func (db *DB) DeleteTelemetrySink(ctx context.Context, name string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM telemetry_sinks WHERE name = ?`, name)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// NoteTelemetrySink records how sending to one has been going. Sent is when something last got there, if it did.
func (db *DB) NoteTelemetrySink(ctx context.Context, id string, sent time.Time, problem string, dropped int64) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		at := int64(0)
		if !sent.IsZero() {
			at = sent.UnixMilli()
		}
		_, err := tx.ExecContext(ctx, `UPDATE telemetry_sinks SET last_sent_at = MAX(last_sent_at, ?), last_error = ?, dropped = dropped + ? WHERE id = ?`,
			at, problem, dropped, id)
		return err
	})
}

// TelemetryCursor says how far log files have been read, or nothing if they have not been.
func (db *DB) TelemetryCursor(ctx context.Context) (time.Time, error) {
	var at int64
	err := db.Reader.QueryRowContext(ctx, `SELECT cursor FROM telemetry_cursor WHERE id = 1`).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, at).UTC(), nil
}

// SetTelemetryCursor notes how far log files have been read.
func (db *DB) SetTelemetryCursor(ctx context.Context, at time.Time) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO telemetry_cursor (id, cursor) VALUES (1, ?) ON CONFLICT (id) DO UPDATE SET cursor = excluded.cursor`, at.UnixNano())
		return err
	})
}

// AlertThreshold says when to tell someone about how an environment is doing.
type AlertThreshold struct {
	ID, EnvID, App, Env string
	Metric              string
	Above               float64
	Minutes             int
	// Firing says that it has been crossed, and has not come right.
	Firing    bool
	CreatedBy string
	CreatedAt time.Time
}

const thresholdColumns = `t.id, t.env_id, a.name, e.name, t.metric, t.above, t.minutes, t.firing, t.created_by, t.created_at`
const thresholdJoin = ` FROM alert_thresholds t JOIN environments e ON e.id = t.env_id JOIN apps a ON a.id = e.app_id`

func scanThreshold(row interface{ Scan(...any) error }) (AlertThreshold, error) {
	var t AlertThreshold
	var created int64
	err := row.Scan(&t.ID, &t.EnvID, &t.App, &t.Env, &t.Metric, &t.Above, &t.Minutes, &t.Firing, &t.CreatedBy, &created)
	if err != nil {
		return t, err
	}
	t.CreatedAt = time.UnixMilli(created).UTC()
	return t, nil
}

func (db *DB) thresholds(ctx context.Context, where string, args ...any) ([]AlertThreshold, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+thresholdColumns+thresholdJoin+where+` ORDER BY t.created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertThreshold
	for rows.Next() {
		t, err := scanThreshold(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AllAlertThresholds lists every environment's.
func (db *DB) AllAlertThresholds(ctx context.Context) ([]AlertThreshold, error) {
	return db.thresholds(ctx, "")
}

// AlertThresholds lists an environment's.
func (db *DB) AlertThresholds(ctx context.Context, envID string) ([]AlertThreshold, error) {
	return db.thresholds(ctx, ` WHERE t.env_id = ?`, envID)
}

// ErrThresholdExists says that an environment has that threshold already.
var ErrThresholdExists = errors.New("that threshold is there already")

// AddAlertThreshold keeps one, or says ErrThresholdExists if it has it already.
func (db *DB) AddAlertThreshold(ctx context.Context, t AlertThreshold) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO alert_thresholds (id, env_id, metric, above, minutes, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (env_id, metric, above) DO NOTHING`, t.ID, t.EnvID, t.Metric, t.Above, t.Minutes, t.CreatedBy, t.CreatedAt.UnixMilli())
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return ErrThresholdExists
		}
		return nil
	})
}

// SetAlertFiring notes whether a threshold has been crossed.
func (db *DB) SetAlertFiring(ctx context.Context, id string, firing bool) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE alert_thresholds SET firing = ? WHERE id = ?`, firing, id)
		return err
	})
}

// DeleteAlertThreshold removes one of an environment's, or says ErrNotFound.
func (db *DB) DeleteAlertThreshold(ctx context.Context, envID, id string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM alert_thresholds WHERE id = ? AND env_id = ?`, id, envID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return ErrNotFound
		}
		return nil
	})
}
