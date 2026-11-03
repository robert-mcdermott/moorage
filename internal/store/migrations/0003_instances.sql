-- Running instances of process apps. A row is live until stopped_at is set;
-- a live instance's port is reserved (docs/10-data-model.md).
CREATE TABLE instances (
    id         TEXT PRIMARY KEY,
    env_id     TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    release_id TEXT NOT NULL REFERENCES releases (id) ON DELETE CASCADE,
    process    TEXT NOT NULL,
    idx        INTEGER NOT NULL,
    driver     TEXT NOT NULL,
    port       INTEGER,
    created_at INTEGER NOT NULL,
    stopped_at INTEGER
) STRICT;

CREATE UNIQUE INDEX instances_live_port ON instances (port) WHERE stopped_at IS NULL AND port IS NOT NULL;
CREATE INDEX instances_live ON instances (env_id) WHERE stopped_at IS NULL;
