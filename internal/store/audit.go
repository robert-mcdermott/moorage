package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// AuditEntry is a record of who did what.
type AuditEntry struct {
	ID string
	At time.Time
	// Actor is who: an email address, or the server's local administrator.
	Actor string
	IP    string
	// Action is what: a route, or a name such as "signin".
	Action string
	// Target is what to.
	Target string
	// Result is how it went: "ok", "refused" or "failed".
	Result string
	Data   map[string]any
}

// AuditFilter says which records to list, newest first.
type AuditFilter struct {
	// Actor and Target keep the records with exactly these; Action those that have it
	// as part of theirs.
	Actor, Action, Target string
	// Before lists older records than this one.
	Before string
	Limit  int
}

// AddAudit keeps a record.
func (db *DB) AddAudit(ctx context.Context, e AuditEntry) error {
	data, err := json.Marshal(e.Data)
	if err != nil {
		return err
	}
	if e.Data == nil {
		data = []byte("{}")
	}
	_, err = db.Writer.ExecContext(ctx, `INSERT INTO audit_log (id, at, actor, ip, action, target, result, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.At.UnixMilli(), e.Actor, e.IP, e.Action, e.Target, e.Result, string(data))
	return err
}

// Audit lists records, newest first.
func (db *DB) Audit(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	var where []string
	var args []any
	if f.Actor != "" {
		where = append(where, "actor = ?")
		args = append(args, f.Actor)
	}
	if f.Action != "" {
		where = append(where, "instr(action, ?) > 0")
		args = append(args, f.Action)
	}
	if f.Target != "" {
		where = append(where, "target = ?")
		args = append(args, f.Target)
	}
	if f.Before != "" {
		where = append(where, "id < ?")
		args = append(args, f.Before)
	}
	query := `SELECT id, at, actor, ip, action, target, result, data FROM audit_log`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Reader.QueryContext(ctx, query+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		var data string
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.IP, &e.Action, &e.Target, &e.Result, &data); err != nil {
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

// PruneAudit removes records older than a time, and says how many.
func (db *DB) PruneAudit(ctx context.Context, before time.Time) (int64, error) {
	var n int64
	err := db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM audit_log WHERE at < ?`, before.UnixMilli())
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}
