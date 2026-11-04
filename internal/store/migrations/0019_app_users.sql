-- Which operating system user each app runs as, where apps are isolated. A row outlives its app, so
-- that a user is never given again to another app that might find what the first one left.
CREATE TABLE app_users (
    app_id TEXT PRIMARY KEY,
    uid    INTEGER NOT NULL UNIQUE
);
