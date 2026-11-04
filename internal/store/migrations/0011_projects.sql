-- Projects hold apps, and members hold roles in projects (spec/08-security.md,
-- "Authorisation"). Every app so far goes in a default project that everyone is a
-- maintainer of, so that a server keeps working as it did until someone says otherwise.
CREATE TABLE projects (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    -- The role of those who are not members: NULL for none.
    everyone_role    TEXT CHECK (everyone_role IN ('viewer', 'deployer', 'maintainer')),
    -- Whether viewers may read logs, which can hold sensitive data.
    viewers_see_logs INTEGER NOT NULL DEFAULT 1,
    created_at       INTEGER NOT NULL
) STRICT;

INSERT INTO projects (id, name, everyone_role, created_at)
VALUES ('prj_default', 'default', 'maintainer', unixepoch() * 1000);

CREATE TABLE project_members (
    project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('viewer', 'deployer', 'maintainer')),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (project_id, user_id)
) STRICT;

CREATE INDEX project_members_user ON project_members (user_id);

ALTER TABLE apps ADD COLUMN project_id TEXT REFERENCES projects (id);
UPDATE apps SET project_id = 'prj_default';
CREATE INDEX apps_project ON apps (project_id);

-- Who did what: every change through the API or the console, every sign-in, every
-- push and every secret revealed (spec/08-security.md, "Audit log"). By name: a
-- record outlives what it is about.
CREATE TABLE audit_log (
    id     TEXT PRIMARY KEY,
    at     INTEGER NOT NULL,
    actor  TEXT NOT NULL,
    ip     TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL,
    target TEXT NOT NULL DEFAULT '',
    result TEXT NOT NULL DEFAULT '',
    data   TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(data))
) STRICT;

CREATE INDEX audit_log_at ON audit_log (at);
