package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Event is something that happened. Data holds what the type says it holds, and
// never a secret's value.
type Event struct {
	ID    string
	At    time.Time
	Type  string
	App   string
	Env   string
	Actor string
	Data  map[string]any
}

// EventFilter says which events to list. Before and After are event IDs.
type EventFilter struct {
	App, Env string
	// Before lists older events than this, newest first; After lists newer ones,
	// oldest first.
	Before, After string
	Limit         int
}

// AddEvent records an event.
func (db *DB) AddEvent(ctx context.Context, e Event) error {
	data, err := json.Marshal(e.Data)
	if err != nil {
		return err
	}
	if e.Data == nil {
		data = []byte("{}")
	}
	_, err = db.Writer.ExecContext(ctx, `INSERT INTO events (id, at, type, app, env, actor, data) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.At.UnixMilli(), e.Type, e.App, e.Env, e.Actor, string(data))
	return err
}

// Events lists events: newest first, or with After, oldest first.
func (db *DB) Events(ctx context.Context, f EventFilter) ([]Event, error) {
	var where []string
	var args []any
	if f.App != "" {
		where = append(where, "app = ?")
		args = append(args, f.App)
	}
	if f.Env != "" {
		where = append(where, "env = ?")
		args = append(args, f.Env)
	}
	if f.Before != "" {
		where = append(where, "id < ?")
		args = append(args, f.Before)
	}
	order := "DESC"
	if f.After != "" {
		where = append(where, "id > ?")
		args = append(args, f.After)
		order = "ASC"
	}
	query := `SELECT id, at, type, app, env, actor, data FROM events`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY id " + order + " LIMIT ?"
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Reader.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		var data string
		if err := rows.Scan(&e.ID, &at, &e.Type, &e.App, &e.Env, &e.Actor, &data); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at).UTC()
		if err := json.Unmarshal([]byte(data), &e.Data); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneEvents removes events older than a time, and says how many.
func (db *DB) PruneEvents(ctx context.Context, before time.Time) (int64, error) {
	res, err := db.Writer.ExecContext(ctx, `DELETE FROM events WHERE at < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
