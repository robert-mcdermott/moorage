-- Named environments and previews (spec/04-build-and-deploy.md, "Preview environments").
ALTER TABLE environments ADD COLUMN branch TEXT;
ALTER TABLE environments ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
ALTER TABLE environments ADD COLUMN locked_by TEXT;
ALTER TABLE environments ADD COLUMN locked_at INTEGER;
ALTER TABLE environments ADD COLUMN last_active_at INTEGER;

-- A variable previews do not inherit from production.
ALTER TABLE vars ADD COLUMN production_only INTEGER NOT NULL DEFAULT 0;
