-- Credentials for container registries, sealed with the master key.
CREATE TABLE registries (
    host       TEXT PRIMARY KEY,
    username   TEXT NOT NULL,
    sealed     BLOB NOT NULL,
    created_at INTEGER NOT NULL
) STRICT;
