# Installing Moorage

Moorage is one program, `moor`: the server and the command line that talks
to it. Version 0.1.0 runs on Linux and macOS (amd64 and arm64); on Windows,
`moor` runs as a command line and `moor server run` works, but installing the
server as a service arrives in a later release.

## 1. Get `moor`

Download the binary for your system from the
[releases page](https://github.com/robert-mcdermott/moorage/releases), check
it against `SHA256SUMS`, and put it on your `PATH`:

```bash
curl -fLO https://github.com/robert-mcdermott/moorage/releases/download/v0.1.0/moor-linux-amd64
```

```bash
curl -fLO https://github.com/robert-mcdermott/moorage/releases/download/v0.1.0/SHA256SUMS
```

```bash
sha256sum --check --ignore-missing SHA256SUMS
```

```bash
sudo install -m 0755 moor-linux-amd64 /usr/local/bin/moor
```

(On macOS, use `moor-darwin-arm64` or `moor-darwin-amd64`, and
`shasum -a 256 --check --ignore-missing SHA256SUMS`.)

Or build it from source with Go 1.26 or newer:

```bash
CGO_ENABLED=0 go build -trimpath -o moor ./cmd/moor
```

`moor version` prints the version you have.

## 2. Configure the server

`moor server init` writes `server.toml`, the server's configuration, from a
preset. Run it as root for a server that starts with the machine (the file
goes to `/etc/moorage/server.toml` on Linux, `/Library/Application
Support/Moorage/server.toml` on macOS), or as yourself to try Moorage on
your own computer (the file goes to your user's configuration directory).

**On your own computer** — apps at `http://<app>.localhost:8080`:

```bash
moor server init --preset local
```

Chromium- and Firefox-based browsers resolve `*.localhost` names on their
own; for `curl` and other tools, send the app's name as the `Host` header
(`curl -H 'Host: hello.localhost' http://127.0.0.1:8080/`).

**A server on your network** — apps at `https://<app>.<domain>`, with
certificates from Moorage's own certificate authority:

```bash
sudo moor server init --domain apps.example.com
```

Point a wildcard DNS record, `*.apps.example.com`, at the server. Browsers
trust the certificates once they trust Moorage's CA, which every app serves
at `/.moorage/ca.crt` (installing it with `moor trust` arrives in 0.2.0). To
use your own certificates — a corporate wildcard, for instance — set
`tls.mode = "files"` and list them under `[[tls.certificates]]`; an
intermediate CA from your corporate CA can issue certificates instead
(`tls.ca_cert`, `tls.ca_key`).

**Behind a TLS proxy such as nginx** — Moorage serves plain HTTP on
127.0.0.1:8080 and your proxy terminates TLS:

```bash
sudo moor server init --domain apps.example.com --behind-proxy
```

The proxy must keep the `Host` header, pass WebSocket upgrades, not buffer
responses, and allow long requests and large uploads.

Every setting is explained in the file itself; `moor server run` checks it
strictly and names the line of every problem.

## 3. Run it as a service

```bash
sudo moor server install
```

This installs and starts a systemd unit (`/etc/systemd/system/moorage.service`)
on Linux or a launchd daemon (`/Library/LaunchDaemons/io.moorage.server.plist`)
on macOS, which starts with the machine. Run it without `sudo` to install a
service of your own user instead (on Linux, `loginctl enable-linger` keeps it
running after you log out).

The service runs `moor` from where it was when you installed it, with the
`PATH` you had then — apps see that `PATH` too. Reinstall after moving
`moor` or to change the `PATH`.

```bash
sudo moor server status
```

```bash
sudo moor server restart
```

Apps keep running while the server stops or restarts: each runs under a small
supervisor of its own, and the server takes them over when it starts again.
Requests to apps are refused for the moment the server restarts.
`moor server stop --all` stops the apps too; `moor server uninstall` removes
the service and stops the apps, and leaves the configuration and data in
place.

The server's log is in the journal on Linux
(`journalctl -u moorage.service -f`) and in `logs/server/moorage.log` in the
data directory on every system.

To run the server in the foreground instead, for a try:

```bash
moor server run
```

## 4. Deploy

On the server's machine, the command line finds the server and its admin
token by itself, when run as the user that runs the server (with `sudo` for
a system service). From another
machine, pass the server's address and the token from `keys/admin-token` in
the data directory, or set them in the environment:

```bash
export MOOR_SERVER=https://moorage.apps.example.com MOOR_TOKEN=moor_…
```

With Moorage's own CA, also pass its certificate (`--ca-cert ca.crt`, or
`MOOR_CA_CERT`).

Then deploy an app from its source directory:

```bash
moor deploy --app hello
```

Or push it with git, over SSH (port 2222, `listen.ssh` in `server.toml`)
or HTTPS: register your SSH key once with `moor keys add`, add the app's
remote with `moor git remote`, and `git push moorage main`
([Deploying with git push](git.md)). Open port 2222 in the firewall, beside
80 and 443, for pushes from other machines.

The quick starts show each kind of app: [Go](go.md), [Python](python.md),
[Node.js](node.md), [static sites](static.md) and
[anything else](other.md). [Variables and secrets](variables.md)
configure apps.

## Back up

The data directory holds everything: the database, the builds, the apps'
data directories and the keys. **Back up `keys/master.key` on its own**: it
encrypts the apps' secrets, and a copy of the database is of no use for
them without it. Take a copy of the database while the server runs with
`sqlite3 moorage.db ".backup backup.db"`, or stop the server first (the
apps keep running).
