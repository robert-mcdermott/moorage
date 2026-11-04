# Guestbook: a Go app in a container

A guestbook, with an image that holds one static binary and nothing else.

It shows:

- **`runtime = "dockerfile"`**, which asks for the image to be built, even though
  a `go.mod` here would otherwise mean a native build.
- **A multi-stage Containerfile**, ending in an image `FROM scratch`, running as a
  user that is not root.
- **`[resources]`**, the most of the server the app may have.
- **`[storage]`**, where the data directory appears in the container. The entries
  are there, and outlive every release.
- **A release command with no shell**: `/guestbook migrate` runs directly, in a
  container of the new image, before that image takes traffic.
- **A port from the image**: `EXPOSE 8000` says where the app listens, and Moorage
  publishes it on the server's loopback interface. The app reads `PORT` and
  `HOST` all the same, and would run outside a container too.
- **A command from the image**: `[processes.web]` has none, so the image's
  `ENTRYPOINT` runs.

## Deploy it

The server needs Podman or Docker. Then, from this directory:

```bash
moor deploy
```

The first deploy downloads the Go image and builds; the next, after a change to
`main.go`, takes seconds. See [Running apps in containers](../../docs/containers.md).

## Run it locally

```bash
go run .
```

Then open <http://127.0.0.1:8000>.
