package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Instance is a running copy of a process of a release.
type Instance struct {
	ID        string
	EnvID     string
	ReleaseID string
	Process   string
	Index     int
	Driver    string
	// Port is 0 for a process that does not listen.
	Port      int
	CreatedAt time.Time
}

// LiveInstance is an instance not yet stopped, with what it runs.
type LiveInstance struct {
	Instance
	App     string
	Env     string
	Version int
	Build   Build
}

// CreateInstance records a new instance, reserving its port.
func (db *DB) CreateInstance(ctx context.Context, in Instance) error {
	var port any
	if in.Port != 0 {
		port = in.Port
	}
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO instances (id, env_id, release_id, process, idx, driver, port, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, in.ID, in.EnvID, in.ReleaseID, in.Process, in.Index, in.Driver, port, in.CreatedAt.UnixMilli())
		return err
	})
}

// StopInstance records that an instance stopped, releasing its port.
func (db *DB) StopInstance(ctx context.Context, id string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE instances SET stopped_at = ? WHERE id = ? AND stopped_at IS NULL`, time.Now().UnixMilli(), id)
		return err
	})
}

// LiveInstances lists the instances not yet stopped.
func (db *DB) LiveInstances(ctx context.Context) ([]LiveInstance, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT i.id, i.env_id, i.release_id, i.process, i.idx, i.driver, i.port, i.created_at,
			a.name, e.name, r.version,
			b.id, b.app_id, b.source_type, b.artifact_kind, b.artifact_ref, b.plan, b.files, b.size_bytes, b.created_at
		FROM instances i
		JOIN environments e ON e.id = i.env_id
		JOIN apps a ON a.id = e.app_id
		JOIN releases r ON r.id = i.release_id
		JOIN builds b ON b.id = r.build_id
		WHERE i.stopped_at IS NULL
		ORDER BY a.name, e.name, i.process, i.idx`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LiveInstance
	for rows.Next() {
		var li LiveInstance
		var port sql.NullInt64
		var created, bcreated int64
		var plan string
		b := &li.Build
		if err := rows.Scan(&li.ID, &li.EnvID, &li.ReleaseID, &li.Process, &li.Index, &li.Driver, &port, &created,
			&li.App, &li.Env, &li.Version,
			&b.ID, &b.AppID, &b.SourceType, &b.ArtifactKind, &b.ArtifactRef, &plan, &b.Files, &b.SizeBytes, &bcreated); err != nil {
			return nil, err
		}
		li.Port = int(port.Int64)
		li.CreatedAt = time.UnixMilli(created).UTC()
		b.Plan, b.CreatedAt = json.RawMessage(plan), time.UnixMilli(bcreated).UTC()
		out = append(out, li)
	}
	return out, rows.Err()
}

// LivePorts returns the ports reserved by live instances.
func (db *DB) LivePorts(ctx context.Context) (map[int]bool, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT port FROM instances WHERE stopped_at IS NULL AND port IS NOT NULL UNION SELECT port FROM services`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ports := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		ports[p] = true
	}
	return ports, rows.Err()
}

// EnvID returns an environment's ID.
func (db *DB) EnvID(ctx context.Context, app, env string) (string, error) {
	var id string
	err := db.Reader.QueryRowContext(ctx, `SELECT e.id FROM environments e JOIN apps a ON a.id = e.app_id
		WHERE a.name = ? AND e.name = ?`, app, env).Scan(&id)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return id, err
}
