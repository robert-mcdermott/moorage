"""Brings the database up to date. Moorage runs this as the release command,
once per deployment, before the new release takes traffic:

    python -m tasks.migrate

Each migration runs once, in order; PRAGMA user_version records how many
have run. Add new migrations at the end and never change old ones.
"""

from contextlib import closing

from .db import connect, database_path

MIGRATIONS = [
    """
    CREATE TABLE tasks (
        id         INTEGER PRIMARY KEY,
        title      TEXT    NOT NULL,
        done       INTEGER NOT NULL DEFAULT 0,
        created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
    )
    """,
    "CREATE INDEX tasks_by_state ON tasks (done, created_at)",
]


def migrate() -> None:
    with closing(connect()) as conn:
        # WAL lets the web instances read while another one writes.
        conn.execute("PRAGMA journal_mode = WAL")
        version = conn.execute("PRAGMA user_version").fetchone()[0]
        for number, sql in enumerate(MIGRATIONS[version:], start=version + 1):
            conn.execute("BEGIN IMMEDIATE")
            conn.execute(sql)
            conn.execute(f"PRAGMA user_version = {number}")
            conn.execute("COMMIT")
            print(f"applied migration {number}")
    print(f"{database_path()} is at version {len(MIGRATIONS)}")


if __name__ == "__main__":
    migrate()
