# Moorage

**Push code. Get a URL. Watch it run.**

Moorage is a self-hosted application deployment platform that ships as a
single, dependency-free binary — `moor`. Install it on a server or your
laptop, point a wildcard DNS record at it, and every developer on the team can
deploy apps with `git push` and share a live URL in seconds.

- **Any app, not just containers.** Go binaries, uv-managed Python, Node,
  Bun, Deno, static sites and arbitrary commands run natively; Containerfiles
  and images run on Podman or Docker.
- **A small manifest, or none at all.** `moorage.toml` describes how to build
  and run the app; common stacks are detected automatically.
- **Built-in everything.** Git server, reverse proxy with built-in TLS (or
  plain HTTP behind your own proxy, such as nginx), toolchain manager, process
  supervisor, log and metrics collection, and an embedded web console — all in
  one binary, backed by SQLite.
- **Previews for every branch.** Each pushed branch gets its own environment
  and URL, sleeps when idle, and can be shared behind a password, a sign-in or
  an expiring link.
- **Zero-downtime deploys and instant rollback**, with health checks and
  release history.
- **Cross-platform.** Linux first; macOS and Windows supported.

> **Status:** 0.1.0, the first release: a server on Linux or macOS that
> builds and deploys apps from their source, with zero-downtime releases,
> instant rollbacks, and variables and encrypted secrets. Deploys by
> `git push`, accounts and containers come in 0.2.0, the web console in
> 0.3.0 — see the [roadmap](docs/11-roadmap.md), the
> [changelog](CHANGELOG.md) and the [design documentation](docs/README.md).

## Using it today

```bash
# On the server (see the guide for a server on your network, TLS and proxies)
sudo moor server init --domain apps.example.com
sudo moor server install

# On the server, in an app's source directory
sudo moor deploy --app my-app             # builds it, shows the build, prints the URL
sudo moor env set --secret DATABASE_URL   # asks for the value; rolls out a new release
sudo moor logs -f --app my-app
sudo moor rollback --app my-app
```

The [guide](docs/guide/README.md) covers installation and each kind of app.

## Where it is going

```bash
# On the server
sudo moor server init --preset lan --domain apps.example.com
sudo moor server install

# On a developer machine
moor login https://moorage.apps.example.com
cd my-fastapi-app
moor init                 # detects uv + FastAPI, writes moorage.toml
moor apps create my-app --git-remote
git push moorage main     # build output streams back; prints the live URL

git switch -c feature/new-pricing
git push moorage feature/new-pricing   # → https://my-app--feature-new-pricing.apps.example.com
moor share --env . --expires 7d        # expiring link to this branch's preview
```

## Development

Moorage needs Go 1.26 or newer and nothing else: every build is pure Go
(`CGO_ENABLED=0`), for Linux, macOS and Windows on amd64 and arm64.

```bash
go build ./cmd/moor
```

```bash
go test ./...
```

The sample apps are built from source with real toolchains (downloaded on
the first run) by:

```bash
MOORAGE_TEST_BUILDS=1 go test ./internal/server -run TestSampleApps -v
```

To try the server on this computer:

```bash
./moor server init --preset local --config ~/moorage-dev/server.toml --data-dir ~/moorage-dev/data
```

```bash
./moor server run --config ~/moorage-dev/server.toml
```

```bash
curl http://127.0.0.1:8080/.moorage/health
```

Deploy a directory of files as a static site (on the server's machine the CLI
finds the server and its admin token by itself):

```bash
MOORAGE_CONFIG=~/moorage-dev/server.toml ./moor deploy --static ./some-site --app hello
```

Chromium- and Firefox-based browsers resolve `hello.localhost` on their own: open
`http://hello.localhost:8080`.

Deploy an app from its source: the server works out how to build it (Go,
Python with uv, Node.js with npm, a static site, or what `moorage.toml`
says), downloads and verifies the toolchains it needs, builds it and starts
it. The build is shown as it happens:

```bash
MOORAGE_CONFIG=~/moorage-dev/server.toml ./moor deploy testdata/apps/python
```

`moor manifest explain DIR` shows what the server will do with an app
before you deploy it. `moor env set NAME=value` (or `--secret NAME`, which
asks for the value and keeps it encrypted) configures an app; every change
is rolled out as a new release. [testdata/apps](testdata/apps) has a small app for
each runtime.

An app that is already built is deployed with `--artifact` and the
`[processes]` of its `moorage.toml`, for example:

```toml
[app]
name = "pyweb"

[processes.web]
command = "python3 -m http.server $PORT --bind 127.0.0.1 --directory public"
```

Work is planned in waves and slices: each wave has a `waveN` branch and ends
in a release (see the [roadmap](docs/11-roadmap.md)). Changes are recorded in
the [changelog](CHANGELOG.md).

## Documentation

- [Vision and goals](docs/01-vision.md)
- [Architecture](docs/02-architecture.md)
- [The `moorage.toml` manifest](docs/03-manifest.md)
- [Roadmap](docs/11-roadmap.md)
- [All design documents](docs/README.md)
