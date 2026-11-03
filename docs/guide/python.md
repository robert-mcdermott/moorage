# Deploying a Python app

Moorage builds Python apps with [uv](https://docs.astral.sh/uv/): it installs
the Python version the project asks for and the project's dependencies into
a virtual environment, in the directory the app runs from.

## The app

A FastAPI app, for example, with its dependencies in `pyproject.toml` and
locked in `uv.lock` (`uv lock`):

```toml
[project]
name = "hello"
version = "0.1.0"
requires-python = ">=3.12"
dependencies = ["fastapi", "uvicorn"]
```

```python
# main.py
from fastapi import FastAPI

app = FastAPI()


@app.get("/healthz")
def health():
    return "ok"


@app.get("/")
def hello():
    return {"hello": "from Python"}
```

Moorage finds the app object — in `main.py`, `app.py`, `server.py`,
`asgi.py`, `wsgi.py` or `api.py`, or in those files of a package at the top
(a directory with an `__init__.py`) — and runs
`uvicorn main:app --host 127.0.0.1 --port $PORT`. A Django or Flask app with
`gunicorn` among its dependencies is found the same way, and a project with
a single `[project.scripts]` entry runs that script. Otherwise, say what to
run in `moorage.toml`:

```toml
[app]
name = "hello"

[processes.web]
command = "uvicorn main:app --host 127.0.0.1 --port $PORT --proxy-headers"
health = { path = "/healthz" }

[release]
command = "alembic upgrade head"
```

## Deploy

```bash
moor manifest explain
```

```bash
moor deploy
```

## How it builds

- **Python version:** `.python-version`, or else `requires-python`, or else
  the newest release; `[python].version` overrides them. A partial version
  (`3.13`) means its newest patch release.
- **Dependencies:** `uv sync --locked --no-dev` with a `uv.lock` (a lock that
  does not match `pyproject.toml` fails the build), `uv sync --no-dev`
  without one (with a warning), or for a `requirements.txt`, a virtual
  environment and `uv pip install -r requirements.txt`. `[python].groups` and
  `[python].extras` install more.
- The virtual environment's `bin` comes first on `PATH`, so `python`,
  `uvicorn` and the project's scripts are found without `uv run`.
  `VIRTUAL_ENV` is set, and `PYTHONUNBUFFERED=1`, so that output reaches the
  logs at once.
- uv's cache is shared between builds.

## The release command

`[release].command` runs once per deployment, after the build and before the
new release takes traffic, with the release's variables: database migrations
go there. If it fails, the deployment fails and the previous release keeps
serving. It does not run on a rollback or a change of variables.
`[release].on_create` runs once, after an environment's first release (to
seed data).

## Two traps of `http.server`

Apps built on the standard library's `http.server`:

- Use `ThreadingHTTPServer`, not `HTTPServer`: the latter answers one
  connection at a time, and an idle connection from Moorage's proxy then
  holds up every other request.
- `HTTPServer` looks up the name of the address it binds. On some machines
  that reverse lookup of 127.0.0.1 takes half a minute, and each new instance
  takes as long to start. Override `server_bind`, as the sample in
  `testdata/apps/python` does.

Production servers (uvicorn, gunicorn, hypercorn) have neither problem.
