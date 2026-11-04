# Installing Moorage

Moorage is one program, `moor`: the server and the command line that talks
to it. It runs on Linux, macOS and Windows, on amd64 and arm64.

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
| `MOOR_VERSION` | `--version` | `-Version` | A release, such as `v0.6.0`. By default, the latest |
| `MOOR_INSTALL_DIR` | `--install-dir` | `-InstallDir` | An absolute directory |
| `MOOR_REPOSITORY` | | `-Repository` | `owner/repository`, for a fork |
| `MOOR_ARCH` | | `-Architecture` | `amd64` or `arm64`, if detecting it fails |
| `MOOR_NO_PATH_UPDATE` | | `-NoPathUpdate` | Leave the user `PATH` alone |

```bash
curl --proto '=https' --tlsv1.2 -fsSL https://raw.githubusercontent.com/robert-mcdermott/moorage/main/install.sh | MOOR_VERSION=v0.6.0 sh
```

```powershell
$env:MOOR_VERSION = 'v0.6.0'; irm https://raw.githubusercontent.com/robert-mcdermott/moorage/main/install.ps1 | iex
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
Support/Moorage/server.toml` on macOS), in an elevated terminal on Windows for the same
(`%ProgramData%\Moorage\server.toml`), or as yourself to try Moorage on
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

**On Windows,** run this in an elevated terminal, as an administrator, leaving out the `sudo` here and below:

```bash
moor server install
```

That makes a Windows service called Moorage, which starts with the computer. It runs as an account of its own,
`NT SERVICE\Moorage`, with few rights, and so do your apps, rather than as the system. That account, the system and
administrators are the only ones who can get at the data directory. A `moor` in your own profile, where the installer
puts it, is one the service could not read, so that a copy goes in `%ProgramFiles%\Moorage`: use that one for
`moor server upgrade`. A container engine is usually for one person, so that an app in a container needs an engine that
this account can reach.

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

On Windows, a program has to have the extension it runs by: a `go build -o bin/server.exe .` in your manifest, and
`command = "bin/server.exe"`.

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

## How big a machine

Moorage itself asks for little: about 100 MB of memory, and next to no CPU while nothing is
happening. What a machine has to hold is your apps, and only those awake. An environment
[asleep](previews.md#sleeping) uses no memory until a request wakes it.

We check this with [a script](../.github/scripts/capacity-check.sh) on a machine limited to 4 CPUs and
8 GB: 60 environments of 10 apps, each a small Python server holding 64 MB, with sleep on.

| With 60 environments | Measured |
|---|---|
| Memory, all awake | 4.9 GB |
| Memory, all asleep | 120 MB |
| The server's own memory, at its most | 107 MB |
| The server's CPU, all asleep | under 1% of a core |
| Waking one with a request | 0.13 s for this app: yours takes as long as it takes to start |
| Waking all 60 at once | 2.3 s for the slowest |
| Serving five while the others sleep | 4,100 requests a second, 95% within 12 ms |
| Requests that failed | none |

So count what your apps use while they are awake, and how many of them will be at once. Builds
need room as well, which depends on what you build, and logs and [metrics](metrics.md) take disk.

## Back up

```bash
moor server backup --to /mnt/backups
```

That holds the database, repositories, builds, apps' data and certificates, and the server goes on
running. **Keep `keys/master.key` safe, and apart from your backups**: it encrypts the apps' secrets,
and a backup leaves it out. See [backups](backups.md) for a schedule, buckets and restoring.

## Upgrading

From 0.6.0, a server upgrades itself. To get to 0.6.0 from an earlier version, [run the installer](#with-the-installer)
once, and restart the server.

```bash
moor server upgrade --check
```

```bash
sudo moor server upgrade
```

That downloads the newest release and checks it against the checksum published with it. It then keeps a copy of the database and of the binary, puts the new binary in place, and
restarts the server. Running apps keep running, and the new server takes them over. **If the server does not
come up on the new version, everything goes back by itself.** A database from a large server can take a
while to migrate: `--wait 10m` gives it longer than the 2 minutes it otherwise has.

| Option | What it does |
|---|---|
| `--check` | Only says whether there is a newer version |
| `--version 0.6.1` | Upgrades to that version, and not the newest |
| `--from DIR` | Takes the release from a directory or an address, for a mirror or a machine with no way out. It holds a release's files |
| `--wait 10m` | Gives the new version longer to come up |

To go back later:

```bash
sudo moor server upgrade --rollback
```

That puts back the binary, and leaves the database alone if the new version did not change it. If it did, the
previous version could not read it, so it goes back to its copy from before the upgrade. What has happened
since is then lost from it, so after 10 minutes this takes `--force`, and what it replaced is kept beside the
copy, in `upgrades` in the data directory. There is one way back: from the last upgrade.

A server that nothing manages, run with `moor server run`, gets its new binary, and goes on with the version
it started with until you restart it. To go back, stop it first.

Upgrade `moor` on the other machines that use the server too, by running the installer again: a newer server
may change what the API answers. `moor server doctor` then checks the result.

### With the installer

The installer can upgrade a server as well, without the copies or the way back. Run it again as the same
user, which replaces `moor` where it is and says what it upgraded from, and restart the server:

```bash
sudo moor server restart
```

Until you do, a server goes on with the version it started with. A newer server changes the database as it starts,
after keeping a copy in `backups` in the data directory, and an older one refuses a database that has moved on.
So to go back to an earlier version in this way: stop the server, put that copy in the place of `moorage.db`,
and give the installer the version with `MOOR_VERSION`. `moor server upgrade --rollback` only goes back from the
version it upgraded to, and says so if the installer has been at work since.

On Windows, the server and the supervisors of its apps run from `moor.exe`, and a running program cannot be
replaced there, so both ways move it aside, where it goes on running. A server run in a terminal needs Ctrl+C and
starting again.

### What a release can be checked against

Each release has `SHA256SUMS`, a list of what went into the binaries in `moorage-sbom.cdx.json`, and an attestation
of the workflow that built them. An upgrade and the installer both check a download against `SHA256SUMS`, which comes
from the same place over HTTPS. That catches a damaged download, and a change on the way, and would not catch someone
who could change the release itself. The attestation is GitHub's own record of what its build made, and that you can
look at yourself:

```bash
gh attestation verify moor-linux-amd64 --repo robert-mcdermott/moorage
```

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
