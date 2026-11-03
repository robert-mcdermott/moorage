# Examples

Complete apps, small but realistic, each ready to deploy with `moor deploy`
from its directory. Each has a README that says what it shows and how to
run it locally.

| Example | What it is | What it shows |
|---|---|---|
| [`python-fastapi`](python-fastapi) | A task list: FastAPI, uvicorn and a web page | uv and `uv.lock`, SQLite in the data directory, migrations as the release command, a health check that reads the database |
| [`go-stdlib`](go-stdlib) | A link shortener, with only the standard library | A build from `go.mod` alone, `HOST` and `PORT`, a graceful stop, data that two instances can share during a deploy |
| [`node-express`](node-express) | A live poll: an Express API and a Vite front end | `npm ci`, a build script, pruning, the start script, assets cached for good, variables from `[env]` |
| [`static-site`](static-site) | A team handbook | Clean URLs, a 404 page, headers, redirects that keep old links working |

To try them, install Moorage on your own computer
([installing Moorage](../docs/install.md)):

```bash
moor server init --preset local
```

```bash
moor server run
```

Then, in another terminal, from an example's directory:

```bash
moor deploy
```

The app is at `http://<name>.localhost:8080`: `tasks`, `links`, `poll` or
`handbook`. Chromium- and Firefox-based browsers open `*.localhost`
addresses on their own; for `curl`, send the name as the `Host` header
(`curl -H 'Host: tasks.localhost' http://127.0.0.1:8080/`).

[`testdata/apps`](../testdata/apps) has smaller apps, one per runtime, that
Moorage's own tests deploy.
