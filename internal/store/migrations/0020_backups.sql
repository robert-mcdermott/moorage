-- Where backups go and how often. There is one row, once someone has said.
CREATE TABLE backup_config (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    destination TEXT NOT NULL,
    endpoint   TEXT NOT NULL DEFAULT '',
    region     TEXT NOT NULL DEFAULT '',
    access_key TEXT NOT NULL DEFAULT '',
    sealed_secret BLOB,
    schedule   TEXT NOT NULL DEFAULT '',
    keep       INTEGER NOT NULL DEFAULT 7,
    updated_by TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

-- What has been backed up.
CREATE TABLE backups (
    id          TEXT PRIMARY KEY,
    started_at  INTEGER NOT NULL,
    finished_at INTEGER,
    status      TEXT NOT NULL,
    trigger     TEXT NOT NULL,
    created_by  TEXT NOT NULL DEFAULT '',
    destination TEXT NOT NULL,
    name        TEXT NOT NULL DEFAULT '',
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX backups_started ON backups (started_at);
