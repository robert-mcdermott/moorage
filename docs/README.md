# Moorage documentation

How to install Moorage, run it, and deploy apps to it. This documentation
describes Moorage 0.1.0; the [design documents](../spec/README.md) describe
how it works inside and where it is going.

## Start here

- [Installing Moorage](install.md): get `moor`, configure the server, run it
  as a service, back it up.
- The [examples](../examples/README.md): complete apps, ready to deploy.

## Deploying apps

- [With git push](git.md): the production branch deploys, and the push
  shows the build
- [Go](go.md)
- [Python](python.md), with uv
- [Node.js](node.md), with npm
- [Static sites](static.md)
- [Anything else](other.md): your own build, a script, an app that is
  already built
- [Variables and secrets](variables.md)

## Reference

- [`moorage.toml`](manifest.md): every setting of the app manifest.
- The command line: every command has `--help`; the common ones are below.

| Command | What it does |
|---|---|
| `moor deploy [DIR]` | Builds and deploys the source in DIR; prints the app's URL |
| `moor git remote` | Adds the app's git remote: `git push moorage main` deploys |
| `moor keys add` | Registers your SSH key, so that you can push over SSH |
| `moor apps` | Lists the apps |
| `moor deployments` | Lists an app's deployments; `show` and `follow` one |
| `moor releases` | Lists an app's releases |
| `moor rollback [vN]` | Serves an earlier release again, in a moment |
| `moor ps` | Lists the app's instances: status, health, port, PID |
| `moor logs [-f]` | Shows the app's output |
| `moor restart` | Replaces the instances without downtime |
| `moor env` | Lists and changes variables and secrets |
| `moor manifest explain` | Shows how Moorage will build and run the app |
| `moor server status` | Shows the server and its service |

An app is chosen with `--app`, `$MOOR_APP`, `[app].name` in the
`moorage.toml` of the current directory, or the repository's `moorage` git
remote.

## How Moorage works, in brief

- **The server** is the `moor` binary, run as a service. It keeps its state
  in one SQLite database and one data directory, and answers every app's
  hostname (`<app>.<domain>`) itself.
- **A deploy** uploads the app's source, or pushes it to the app's git
  repository on the server. The server works out how to build it, installs
  the toolchains it needs (checked against their published checksums) and
  builds it in the directory it will run from.
- **A release** is a build with its variables. A new release takes over only
  once its instances answer their health checks; the old ones finish their
  requests first. `moor rollback` brings an earlier release back.
- **Each instance** runs under a small supervisor of its own, which restarts
  it when it fails and keeps it running while the server restarts.
