# Installing Moorage

Moorage is one program, `moor`: the server and the command line that talks
to it. Version 0.4.0 runs on Linux and macOS (amd64 and arm64); on Windows,
`moor` runs as a command line and `moor server run` works, but installing the
server as a service arrives in a later release.

## 1. Get `moor`

On macOS or Linux:

```bash
curl --proto '=https' --tlsv1.2 -fsSL https://raw.githubusercontent.com/robert-mcdermott/moorage/main/install.sh | sh
```

In PowerShell on Windows 11:

```powershell
irm https://raw.githubusercontent.com/robert-mcdermott/moorage/main/install.ps1 | iex
```

Each picks the file for your computer from the latest release and downloads it with
the release's `SHA256SUMS`. It requires exactly one matching checksum, runs the
downloaded program's version check, and only then puts it in place, so a failed
download never replaces a working copy. It needs no administrator rights, and starts
nothing.

| | Installs to | `PATH` |
|---|---|---|
| macOS, Linux | `~/.local/bin/moor` | Left alone: it says what to add, if it is not there |
| macOS, Linux, as root | `/usr/local/bin/moor` | Left alone |
| Windows | `%LocalAppData%\Programs\Moorage\moor.exe` | Added to your user `PATH`: open a new terminal to use it |

**For a server that will run as a system service,** install as root, so that the
service does not run from someone's home directory. Download and read the script
first; never pipe a download straight into `sudo`:

```bash
curl --proto '=https' --tlsv1.2 -fsSLo install-moor.sh https://raw.githubusercontent.com/robert-mcdermott/moorage/main/install.sh
```

```bash
less install-moor.sh
```

```bash
sudo sh install-moor.sh
```

**To choose a release or a directory,** set these (`MOOR_NO_PATH_UPDATE` and
`MOOR_ARCH` are for Windows only):

| Variable | `install.sh` | `install.ps1` | |
|---|---|---|---|
| `MOOR_VERSION` | `--version` | `-Version` | A release, such as `v0.4.0`. By default, the latest |
| `MOOR_INSTALL_DIR` | `--install-dir` | `-InstallDir` | An absolute directory |
| `MOOR_REPOSITORY` | | `-Repository` | `owner/repository`, for a fork |
| `MOOR_ARCH` | | `-Architecture` | `amd64` or `arm64`, if detecting it fails |
| `MOOR_NO_PATH_UPDATE` | | `-NoPathUpdate` | Leave the user `PATH` alone |

```bash
curl --proto '=https' --tlsv1.2 -fsSL https://raw.githubusercontent.com/robert-mcdermott/moorage/main/install.sh | MOOR_VERSION=v0.4.0 sh
```

```powershell
$env:MOOR_VERSION = 'v0.4.0'; irm https://raw.githubusercontent.com/robert-mcdermott/moorage/main/install.ps1 | iex
```

**On Windows,** `irm … | iex` reads the script into memory and runs it, so the
PowerShell execution policy, which governs script files, does not apply. To read it
first, or for a saved copy:

```powershell
Invoke-WebRequest -UseBasicParsing https://raw.githubusercontent.com/robert-mcdermott/moorage/main/install.ps1 -OutFile install-moor.ps1
```

```powershell
Get-Content install-moor.ps1
```

```powershell
powershell -ExecutionPolicy Bypass -File .\install-moor.ps1
```

### By hand

