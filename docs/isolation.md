# How apps are kept apart

On a server that several people deploy to, one app should not be able to read another's data or
secrets, or take all of its memory. How far Moorage can see to that depends on the system, and it
tells you what is in effect: on an app's page in the console, in **Settings → System**, and from
`moor server doctor`.

## The levels

| Level | What it does | Where |
|---|---|---|
| `none` | Every app runs as the same user as the server, and can read what the others can. | Everywhere |
| `user` | Each app runs as an operating system user of its own, and its builds as another. | Linux, with the server running as root |
| `sandbox` | As `user`, and each app can only read its own build, toolchains and system files, can only write to its own data and temporary directories, and can only listen on its own port. | Linux 5.13 or newer, with the server running as root |

`server.toml` says which:

```toml
[isolation]
level = "sandbox"
```

Without that, it is `auto`: `user` where the server runs as root on Linux, as the [system
service](install.md) does, and `none` elsewhere. Asking for a level that this system cannot have
stops the server from starting, with the reason. `auto` settles for what it can get, and says why.
This is a server-wide setting: a change takes effect for each instance as it next starts, so
restart your apps after changing it.

Moorage assumes that those who deploy are teammates and not adversaries. Isolation prevents
accidents and limits the damage if one app is broken into. It does not make a server safe for
strangers' code, which needs virtual machines.

## With a user of its own

- An app's users come from a range that starts at 300000, or at `uid_base` under `[isolation]`.
  They need no entries in `/etc/passwd`, and an app keeps its user for good, so that `uid_base` is to be
  set before there are apps: a server refuses to run one as a user below it.
- Its data directory belongs to it, and no other app can enter it. Its build is read-only.
- Other apps cannot read its environment, including its secrets, from `/proc`.
- `moor run`, cron jobs and release commands run as the app's user.
- Builds run as a user of their own in the app's group, with a cache of their own, so that what
  a package's install script does cannot reach the app's data or another app's builds.

**An app can only write to `$MOORAGE_DATA_DIR` and `$TMPDIR`.** One that writes beside its own code
has to change, since its build is read-only.

Upgrading a server that already runs as root changes who its apps run as when they next start, and
Moorage gives them their data directories then. **Instances that were running through the upgrade go on
as root until they restart**, which the app's page and **Settings → System** say: `moor restart APP`
does it without downtime, and the other apps cannot read their files in the meantime. An app that reads
something only root can read will stop being able to, and `level = "none"` keeps it as it was while you
look into that. Coming back from `none`, an app gets back what it wrote as root the next time it starts.

An app's environments share its user, so that a preview can read production's data and variables. Keep
what should not meet in apps of their own. `/dev/shm` is left writable in a sandbox too, as programs
expect it to be, and is shared between apps, under the usual file permissions.

An environment's data can be a link to a bigger disk, in `data/<app>`: the app's user gets what it leads to, and a
backup holds that, so that a restore brings it back as a plain directory.

The data directory and `moor` itself have to be where other users can reach them. `/var/lib/moorage`
and `/usr/local/bin`, where the system service puts them, are. If they are not, `auto` stays at
`none` and says so.

## In a sandbox

Besides what `user` does, it sets `no_new_privs`, so that nothing it runs can gain privileges, and
uses Landlock to confine it. A kernel with an earlier version of Landlock restricts what it can: the
limit on ports takes Linux 6.7. A kernel with none runs apps at the `user` level, and says so.

Some programs expect more. One that writes to `/tmp` by its full name, rather than to `$TMPDIR`, fails
in a sandbox, as does one that listens on a second port.

## On Windows

There is one level there, `none`. The server runs as the account of its own that the service has, `NT SERVICE\Moorage`,
and all apps run as it too, so that they are kept from the rest of the computer, and not from one another. Each
instance runs in a Job Object, which holds it and all it starts to its limits, and ends them all together.

## Limits

`[resources]` in the [manifest](manifest.md#resources) says how much each instance may use:

```toml
[resources]
memory = "512MiB"
cpu = 0.5
pids = 200
```

| Where | Memory | CPU and processes |
|---|---|---|
| Linux, with the server running as root | The kernel kills an instance over it, and the whole instance goes together. It cannot swap instead | Enforced by the kernel |
| Windows | A Job Object holds each instance: what it asks for beyond its memory is refused, and it is not killed for that | Enforced, by the same Job Object |
| Anywhere else | Moorage restarts an instance that stays over it for three samples, 30 seconds or so | Not enforced, which the app's page says |
| In a container | The engine enforces it | The engine enforces it |

An instance the kernel kills for its memory is restarted, and says so where it says how it last exited:
**out of memory**. A command or cron job killed in that way fails, and says so too. One that Moorage restarts
instead says that it was stopped, with `SIGTERM`, and the activity says why.

Builds count for half as much for CPU as running apps do where the kernel can enforce that, so
that a heavy build slows itself down rather than the apps people are using.

## See also

- [Metrics](metrics.md), for how much each instance uses
- [Cron jobs and one-off commands](cron.md)
