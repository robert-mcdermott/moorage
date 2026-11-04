-- One-off runs of a release: cron jobs, and commands someone asked for
-- (spec/05-runtime.md, "Cron jobs and one-off runs").
CREATE TABLE runs (
    id          TEXT PRIMARY KEY,
    env_id      TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    job         TEXT NOT NULL DEFAULT '',
    command     TEXT NOT NULL,
    trigger     TEXT NOT NULL,
    created_by  TEXT NOT NULL DEFAULT '',
    version     INTEGER NOT NULL DEFAULT 0,
    status      TEXT NOT NULL,
    exit_code   INTEGER,
    error       TEXT,
    started_at  INTEGER NOT NULL,
    finished_at INTEGER
);
CREATE INDEX runs_env_job ON runs (env_id, job, started_at);
