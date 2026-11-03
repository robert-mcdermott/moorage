# 3. The App Manifest — `moorage.toml`

The manifest tells Moorage how to **build** and **run** an app. It lives in
the repository root (or in the app's sub-directory for monorepos) and is
versioned with the code, so every release records exactly how it was built.

The manifest is **optional**. For common layouts Moorage detects the runtime
and fills in sensible defaults (see [Detection](#runtime-detection)). Running
`moor init` in a project writes a manifest containing what was detected, so
nothing stays implicit for long.

## Why TOML

- Unambiguous types (a bare `no` or `3.10` is never silently reinterpreted)
  and no significant indentation.
- Comfortable for Go, Python (`pyproject.toml`) and Rust developers alike.
- Comments are supported, and there is good editor support with schema
  validation. Moorage publishes a JSON Schema (`moor manifest schema`) so
  editors can autocomplete and validate the file.

## Design rules

1. **Code-owned vs operator-owned settings.** *How* to build and run the app
   (build commands, processes, health checks, release command, cron, routes,
   static rules) comes **only** from the manifest. Operational settings
   (environment variables, instance counts, resource limits, sleep, access
   gates) can be **defaulted** in the manifest but are **overridable** on the
   server through the console or CLI; server-side values win and the console
   marks them as overridden. Domains and secrets are server-side only.
2. **Secrets never go in the manifest.** `moor manifest validate` warns when a
   literal value looks like a credential (`*_SECRET`, `*_TOKEN`, `PASSWORD`,
   high-entropy strings).
3. **Every key is optional** except where noted. Unknown keys are an error
   (typos should fail fast), except inside `[x-*]` tables reserved for tools.
4. **Durations** accept `ms`, `s`, `m`, `h`, `d` (`"90s"`, `"7d"`).
   **Sizes** accept `KiB/MiB/GiB` and `K/M/G` (`"512MiB"`).
5. **Commands** are strings interpreted by Moorage's embedded POSIX-compatible
   shell, so `&&`, `$VAR`, quoting and redirection behave identically on
   Linux, macOS and Windows (see [Runtime](05-runtime.md#command-execution)).
   The *programs* a command calls must still exist on the host — stock
   Windows has no `cp`, `grep` or `sed` — so portable manifests stick to the
   toolchains' own commands.

## Quick examples

### Static site

```toml
[app]
runtime = "static"

[static]
dir = "public"
```

### Go service

```toml
[app]
runtime = "go"            # version comes from go.mod

[go]
main = "./cmd/server"

[processes.web]
command = "$MOORAGE_BIN/server --addr 127.0.0.1:$PORT"
health = { path = "/healthz" }
```

### FastAPI (uv) with a Vite frontend, a worker and migrations

```toml
[app]
runtime = "python"        # uv-managed; Python version from .python-version
toolchains = ["node@24"]  # frontend build needs Node too

[build]
post = "npm --prefix frontend ci && npm --prefix frontend run build"

[processes.web]
command = "uvicorn app.main:app --host 127.0.0.1 --port $PORT --proxy-headers"
instances = 2
health = { path = "/api/health" }

[processes.worker]
command = "python -m app.worker"

[release]
command = "alembic upgrade head"

[static]
dir = "frontend/dist"
spa = true

[[routes]]
path = "/api/"
process = "web"

[[routes]]
path = "/"
static = true

[[cron]]
name = "nightly-cleanup"
schedule = "0 3 * * *"
command = "python -m app.tasks.cleanup"

[env]
LOG_LEVEL = "info"

[previews]
ttl = "5d"
sleep_after = "20m"
access = "login"
```

### Node app

```toml
[app]
runtime = "node"          # version from .nvmrc / engines; pnpm from lockfile

[processes.web]
command = "node dist/server.js"
```

### Containerfile / Dockerfile

```toml
[app]
runtime = "dockerfile"

[container]
file = "Containerfile"
port = 8080               # port the app listens on inside the container

[processes.web]
health = { path = "/ready" }
```

### Prebuilt image

```toml
[app]
runtime = "image"

[container]
image = "ghcr.io/acme/reporting:1.8.2"
port = 3000
```

### Anything else (`command` runtime)

```toml
[app]
runtime = "command"
toolchains = ["java@21"]

[build]
command = "./gradlew --no-daemon installDist"

[processes.web]
command = "build/install/app/bin/app --port $PORT"
```

## Full reference

### Top level

| Key | Type | Default | Description |
|---|---|---|---|
| `version` | int | `1` | Manifest schema version. |

### `[app]`

| Key | Type | Default | Description |
|---|---|---|---|
| `name` | string | — | Suggested name used by `moor apps create`. The server-side name always wins. |
| `runtime` | string | `"auto"` | `auto`, `static`, `go`, `python`, `node`, `bun`, `deno`, `binary`, `command`, `dockerfile`, `image`. |
| `toolchains` | string[] | `[]` | Extra toolchains (`name@version`) on `PATH` during build and run, in addition to the runtime's own. Versions may be partial (`"22"`, `"3.13"`); the resolved exact version is recorded in the release. |
| `root` | string | `"."` | Sub-directory containing the app (monorepos). The manifest is read from `<root>/moorage.toml`. |
| `shell` | string | `"embedded"` | `embedded` (portable POSIX shell built into Moorage) or `system` (`/bin/sh -c`, or `cmd.exe /c` on Windows) for build and run commands. |
| `watch` | string[] | `["**"]` | Path globs (relative to the repository root). A push whose changes match none of them is stored but not deployed — useful when several apps share one repository. |

### Runtime-specific tables

Each runtime has a table of options; all keys are optional.

**`[go]`**

| Key | Default | Description |
|---|---|---|
| `version` | from `go.mod`: the `toolchain` line, else the newest patch release of the `go` line's minor version | Go toolchain version. Builds run with `GOTOOLCHAIN=local`, so this is the Go that builds. |
| `main` | `.` if it is `package main`, else the single `./cmd/*` | Main package to build. |
| `binary` | last element of `main` (of the module path when `main` is `.`) | Output name, placed in `$MOORAGE_BIN`. |
| `ldflags` | `"-s -w"` | Linker flags. |
| `tags` | `[]` | Build tags. |
| `cgo` | `false` | Sets `CGO_ENABLED`. Requires a host C toolchain when `true`. |

Preset build: `go build -trimpath -ldflags="$ldflags" -o "$MOORAGE_BIN/$binary" $main`.
Preset `web`: `$MOORAGE_BIN/$binary`.

**`[python]`**

| Key | Default | Description |
|---|---|---|
| `version` | `.python-version`, else `requires-python`, else latest stable | Python version (installed via uv). |
| `manager` | `uv` if `pyproject.toml`; `uv-pip` if only `requirements.txt` | `uv` runs `uv sync --locked --no-dev` (without `--locked`, plus a reproducibility warning, when the repository has no `uv.lock`; a stale lock fails the build); `uv-pip` creates a venv and runs `uv pip install -r requirements.txt`. |
| `groups` | `[]` | Extra dependency groups to install. |
| `extras` | `[]` | Optional extras to install. |

The project virtualenv's `bin` directory is placed first on `PATH`, so
commands like `uvicorn …` or `python -m …` resolve without `uv run`.

**`[node]` / `[bun]` / `[deno]`**

| Key | Default | Description |
|---|---|---|
| `version` | `.nvmrc` / `.node-version` / `engines.node`, else current LTS | Runtime version. |
| `package_manager` | from lockfile (`package-lock.json`→npm, `pnpm-lock.yaml`→pnpm, `yarn.lock`→yarn, `bun.lock`→bun) | Installer. npm runs `npm ci` (`npm install`, with a reproducibility warning, without a lockfile). *pnpm, yarn and bun are not available yet.* |
| `build_script` | `"build"` if present in `package.json` | Script run after install. |
| `prune` | `true` | Remove dev dependencies after build. |

Preset `web`: the `start` script, executed directly with `node_modules/.bin` on
`PATH` (not through the package manager, so signals reach the app). If there
is no `start` script but the build produced `dist/`, `build/` or `out/` with
an `index.html`, the app is treated as a static site.

**`[static]`** — valid with any runtime (see [Routes](#routes)).

| Key | Default | Description |
|---|---|---|
| `dir` | first of `dist`, `build`, `out`, `public`, `_site`, `site`, `.` containing `index.html` | Directory to serve. |
| `spa` | `false` | Serve `index.html` for unknown paths (client-side routing). |
| `clean_urls` | `true` | `/about` serves `about.html`; `/about.html` redirects to `/about`. |
| `not_found` | `"404.html"` if present | Page served with status 404. |
| `trailing_slash` | `"auto"` | `auto`, `always`, `never`. |
| `listing` | `false` | Generate directory listings (internal file shares, artifact sites). |
| `[[static.headers]]` | — | `for = "/assets/*"`, `values = { "Cache-Control" = "…" }`. |
| `[[static.redirects]]` | — | `from`, `to`, `status` (301/302/307/308, or 200 for a rewrite), `force`. Rules are evaluated in order and the first match wins; an existing file shadows a rule unless `force = true`. `*` splats and `:name` placeholders are supported: `from = "/blog/:year/*"`, `to = "/posts/:year/:splat"`. |

Hashed asset files (`*.[hash].js` etc.) automatically get long-lived
immutable cache headers; HTML gets `no-cache`. Pre-compressed `.br`/`.gz`
siblings are served when the client accepts them.

**`[container]`** — for `dockerfile` and `image` runtimes.

| Key | Default | Description |
|---|---|---|
| `file` | `Containerfile`, else `Dockerfile` | Build file. |
| `context` | `"."` | Build context. |
| `target` | — | Multi-stage target. |
| `args` | `{}` | Build arguments. |
| `image` | — | Image reference (`runtime = "image"` only). |
| `port` | first `EXPOSE`, else `8080` | Port the app listens on **inside** the container. `PORT` is set to this value in the container. |
| `user` | image default | Run as this user. |

### `[build]`

| Key | Type | Default | Description |
|---|---|---|---|
| `command` | string | runtime preset | **Replaces** the runtime's preset build. `moor manifest explain` prints the preset so it can be copied and adapted. |
| `pre` | string | — | Runs before the preset/`command` (e.g. code generation). |
| `post` | string | — | Runs after the preset/`command` (e.g. building a frontend). |
| `env` | table | `{}` | Build-time-only environment variables. |
| `timeout` | duration | `"20m"` | Build is cancelled after this long. |
| `exclude` | string[] | `[]` | Patterns, in `.gitignore` syntax, pruned from the release after the build (tests, caches, sources not needed at runtime). `.moorignore` is honoured too. |

### `[processes.<name>]`

A process type is a long-running command. `web` is special: it receives HTTP
traffic by default. A process can be given as a plain string
(`worker = "python -m app.worker"`) or as a table:

| Key | Type | Default | Description |
|---|---|---|---|
| `command` | string | preset for `web` | Command to run. For container runtimes, overrides the image `CMD`. |
| `instances` | int | `1` | Number of instances (operator-overridable). `0` disables the process. |
| `expose` | bool | `true` for `web` | Process listens on `$PORT` and can be routed to. |
| `health` | table | TCP check for exposed processes | See below. |
| `stop_signal` | string | `"TERM"` | Signal sent on stop (`TERM`, `INT`, `QUIT`; Windows uses console break). |
| `stop_timeout` | duration | `"30s"` | Time between stop signal and forced kill. |
| `restart` | string | `"always"` | `always`, `on-failure`, `never`. |
| `sleep` | bool | `true` | Whether this process is stopped when the environment sleeps. |
| `protocol` | string | `"http"` | `http`, or `h2c` for cleartext HTTP/2 backends such as gRPC *(Phase 3)*. |

**`health`**

| Key | Default | Description |
|---|---|---|
| `path` | — | HTTP `GET` path; 2xx/3xx is healthy. Without it, a TCP connect to `$PORT` is used. |
| `expect` | — | Optional text the HTTP response body must contain. |
| `min_uptime` | `"5s"` | For non-exposed processes (workers): during a rollout, the new instance must stay running this long to count as healthy. |
| `command` | — | Alternative: command run in the instance environment; exit 0 is healthy *(not supported yet)*. |
| `interval` | `"10s"` | Time between liveness checks. |
| `timeout` | `"3s"` | Per-check timeout. |
| `startup` | `"60s"` | How long a new instance may take to become healthy during a deploy. |
| `threshold` | `3` | Consecutive failures before an instance is marked unhealthy. |

### `[release]`

Runs **once per deployment**, after the build and before any new instance
receives traffic, using the new release and the environment's runtime
configuration. Typical use: database migrations. A failing release command
aborts the deployment and leaves the current release serving. A rollback
or a change of variables publishes a build that was already released, so it
does not run the release command again.

| Key | Default | Description |
|---|---|---|
| `command` | — | Command to run on every deployment. |
| `on_create` | — | Command run **once**, after the first successful deployment of each new environment. Ideal for seeding demo data into previews and customer sandboxes. |
| `timeout` | `"10m"` | Time limit (each command). |

### `[deploy]`

| Key | Default | Description |
|---|---|---|
| `strategy` | `"rolling"` | `rolling` starts new instances, waits for health, switches traffic, then drains old ones. `recreate` stops old instances first (for apps that cannot run two copies at once). |
| `drain` | `"30s"` | How long old instances keep finishing in-flight requests after traffic is switched. |
| `keep_releases` | `5` | Releases kept for instant rollback. |

### `[[cron]]`

| Key | Default | Description |
|---|---|---|
| `name` | required | Unique per app. |
| `schedule` | required | 5-field cron expression or `@hourly`, `@daily`, `@every 15m`. |
| `command` | required | Runs as a one-off instance of the current release. |
| `timezone` | server timezone | IANA timezone name. |
| `timeout` | `"1h"` | Run is killed after this long. |
| `overlap` | `"skip"` | `skip`, `allow`, `replace` when the previous run is still going. |
| `environments` | `["production"]` | Environments the job runs in (`"*"` for all, previews excluded unless listed). |

### Routes

By default:

- an app with only a `web` process routes `/` to `web`;
- a `static` app serves its static directory at `/`;
- an app with **both** a `web` process and `[static]` serves a static file
  when one exists at the requested path and otherwise proxies to `web`.

`[[routes]]` makes the mapping explicit. Routes are matched by longest path
prefix.

| Key | Description |
|---|---|
| `path` | Path prefix (`"/api/"`). |
| `process` | Name of an exposed process to proxy to. |
| `static` | `true` to serve the `[static]` directory. |
| `strip_prefix` | Remove `path` before proxying (default `false`). |

### `[env]`

Non-secret default environment variables, available at build and run time.
Server-side variables with the same name win. Moorage also injects its own
variables (see [Runtime → Environment](05-runtime.md#environment-variables)).

### `[vars.<NAME>]`

Declares variables the app **expects** without putting values in the
repository. The console shows declared variables as a checklist per
environment, and a deployment fails early, with a clear message, if a
required variable is missing.

```toml
[vars.SECRET_KEY]
generate = "secret"          # random value created once per environment, stored encrypted

[vars.PAYMENTS_API_KEY]
required = true
secret = true
description = "Sandbox key for the payments provider"
```

| Key | Default | Description |
|---|---|---|
| `description` | — | Shown in the console and CLI. |
| `required` | `false` | Deployment fails if unset. |
| `secret` | `false` | Value must be stored as a secret. |
| `generate` | — | `secret` (32 random bytes, URL-safe) or `uuid`: generated once per environment, so every preview gets its own value. |

### `[resources]`

Operator-overridable defaults. Enforcement depends on OS and driver (see
[Runtime → Resource limits](05-runtime.md#resource-limits)).

| Key | Description |
|---|---|
| `memory` | Memory limit per instance (`"512MiB"`). |
| `cpu` | CPU limit per instance in cores (`0.5`, `2`). |
| `pids` | Max processes/threads per instance. |

### `[storage]`

Every environment has a persistent data directory that survives deploys,
exposed as `$MOORAGE_DATA_DIR` (native) or mounted into containers.

| Key | Default | Description |
|---|---|---|
| `mount` | `"/data"` | Mount point inside containers. |

### `[previews]`

| Key | Default | Description |
|---|---|---|
| `enabled` | `true` (if previews are enabled server-side) | Create an environment per pushed branch. |
| `branches` | `["*"]` | Glob filters; prefix `!` to exclude (`"!dependabot/*"`). |
| `ttl` | `"7d"` | Delete after this long without a deploy or request. |
| `sleep_after` | `"30m"` | Stop instances after this long without requests; wake on next request. |
| `access` | server default | `public`, `password`, `login`, `share`. |
| `data` | `"empty"` | `empty` or `copy` (copy production's data directory when created). |

### `[environments.<name>]`

Overrides for a specific environment. `<name>` is `production`, a named
environment such as `staging`, or the special name `preview` (applies to all
previews). Named environments other than `production` and `preview` are
created automatically when declared.

| Key | Description |
|---|---|
| `branch` | Branch that deploys to this environment (`production` defaults to the repository's default branch). |
| `env` | Additional/overriding env vars. |
| `resources` | Overriding resource limits. |
| `processes.<name>.instances` | Overriding instance count. |
| `sleep_after` | Idle sleep for this environment (`"off"` to disable). |
| `access` | Default access gate. |

### `[services.<name>]` *(planned — Phase 4)*

Declares a backing service provisioned by Moorage (via the container engine)
and linked to the app.

```toml
[services.db]
type = "postgres"     # postgres | mysql | redis | valkey | s3
version = "17"
# injects DATABASE_URL (or REDIS_URL, S3_* …); previews get their own instance
```

## Runtime detection

When `runtime = "auto"` (or there is no manifest), Moorage inspects the
source tree in this order and picks the first match:

| Order | Signal | Runtime |
|---|---|---|
| 1 | `go.mod` | `go` |
| 2 | `pyproject.toml`, `uv.lock` or `requirements.txt` | `python` |
| 3 | `deno.json` / `deno.jsonc` | `deno` |
| 4 | `bun.lock` / `bun.lockb` | `bun` |
| 5 | `package.json` | `node` |
| 6 | `Containerfile` / `Dockerfile` | `dockerfile` |
| 7 | `index.html` in `.` or a common output directory | `static` |

Native runtimes are preferred over a Containerfile/Dockerfile that sits in
the same repository; the build log says so explicitly and shows the line to
add (`runtime = "dockerfile"`) to change that.

**Process inference.** If `[processes]` is absent, a `Procfile` (one
`name: command` per line) is honoured. Otherwise the runtime preset supplies
`web`:

- **go** — the built binary.
- **node/bun/deno** — the `start` script / `deno task start`.
- **python** — a single `[project.scripts]` entry; otherwise framework
  heuristics: FastAPI/Starlette with `uvicorn` in dependencies →
  `uvicorn <module>:<app> --host 127.0.0.1 --port $PORT`; Django with
  `gunicorn` → `gunicorn <project>.wsgi`; Flask with `gunicorn` →
  `gunicorn <module>:<app>`.

If nothing matches, the build fails with a message that lists what was
inspected and the exact manifest lines to add. `moor init` runs the same
detection locally and writes the result to `moorage.toml`.

## Validation and tooling

- `moor manifest validate` — parse, type-check, lint (secrets, unknown keys,
  unreachable routes, processes without health checks).
- `moor manifest explain` — print the fully resolved build and run plan after
  detection and presets, including injected environment variables.
- `moor manifest schema` — print the JSON Schema for editor integration.
- Every release stores the **resolved** manifest; the console shows a diff
  between any two releases.
