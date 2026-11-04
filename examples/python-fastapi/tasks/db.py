"""The database: SQLite, in the directory Moorage keeps for the app's data."""

import os
import sqlite3
from pathlib import Path


def database_path() -> Path:
    # MOORAGE_DATA_DIR survives deploys and rollbacks; the app's own
    # directory is read-only once it is built. Locally, use ./data.
    data = Path(os.environ.get("MOORAGE_DATA_DIR") or "data")
    data.mkdir(parents=True, exist_ok=True)
    return data / "tasks.db"


def connect() -> sqlite3.Connection:
    # isolation_level=None: every statement commits on its own, and
    # migrations open their transactions explicitly.
    conn = sqlite3.connect(database_path(), timeout=5, isolation_level=None)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA busy_timeout = 5000")
    return conn
