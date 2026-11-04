# Tasks: FastAPI with uv

A task list: a JSON API built with [FastAPI](https://fastapi.tiangolo.com),
run by uvicorn, and a web page (plain HTML, CSS and JavaScript) served by the
same app. Tasks are kept in SQLite.

What it shows:

- **A uv project.** Moorage installs the Python of `.python-version` and the
  dependencies locked in `uv.lock`, with uv, into a virtual environment.
- **Data that survives deploys.** The database lives in `$MOORAGE_DATA_DIR`
  ([`tasks/db.py`](tasks/db.py)); the app's own directory is read-only once
  built.
- **Migrations in the release command.** [`tasks/migrate.py`](tasks/migrate.py)
  runs once per deployment, before the new release takes traffic. If it
  fails, the previous release keeps serving.
- **A health check that means something.** `/healthz` reads the database, so
  a release whose database is missing or not migrated never takes traffic.
- **Variables.** `TASKS_TITLE` comes from `[env]` in
  [`moorage.toml`](moorage.toml) and can be changed on the server; the page's
  footer shows `MOORAGE_RELEASE` and `MOORAGE_INSTANCE`, which Moorage sets.

## Deploy it

From this directory, with a Moorage server
([installing Moorage](../../docs/install.md)):

```bash
moor deploy
```

The build is shown as it happens, then the URL, such as
`https://tasks.apps.example.com` (or `http://tasks.localhost:8080` on your
own computer). Then:

```bash
moor env set TASKS_TITLE="Team tasks"    # a new release, rolled out without downtime
```

```bash
moor rollback --with-config             # back to the previous release and its variables
```

```bash
moor logs -f
```

## Run it locally

With [uv](https://docs.astral.sh/uv/):

```bash
uv run python -m tasks.migrate
```

```bash
uv run uvicorn tasks.main:app --reload
```

Then open <http://127.0.0.1:8000>. Locally, the database goes to `./data`.

## The API

| Request | What it does |
|---|---|
| `GET /api/tasks` | Lists the tasks, open ones first |
| `POST /api/tasks` `{"title": "…"}` | Adds a task |
| `PATCH /api/tasks/{id}` `{"done": true}` | Changes a task's title or state |
| `DELETE /api/tasks/{id}` | Deletes a task |
| `DELETE /api/tasks` | Deletes the tasks that are done |
| `GET /api/info` | The title, release and instance |
| `GET /docs` | FastAPI's interactive documentation |

## Two instances

SQLite in WAL mode lets several instances share the database on one
server. To run two, add `instances = 2` under `[processes.web]` in
`moorage.toml` and deploy again: Moorage spreads the requests over both, and
the footer shows which instance answered.