Download the file for your system from the
[releases page](https://github.com/robert-mcdermott/moorage/releases), check it against
`SHA256SUMS`, and put it on your `PATH`:

```bash
curl -fLO https://github.com/robert-mcdermott/moorage/releases/latest/download/moor-linux-amd64
```

```bash
curl -fLO https://github.com/robert-mcdermott/moorage/releases/latest/download/SHA256SUMS
```

```bash
sha256sum --check --ignore-missing SHA256SUMS
```

```bash
sudo install -m 0755 moor-linux-amd64 /usr/local/bin/moor
```

(On macOS, use `moor-darwin-arm64` or `moor-darwin-amd64`, and
`shasum -a 256 --check --ignore-missing SHA256SUMS`.)

The checksum shows a damaged download, and a file that differs from the release's
list. Both come from the same release, so it cannot protect against someone able to
replace both.

Moorage's programs are not signed with an Apple or Microsoft certificate. The commands
above download without a browser, so the system does not flag the file. If you save it
with a browser, macOS or Windows SmartScreen asks for confirmation the first time it
runs: on macOS, remove the flag with `xattr -d com.apple.quarantine ./moor-darwin-arm64`,
and on Windows, with `Unblock-File .\moor-windows-amd64.exe`.

### From source

Build it with Go 1.26 or newer:

```bash
CGO_ENABLED=0 go build -trimpath -o moor ./cmd/moor
```

`moor version` prints the version you have.

## 2. Configure the server

`moor server init` writes `server.toml`, the server's configuration, from a
preset. Run it as root for a server that starts with the machine (the file
goes to `/etc/moorage/server.toml` on Linux, `/Library/Application
Support/Moorage/server.toml` on macOS), or as yourself to try Moorage on
your own computer: everything then goes in one folder, `~/.moorage`, on macOS and Linux,
and in `%LocalAppData%\Moorage` on Windows. `XDG_CONFIG_HOME` and `XDG_DATA_HOME` win on
Linux, if they are set.

A server set up as yourself by an earlier version stays where it is, and `moor` finds it.
To move one, stop it, move the folder, change `data_dir` in `server.toml`, and start it again.

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
at `/.moorage/ca.crt`, and `moor trust` installs ([TLS and trusting the
server](tls.md)). To use your own certificates — a corporate wildcard, for instance — set
`tls.mode = "files"` and list them under `[[tls.certificates]]`; an
intermediate CA from your corporate CA can issue certificates instead
(`tls.ca_cert`, `tls.ca_key`).

**Behind a TLS proxy such as nginx** — Moorage serves plain HTTP on
127.0.0.1:8080 and your proxy terminates TLS:

```bash
sudo moor server init --domain apps.example.com --behind-proxy
```

The proxy must keep the `Host` header, pass WebSocket upgrades, not buffer
responses, and allow long requests and large uploads. `moor server proxy-config
nginx` writes a configuration for nginx: see [Behind a TLS proxy](proxy.md).

Every setting is explained in the file itself; `moor server run` checks it
strictly and names the line of every problem. `moor server doctor` checks the
rest, at any time, and says how to fix what it finds ([When something is
wrong](troubleshooting.md)).

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
`moor server stop --all` stops the apps too, and also stops a server you started
with `moor server run`; `moor server uninstall` removes
the service and stops the apps, and leaves the configuration and data in
place.

The server's log is in the journal on Linux
(`journalctl -u moorage.service -f`) and in `logs/server/moorage.log` in the
data directory on every system.

To run the server in the foreground instead, for a try:

```bash
moor server run
```

## 4. Claim it

`moor server init` printed a one-time setup link (`moor server status` shows it
again). Open it and create the first account, the server's administrator. On
your own computer you can skip this: the command line needs no account there.

## 5. Deploy

On the server's machine, the command line finds the server and its admin
token by itself, when run as the user that runs the server (with `sudo` for
a system service). From another machine, sign in:

```bash
moor login https://moorage.apps.example.com
```

That opens your browser to approve the sign-in; with Moorage's own CA, also pass
its certificate (`--ca-cert ca.crt`). `moor server users create` adds other
people ([Accounts and access](accounts.md)).

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

## Upgrading

Run the installer again, as the same user: it replaces `moor` where it is, and says
what it upgraded from. To go back, give it an earlier release with `MOOR_VERSION`.

Then restart the server, to use the new version:

```bash
sudo moor server restart
```

Running apps keep running, and the new server takes them over. Until you restart it, a
server goes on with the version it started with. It migrates the database as it
starts, after keeping a snapshot in `backups` in the data directory. Upgrade `moor` on
the other machines that use the server too: a newer server may change what the API
answers. `moor server doctor` then checks the result.

On Windows, the server and the supervisors of its apps run from `moor.exe`, and a
running program cannot be replaced there. The installer moves it aside as
`moor.previous.*.exe`, which goes on running, and clears that away on a later upgrade
once nothing uses it. Press Ctrl+C where the server runs, and start it again.

From 0.1.0, the server has no accounts yet: `moor server status` shows the link to
create the first, and until then the admin token works from anywhere, as it did.

## Uninstalling

First, on a server, stop it and its apps and remove the service:

```bash
sudo moor server uninstall
```

Then delete the program: `~/.local/bin/moor` or `/usr/local/bin/moor`, or on Windows,
the `%LocalAppData%\Programs\Moorage` folder, whose entry you can also remove from your
user `PATH` (**Settings → System → About → Advanced system settings → Environment
Variables**).

The configuration and the data stay. Delete `server.toml`, which `moor server status`
names, and the data directory that it names under `data_dir`, only if you no longer
need the apps' data and the keys in it.
