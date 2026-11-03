-- Deployments are persisted jobs: each records how one release came to be,
-- or why it did not (docs/04-build-and-deploy.md, "Pipeline"). The build of
-- a failed deployment may have no row in builds, so build_id is not a
-- foreign key.
CREATE TABLE deployments (
    id          TEXT PRIMARY KEY,
    env_id      TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    source_type TEXT NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('queued', 'building', 'releasing', 'rolling_out', 'live', 'failed', 'superseded', 'canceled')),
    build_id    TEXT,
    release_id  TEXT REFERENCES releases (id) ON DELETE SET NULL,
    error       TEXT,
    -- The last lines of the output that explain a failure, as a JSON array.
    output      TEXT CHECK (output IS NULL OR json_valid(output)),
    created_by  TEXT,
    created_at  INTEGER NOT NULL,
    started_at  INTEGER,
    finished_at INTEGER
) STRICT;

CREATE INDEX deployments_env ON deployments (env_id, created_at);
CREATE INDEX deployments_active ON deployments (status) WHERE status NOT IN ('live', 'failed', 'superseded', 'canceled');

-- A build whose files were removed by garbage collection keeps its row (its
-- releases still refer to it) with the time it was collected.
ALTER TABLE builds ADD COLUMN collected_at INTEGER;
