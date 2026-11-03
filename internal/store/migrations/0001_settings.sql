-- Runtime server settings (bootstrap settings live in server.toml).
-- Values are JSON; timestamps are Unix milliseconds (UTC).
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL CHECK (json_valid(value)),
    updated_by TEXT,
    updated_at INTEGER NOT NULL
) STRICT;
