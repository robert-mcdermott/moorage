-- SSH keys that may push (spec/10-data-model.md). Until accounts arrive
-- (wave 2, slice 3) every key is the administrator's; accounts give each
-- key its user.
CREATE TABLE ssh_keys (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    fingerprint  TEXT NOT NULL UNIQUE,
    public_key   TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER
) STRICT;

-- Where a deployment's source came from: for a push, the ref, the commit
-- and the first line of its message.
ALTER TABLE deployments ADD COLUMN source_ref TEXT;
ALTER TABLE deployments ADD COLUMN source_commit TEXT;
ALTER TABLE deployments ADD COLUMN source_message TEXT;
