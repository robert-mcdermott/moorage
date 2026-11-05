-- A token can be limited to previews, for a coding agent. Such a token makes previews that take nothing of production's.
ALTER TABLE api_tokens ADD COLUMN envs TEXT NOT NULL DEFAULT '';
ALTER TABLE environments ADD COLUMN inherits INTEGER NOT NULL DEFAULT 1;
