# Deploying any other app

Moorage runs anything that serves HTTP on a port: say how to build it, if
it needs building, and what to run.

## Build on the server

The `command` runtime runs your build command, with extra toolchains on the
`PATH` — installed by Moorage (`go`, `node`, `python`, `uv`) or, with
`@system`, the server's own:

```toml
[app]
name = "reports"
runtime = "command"
toolchains = ["java@system"]

[build]
command = "./gradlew --no-daemon installDist"

[processes.web]
command = "build/install/reports/bin/reports --port $PORT"
health = { path = "/health" }
```

```bash
moor deploy
```

A `Procfile` (`web: …` lines) can stand in for `[processes]`.

## Run it as it is

The `binary` runtime runs the uploaded files without building them — a
script for an interpreter on the server, for instance:

```toml
[app]
name = "tool"
runtime = "binary"

[processes.web]
command = "./server.py"
```

To upload something you built elsewhere, such as in CI, deploy the build's
output with `--artifact` and the `[processes]` of the `moorage.toml` in the
current directory:

```bash
moor deploy --artifact ./dist --app tool
```

## What every app gets

- `PORT` to listen on, and `HOST=127.0.0.1`: apps are reached only through
  Moorage.
- `MOORAGE_DATA_DIR`, a directory that survives deploys, and `MOORAGE_APP`,
  `MOORAGE_ENV`, `MOORAGE_RELEASE`, `MOORAGE_URL` and others describing where
  it runs.
- A health check: a TCP connection to `PORT`, or with `health.path`, an HTTP
  request that must answer 2xx or 3xx. A new release takes traffic only once
  every instance is healthy; until then the previous one keeps serving.
- Commands are run by a POSIX shell built into Moorage, the same on every
  system; a simple command (a program and its arguments) is run directly.
  The programs they call must exist on the server.
- The stop signal (TERM, or `stop_signal`), then a kill after `stop_timeout`
  (30 seconds). Processes that exit are restarted, more and more slowly if
  they keep crashing.
