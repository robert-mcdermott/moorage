# `moorage.toml` reference

`moorage.toml`, at the top of an app's directory, says how Moorage builds
and runs the app. It is optional: without it, Moorage detects the kind of
app (see [Detection](#detection)), and `moor manifest explain` shows what it
detected. This page describes the manifest as Moorage 0.6.0 implements it;
the [design](../spec/03-manifest.md) describes what later releases add.

```bash
moor manifest validate      # checks the file, reporting every problem at once
```

```bash
moor manifest explain       # shows how the app will be built and run
```

```bash
moor manifest schema        # prints a JSON Schema for editors
```

## Rules

- **Every key is optional**, and an unknown key is an error, so that a typo
  fails at once. Tables whose names start with `x-` are left for other
  tools.
- **Durations** are strings such as `"500ms"`, `"90s"`, `"10m"` or `"2h"`.
- **Commands** run in Moorage's embedded POSIX shell, the same on every
  operating system: `&&`, `$VAR`, quotes and redirections work everywhere.
  The programs a command calls must exist on the server.
- **Secrets never go in the manifest.** `moor manifest validate` warns about
  values that look like credentials; set them on the server with
  `moor env set --secret` (see [Variables and secrets](variables.md)).

## An example

```toml
[app]
name = "tasks"

[python]
version = "3.13"

[processes.web]
command = "uvicorn tasks.main:app --host 127.0.0.1 --port $PORT --proxy-headers"
health = { path = "/healthz" }

[processes.worker]
command = "python -m tasks.worker"

[release]
command = "python -m tasks.migrate"

[deploy]
keep_releases = 10

[env]
LOG_LEVEL = "info"
```

## `[app]`

| Key | Default | Description |
|---|---|---|
| `name` | — | The app's name, used when `moor deploy` is not given `--app`: 3 to 40 lowercase letters, digits and hyphens, starting with a letter. It is also the first part of the app's hostname. |
| `runtime` | `"auto"` | `auto` (detect), `go`, `python`, `node`, `static`, `command` (your own build), `binary` (run the files as they are), or [`dockerfile` or `image`](containers.md) (run in a container). |
| `toolchains` | `[]` | More toolchains on the `PATH` while building and running, as `name@version`: `go`, `node`, `python` and `uv` are installed by Moorage, and `name@system` uses the server's own (`"java@system"`). Versions may be partial (`"24"`, `"3.13"`); the exact version is recorded in the build. |

`watch` lists the paths that matter, as globs, whose `*` crosses slashes: a [push](git.md) that touches none of them, nor this
file, is kept and not deployed.

`strip_prefix`, with [path routing](domains.md#with-no-wildcard-dns-path-routing), takes the path the app is served at away
from what it hears.

## `[go]`

| Key | Default | Description |
|---|---|---|
| `version` | the `toolchain` line of `go.mod`, or else the newest patch release of its `go` line | The Go that builds the app. Builds run with `GOTOOLCHAIN=local`. |
| `main` | `.` when it is `package main`, or else the single `./cmd/*` | The main package. |
| `binary` | the last element of the module path (or of `main`) | The binary's name, in `$MOORAGE_BIN`. |
| `ldflags` | `"-s -w"` | Linker flags. |
| `tags` | `[]` | Build tags. |
| `cgo` | `false` | Sets `CGO_ENABLED`; the server then needs a C compiler. |

The build is `go build -trimpath -ldflags="…" -o "$MOORAGE_BIN/<binary>" <main>`,
and the `web` process runs the binary. See [Go](go.md).

## `[python]`

| Key | Default | Description |
|---|---|---|
| `version` | `.python-version`, or else `requires-python`, or else the newest release | Installed with uv; a partial version means its newest patch release. |
| `manager` | `uv` with a `pyproject.toml`, `uv-pip` with only a `requirements.txt` | `uv` runs `uv sync --locked --no-dev` (`uv sync --no-dev`, with a warning, without a `uv.lock`); `uv-pip` makes a virtual environment and runs `uv pip install -r requirements.txt`. |
| `groups` | `[]` | More dependency groups to install. |
| `extras` | `[]` | Optional extras to install. |

The virtual environment's `bin` comes first on the `PATH`, and
`VIRTUAL_ENV` and `PYTHONUNBUFFERED=1` are set. See [Python](python.md).

## `[node]`

| Key | Default | Description |
|---|---|---|
| `version` | `.nvmrc`, or else `.node-version`, or else `engines.node`, or else the current LTS release | Partial versions and ranges (`"24"`, `">=22"`, `"lts/*"`) mean the newest matching release. |
| `package_manager` | `npm` | Only npm in this release: `npm ci` with a `package-lock.json`, `npm install` (with a warning) without one. |
| `build_script` | `"build"`, when `package.json` has one | The script run after the install. |
| `prune` | `true` | Removes development dependencies after the build. |

The `web` process runs the `start` script directly, not through npm, so that
the stop signal reaches the app; `NODE_ENV=production` is set. Without a
`start` script, the build's `dist/`, `build/` or `out/` is served as a static
site. See [Node.js](node.md).

## `[build]`

| Key | Default | Description |
|---|---|---|
| `command` | the runtime's build | Replaces the runtime's build. `moor manifest explain` shows the build it replaces. |
| `pre` | — | Runs before the build (code generation, for instance). |
| `post` | — | Runs after the build (a front end, for instance). |
| `env` | `{}` | Variables for the build only. |
| `timeout` | `"20m"` | The build fails after this long. |
| `exclude` | `[]` | Patterns, in `.gitignore` syntax, removed from the build's output. |

Builds run in the directory the app will run from. The upload leaves out what
`.gitignore` and `.moorignore` exclude, and `node_modules`, `.venv` and
`.git`.

## `[processes]`

Each entry is a long-running process. `web` receives the app's HTTP traffic.
An entry is a command (`worker = "python -m tasks.worker"`) or a table:

| Key | Default | Description |
|---|---|---|
| `command` | the runtime's, for `web` | What to run. |
| `instances` | `1` | How many instances run (0 to 20; `0` turns the process off). Requests are spread over the healthy ones. [`moor scale`](console.md#scaling) overrides it, on the server. |
| `expose` | `true` for `web` | The process listens on `$PORT` (and `$HOST`). |
| `health` | a TCP check of `$PORT` | See below. |
| `stop_signal` | `"TERM"` | The signal that asks the process to stop: `TERM`, `INT`, `QUIT`, `HUP`, `USR1` or `USR2`. |
| `stop_timeout` | `"30s"` | How long the process may take to stop before it is killed. |
| `sleep` | `true` | Whether it stops while its environment [sleeps](previews.md#sleeping). |
| `restart` | `"always"` | When an exited process is restarted: `always`, `on-failure` or `never`. Restarts slow down when a process keeps failing. |

`health`:

| Key | Default | Description |
|---|---|---|
| `path` | — | An HTTP `GET` path; a 2xx or 3xx answer is healthy. Without it, a TCP connection to `$PORT` is the check. |
| `expect` | — | Text the answer must contain. |
| `startup` | `"60s"` | How long a new instance may take to become healthy during a deploy. |
| `interval` | `"10s"` | Time between checks while the instance runs. |
| `timeout` | `"3s"` | Each check's time limit. |
| `threshold` | `3` | Failed checks in a row before an instance is unhealthy: it gets no traffic and is restarted. |
| `min_uptime` | `"5s"` | For a process that does not listen: how long a new instance must keep running to count as healthy. |

Without `[processes]`, a `Procfile` (`name: command` lines) is used, and
then the runtime's own `web` process.

## `[container]`

For `runtime = "dockerfile"` and `"image"`. See [Running apps in
containers](containers.md).

| Key | Default | Description |
|---|---|---|
| `file` | `Containerfile`, else `Dockerfile` | What to build the image from (`dockerfile`). |
| `context` | `"."` | The build's context, in the app's directory (`dockerfile`). |
| `target` | — | The stage of a multi-stage build to stop at (`dockerfile`). |
| `args` | `{}` | Build arguments (`dockerfile`); `[build].env` adds to them. |
| `image` | — | The image to run (`image`): `ghcr.io/acme/api:1.4`. `moor deploy --image` overrides it. |
| `port` | the lowest the image exposes, else `8080` | Where the app listens inside the container. `$PORT` is set to it. |
| `user` | the image's | Who to run as: `1000`, `1000:1000` or a name the image knows. |

Of `[build]`, a container build uses `env` and `timeout`. Without `[processes]`,
`web` runs what the image says to, and a `command` replaces the image's
entrypoint and command.

## `[resources]`

Limits for each instance of an app, and `[environments.NAME.resources]` for one environment's. A
container's engine enforces them. For an app outside a container, it [depends on the
server](isolation.md#limits).

| Key | Description |
|---|---|
| `memory` | Memory: `"512MiB"`. An instance that goes over it is killed and restarted, and it says that it ran out of memory. |
| `cpu` | CPU, in cores: `0.5`, `2`. |
| `pids` | The most processes and threads. |

## `[storage]`

| Key | Default | Description |
|---|---|---|
| `mount` | `"/data"` | Where the environment's data directory appears inside a container. |

## `[release]`

| Key | Default | Description |
|---|---|---|
| `command` | — | Runs once per deployment, after the build and before the new release takes traffic, with the release's variables: database migrations go here. If it fails, the deployment fails and the previous release keeps serving. It does not run on a rollback or a change of variables. |
| `on_create` | — | Runs once, after an environment's first release (to seed data). |
| `timeout` | `"10m"` | Each command's time limit. |

## `[deploy]`

| Key | Default | Description |
|---|---|---|
| `strategy` | `"rolling"` | `rolling` starts the new instances, waits until they are healthy, moves the traffic, then stops the old ones. `recreate` stops the old instances first, for apps that cannot run twice at once. |
| `drain` | `"30s"` | How long old instances may finish their requests after the traffic moves. |
| `keep_releases` | `5` | Builds kept for rollbacks. |

## `[static]`

How a static site is served (`runtime = "static"`, or a Node.js build
without a `start` script).

| Key | Default | Description |
|---|---|---|
| `dir` | the first of `dist`, `build`, `out`, `public`, `_site`, `site` and `.` with an `index.html` | The directory served. |
| `spa` | `false` | Serves `index.html` for paths that are not files (client-side routing). |
| `clean_urls` | `true` | `/about` serves `about.html`, and `/about.html` redirects to `/about`. |
| `not_found` | `"404.html"`, when there is one | The page served, with status 404, for paths that are not files. |
| `trailing_slash` | `"auto"` | `auto` (directories end in a slash, pages do not), `always` or `never`. |
| `listing` | `false` | Lists the files of directories without an `index.html`. |

`[[static.headers]]` adds headers to the paths that match `for`:

```toml
[[static.headers]]
for = "/assets/*"
values = { "Cache-Control" = "public, max-age=31536000, immutable" }
```

`[[static.redirects]]` redirects (`status` 301, 302, 307 or 308) or rewrites
(`status = 200`) paths, the first rule that matches winning. `*` matches
the rest of a path (`:splat` in `to`) and `:name` matches one segment. A
file that exists is served rather than redirected unless `force = true`.

```toml
[[static.redirects]]
from = "/blog/:year/*"
to = "/posts/:year/:splat"
status = 301
```

Pages get strong ETags and ranges; files whose names carry a content hash
(`app.3f2a9c1b.js`) are cached as immutable, and HTML is revalidated;
`.br` and `.gz` files next to a file are served to browsers that accept
them. See [Static sites](static.md).

## `[previews]`

[Previews](previews.md) are on unless this says otherwise.

| Key | Default | |
|---|---|---|
| `enabled` | `true` | A preview for each branch pushed. |
| `branches` | every branch | Globs of the branches that get one; `!` leaves some out: `["*", "!dependabot/*"]`. |
| `ttl` | `"7d"` | Destroyed after this long without a deployment or a request. |
| `sleep_after` | `"30m"` | [Sleeps](previews.md#sleeping) after this long without a request; `"off"` never. |
| `access` | the server's | [Who can reach them](access.md): `password`, `login` or `share`; `public` only if the server's default is. |

## `[environments.NAME]`

What is different about [an environment](previews.md#named-environments), which deploying the
manifest makes if it is not there. `NAME` is `production`, a name such as `staging`, or `preview`, which
applies to every preview.

| Key | |
|---|---|
| `branch` | The branch whose pushes deploy to it. |
| `env` | Variables, over those of `[env]`. |
| `processes.web.instances` | How many instances of that process run. |
| `sleep_after` | [Sleeps](previews.md#sleeping) after this long without a request. By default a preview does after 30 minutes, and others never do; `"off"` says never. |
| `access` | [Who can reach it](access.md): `password`, `login` or `share`; `public` only if the server's default is. |

## `[[cron]]`

[Cron jobs](cron.md): `name`, `schedule`, `command`, `timezone`, `timeout`, `overlap` and `environments`.

## `[vars.NAME]`

[What the app expects](variables.md#what-an-app-expects), with no values in the repository.

| Key | Default | |
|---|---|---|
| `description` | | Shown in the console and by `moor env`. |
| `required` | `false` | A deployment fails, early, if nothing sets it. |
| `secret` | `false` | A deployment fails if it is set, and not as a secret. |
| `generate` | | `secret` or `uuid`: a value is made once for each environment, and kept as a secret. |

## `[env]`

Variables for the app, at build and run time, such as `LOG_LEVEL = "info"`.
Variables set on the server with `moor env set` override them.

## What Moorage sets

Every process gets these variables; they cannot be overridden.

| Variable | Example | Description |
|---|---|---|
| `PORT` | `24817` | Where the process listens (exposed processes). In a container: the container's port. |
| `HOST` | `127.0.0.1` | The address to listen on. In a container: `0.0.0.0`. |
| `MOORAGE_APP`, `MOORAGE_ENV` | `tasks`, `production` | The app and its environment. |
| `MOORAGE_RELEASE` | `v8` | The release. |
| `MOORAGE_BUILD_ID` | `bld_…` | The build. |
| `MOORAGE_URL` | `https://tasks.apps.example.com` | The app's public address. |
| `MOORAGE_PROCESS`, `MOORAGE_INSTANCE`, `MOORAGE_INSTANCE_ID` | `web`, `1`, `ins_…` | Which instance this is. |
| `MOORAGE_DATA_DIR` | `…/data/tasks/production` | A directory of the environment's that survives deploys, for the app's own files. In a container: where it is mounted. |
| `MOORAGE_BIN` | `…/.moorage/bin` | Where builds put binaries. Not in a container. |
| `TMPDIR` | | A temporary directory of the instance's own, removed when it stops. Not in a container. |

The app's directory is read-only once built: write to `$MOORAGE_DATA_DIR`
or `$TMPDIR`.

## Detection

With `runtime = "auto"`, or without a manifest, the first match wins:

| Order | Signal | Runtime |
|---|---|---|
| 1 | `go.mod` | `go` |
| 2 | `pyproject.toml`, `uv.lock` or `requirements.txt` | `python` |
| 3 | `package.json` | `node` |
| 4 | `Containerfile` or `Dockerfile` | `dockerfile` |
| 5 | `index.html` at the top or in a common output directory | `static` |

The `web` process of a Python app is the project's single
`[project.scripts]` entry, or else a FastAPI or Starlette app run with
uvicorn, or a Django or Flask app run with gunicorn, found in `main.py`,
`app.py`, `server.py`, `asgi.py`, `wsgi.py` or `api.py` (at the top or in a
package at the top). When nothing matches, the deploy says what it looked
at and which lines to add.

## Accepted, not acted on yet

These parts of the [design](../spec/03-manifest.md) are read and checked,
and `moor manifest validate` warns that they have no effect yet:

| Setting | Arrives |
|---|---|
| `app.root` | a later release |
| `[storage]` outside a container | a later release; `$MOORAGE_DATA_DIR` is always there |
| `[[routes]]`, `[services]` | later releases |
| `previews.data` | a later release |
| `runtime = "bun"` or `"deno"`, `[bun]`, `[deno]` | a later release |
| `[static]` beside `[processes]` | with `[[routes]]`; until then the processes serve the app |
