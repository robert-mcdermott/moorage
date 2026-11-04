-- Where a deployment's source came from: for a push, the ref, the commit
-- and the first line of its message.
ALTER TABLE deployments ADD COLUMN source_ref TEXT;
ALTER TABLE deployments ADD COLUMN source_commit TEXT;
ALTER TABLE deployments ADD COLUMN source_message TEXT;
