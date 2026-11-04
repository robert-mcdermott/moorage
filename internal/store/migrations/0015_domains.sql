-- Hostnames of the user's own, and paths on them (spec/06-ingress.md, "Hostnames" and
-- "Path-based routing"). A domain serves nothing until its DNS has been seen to point here.
CREATE TABLE domains (
    id           TEXT PRIMARY KEY,
    env_id       TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    hostname     TEXT NOT NULL,
    path_prefix  TEXT NOT NULL DEFAULT '',
    strip_prefix INTEGER NOT NULL DEFAULT 0,
    verified_at  INTEGER,
    checked_at   INTEGER,
    check_error  TEXT,
    created_by   TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    UNIQUE (hostname, path_prefix)
);
CREATE INDEX domains_env ON domains (env_id);
