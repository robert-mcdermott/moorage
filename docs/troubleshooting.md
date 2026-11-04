# When something is wrong

## Ask the doctor

```bash
moor server doctor
```

It checks a server on this machine, running or not, and says how to fix what it
finds. Run it with `sudo` if the server runs as root, so that it can read the
server's files.

```text
✔ config         /etc/moorage/server.toml
✔ server         running 0.2.0 on :80
✔ listen.http    answering on :80
✔ listen.https   answering on :443
✔ listen.ssh     answering on :2222
✘ dns (apps)     doctor-7f3a91c2.apps.example.com does not resolve
                 → add a wildcard DNS record, *.apps.example.com, for this server or its proxy
✔ dns (console)  moorage.apps.example.com → 192.0.2.10
✔ tls (ca)       the internal CA expires on 3 Nov 2036, SHA-256 4F:9A:…:C1
· tls (trust)    this machine does not trust the internal CA, and browsers will warn about its certificates
                 → moor trust, on each machine that uses the server
✔ tls (serving)  serves moorage.apps.example.com with a certificate from it, until 3 Dec 2026
✔ containers     podman version 5.8.5, answering
✔ data           /var/lib/moorage, with a database of 2.4 MiB
✔ disk           63.6 GiB free of 926.4 GiB, for /var/lib/moorage
✔ clock          synchronised
✔ kernel         cgroup v2 (cpu, memory, pids), Landlock: for isolating native apps, in a later release
✔ apps           4 instances, reachable only through the router

1 problem.
```

| Check | What it looks at |
|---|---|
| `server` | That it answers, is healthy, and is the version of this `moor` |
| `listen.*` | That it answers on each address; while it is stopped, that it could listen there |
| `dns` | The console's hostname and a made-up app's, through the system's resolver |
| `tls` | The CA or certificate files: that they exist, match, cover the hostnames, and how long they have left. Plain HTTP where it should not be |
| `containers` | That the engine is there and answers: a problem if `server.toml` names it, a warning if it was only found on this machine |
| `data`, `keys` | The data directory, and that nobody else can read its keys |
| `disk` | Free space where the data is |
| `clock` | That the clock is synchronised (Linux) |
| `kernel` | cgroup v2 and Landlock (Linux), which isolating native apps will use |
| `apps` | That no instance listens beyond loopback, where it could be reached without the router |

`--public-url` adds [the path through a proxy](proxy.md#check-the-whole-path). The
exit status is 1 if anything failed, and `--json` prints the checks for a script.

## Stopping everything

Apps keep running when the server stops, on purpose. `moor server stop` stops the
server, as a service or as started with `moor server run`; with `--all` it stops
the apps too, in containers or not, whether or not a server is running:

```bash
moor server stop --all
```

The server starts them again when it next starts.

## `*.localhost` does not resolve

On your own computer, apps are at `http://<app>.localhost:8080`. Chromium- and
Firefox-based browsers and systemd-resolved on Linux resolve those names by
themselves. Safari and command-line tools on macOS do not, and this fixes that:

```bash
moor server resolver install
```

It adds `/etc/resolver/localhost`, for which it uses `sudo`, and turns on a small
DNS responder in the server, which answers on loopback only, for that domain only.
Restart the server to take it up. `moor server resolver` shows where things stand,
and `moor server resolver remove` undoes it.

Windows has no equivalent yet. There, send the name as the `Host` header:

```bash
curl -H "Host: tasks.localhost" http://127.0.0.1:8080/
```

## Where the logs are

| Log | |
|---|---|
| The server's | `logs/server/moorage.log` in the data directory, and the journal on Linux (`journalctl -u moorage.service -f`) |
| An app's | `moor logs -f` |
| A deployment's | `moor deployments show ID` |
| Requests to an app | `logs/router/<app>/<env>.log` in the data directory |
