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

> **Status:** design phase. See the [design documentation](docs/README.md).

## What it will feel like

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

## Documentation

- [Vision and goals](docs/01-vision.md)
- [Architecture](docs/02-architecture.md)
- [The `moorage.toml` manifest](docs/03-manifest.md)
- [Roadmap](docs/11-roadmap.md)
- [All design documents](docs/README.md)
