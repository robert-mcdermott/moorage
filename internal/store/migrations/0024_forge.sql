-- Where an app's code is kept, when that is on a forge: what it deploys from, and how the forge tells of changes.
CREATE TABLE forge_connections (
    app_id         TEXT PRIMARY KEY REFERENCES apps (id) ON DELETE CASCADE,
    kind           TEXT NOT NULL,
    url            TEXT NOT NULL,
    api_url        TEXT NOT NULL DEFAULT '',
    repo           TEXT NOT NULL DEFAULT '',
    default_branch TEXT NOT NULL DEFAULT 'main',
    -- Whoever connected it said which branch is production, so what the forge says does not change that.
    branch_pinned  INTEGER NOT NULL DEFAULT 0,
    hook_id        TEXT NOT NULL UNIQUE,
    sealed_secret  BLOB NOT NULL,
    sealed_token   BLOB,
    previews       INTEGER NOT NULL DEFAULT 1,
    poll           INTEGER NOT NULL DEFAULT 0,
    -- When it last looked at all the branches. Until it has, what it finds there is not news.
    looked_at      INTEGER NOT NULL DEFAULT 0,
    last_event_at  INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT NOT NULL DEFAULT '',
    created_by     TEXT NOT NULL,
    created_at     INTEGER NOT NULL
) STRICT;

-- What is being deployed from a branch now, whose check would say so for ever if the server stopped here.
CREATE TABLE forge_deploying (
    app_id TEXT NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    commit_sha TEXT NOT NULL,
    PRIMARY KEY (app_id, commit_sha)
) STRICT;

-- The pull requests it has heard of. Code from someone who could not push there is only built once someone approves
-- that commit, which is why this says which one.
CREATE TABLE forge_pulls (
    app_id       TEXT NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    number       INTEGER NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    author       TEXT NOT NULL DEFAULT '',
    from_fork    INTEGER NOT NULL DEFAULT 0,
    head_sha     TEXT NOT NULL,
    state        TEXT NOT NULL,
    env          TEXT NOT NULL DEFAULT '',
    error        TEXT NOT NULL DEFAULT '',
    comment_id   TEXT NOT NULL DEFAULT '',
    approved_sha TEXT NOT NULL DEFAULT '',
    approved_by  TEXT NOT NULL DEFAULT '',
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (app_id, number)
) STRICT;
