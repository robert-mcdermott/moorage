# Running apps in containers

Moorage runs most apps directly, with a toolchain it installs. An app runs in a
container when it brings its own image: a `Containerfile` or `Dockerfile` to
build, or an image that is built already. It is deployed, released, rolled back,
routed and logged like any other app.

The server needs [Podman](https://podman.io) or
[Docker](https://www.docker.com/). No other app needs either.

## From a Containerfile

With a `Containerfile` or `Dockerfile` and no `go.mod`, `pyproject.toml` or
`package.json`, nothing else is needed:

```bash
moor deploy
```

A language's own files come first in [detection](manifest.md#detection): to build
the image of an app that has them, say so.

```toml
[app]
name = "guestbook"
runtime = "dockerfile"
```

```text
▸ detected   dockerfile · Containerfile
▸ build      podman build -f Containerfile .
             …
▸ image      localhost/moorage/guestbook:bld_01m6hrr5e7pw8bxb1jntjhx30m, 7.6 MiB in 19.2s
▸ release    v1: /guestbook migrate
             set up the guestbook in /data
▸ rollout    web ×1
✔ live       v1  https://guestbook.apps.example.com  (22.1s)
```

The engine keeps its layers, so the next build is quicker. `git push` works the
same way. A complete app: the [Guestbook example](../examples/container-go), with
a multi-stage build and an image holding one binary.

Variables flagged for builds (`moor env set --build`) reach the build as build
arguments, for the `ARG` lines that name them. Remember that an image's history
keeps its build arguments: don't pass a secret this way to an image anyone else
will see.

## From an image

```bash
moor deploy --image ghcr.io/acme/api:1.4 --app api
```

The server pulls the image and records its digest. Restarts and rollbacks run
exactly that image, whatever the tag has come to mean since; deploying the same
tag again picks up what it means now. An image on the server only, that no
registry has, is used as it is.

The `moorage.toml` in the current directory goes with it, if there is one, and no
other file. Or name the image there, and `moor deploy` needs no flag:

```toml
[app]
name = "api"
runtime = "image"

[container]
image = "ghcr.io/acme/api:1.4"
```

### Private registries

The server signs in with credentials an admin gives it, kept encrypted with its
[master key](variables.md):

```bash
moor registry login ghcr.io --username acme-deploy
```

The password is asked for, or read from standard input. Nothing checks it until
the next pull. It reaches the engine in a temporary file, readable only by the
server's user, for as long as the pull takes; it is used for private base images
too. `moor registry` lists the registries and `moor registry logout HOST` removes
one.

## What runs

| | |
|---|---|
| **Command** | What the image says to run. A `command` under [`[processes]`](manifest.md#processes) replaces it, entrypoint and all. |
| **Plain and shell commands** | A command of plain words runs directly, so an image needs no shell for it: `/app/server --port 8000`. Anything else (`$PORT`, `&&`, quotes) runs in the image's `/bin/sh`. |
| **Port** | `port` under [`[container]`](manifest.md#container), or else the lowest port the image `EXPOSE`s, or else 8080. `$PORT` is set to it, and the app must listen on all addresses: `$HOST` is `0.0.0.0`. |
| **Publishing** | On the server's loopback interface only, on a port Moorage chooses. Traffic arrives through the router. |
| **User** | `user` under `[container]`, or else whoever the image says. |
| **Data** | The environment's [data directory](manifest.md#what-moorage-sets) at `/data`, or `mount` under [`[storage]`](manifest.md#storage), as `$MOORAGE_DATA_DIR`. It survives deploys. |
| **Variables** | [`[env]`](manifest.md#env), the app's [variables and secrets](variables.md), and Moorage's. Nothing else comes from the server: `PATH`, `HOME` and the rest are the image's. |
| **Limits** | [`[resources]`](manifest.md#resources): memory, CPU and processes, for each instance. |
| **Privileges** | `no-new-privileges`, and no host network or privileged mode. An init process reaps zombies and passes on signals. |

The release command, and `on_create`, run in a container of the new image, with
the same variables and data directory, and no port. So do workers: every process
under `[processes]` is a container of the same image.

```toml
[app]
runtime = "dockerfile"

[container]
port = 8000
user = "1000:1000"

[resources]
memory = "512MiB"
cpu = 1.5

[release]
command = "/app/migrate"

[processes.web]
health = { path = "/healthz" }

[processes.worker]
command = "/app/worker"
```

## Running them

`moor ps`, `moor logs`, `moor restart` and `moor rollback` work as for any app.

- **Moorage owns restarts**, by `restart` under [`[processes]`](manifest.md#processes).
  The engine's own restart policy is off, and a container that keeps exiting is
  restarted more and more slowly. One the kernel killed for running out of memory
  says so in its log and in `moor ps --json`, where the engine reports it. Podman
  4.9 does not: the log says the container was killed, perhaps for going over its
  memory limit.
- **A health check needs an answer.** A published port accepts connections
  whatever the container is doing, so a TCP check also waits to see that the
  connection stays open: a `health.path` is still the better check.
- **Containers outlive the server.** A restarted server finds them by their labels
  and takes them over, and their output in between is in their log. After a reboot,
  they are started again.
- **Each is a command you could run yourself.** Moorage calls the engine's command
  line. Its containers are named `moorage-<app>-<env>-<process>-<n>-<id>` and
  labelled `io.moorage.*`, so `podman ps --filter label=io.moorage.app=guestbook`
  finds an app's. Removing one is noticed, and the instance is replaced.

## Choosing the engine

Under `[containers]` in `server.toml`:

```toml
[containers]
engine = "auto"
```

`auto` is Podman if it is installed, or else Docker. Otherwise, name a binary. The
engine runs as the server's user, with its usual settings: `CONTAINER_HOST` and
`DOCKER_HOST` in the server's environment are honoured. The server looks for it as
it starts, and says what it found in its log.

## Good to know

- **Secrets are visible to the engine's users.** A secret
  reaches the engine through its environment, on no command line and in no file.
  The container keeps it in its configuration, so anyone who can `inspect` the
  container can read it: on a server, that is whoever can already use the engine.
- **A secret cannot be called what the engine's command line uses for itself**:
  `PATH`, `HOME`, `TMPDIR`, or `DOCKER_*`, `CONTAINER_*` and `XDG_*` variables it
  reads. The deploy says so.
- **The data directory is writable by any user**, because the image's user is
  whoever the image says. The directories above it keep out the server's other
  users. With rootless Podman, files a container's non-root user writes belong to
  a user of the container's, and to read them on the server you need
  `podman unshare`.
- **Old images are removed** with their releases, beyond `keep_releases`. An image
  another app also uses is pulled again when it is next needed.
- **Metrics come later.** The engine's numbers for CPU and memory arrive with
  metrics, in 0.5.0.
