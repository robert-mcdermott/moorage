# Backups

A backup holds what a server cannot make again: its database, the apps' repositories and builds,
their data directories and the server's certificates. It is one file, and the server goes on
running while it is made. Logs, toolchains and caches are left out, as they come back by themselves.

```bash
moor server backup --to /mnt/backups
```

Backups run where the server is, as the user that runs it, so that on a system service that takes
`sudo`.

## The master key

A backup leaves out `keys/master.key`, which encrypts the apps' secrets, so that a backup that goes
astray gives them away to nobody. **Without that key a backup's secrets are lost for good, so that
keep it safe, and somewhere other than your backups.**

Restoring needs that key to be in place, and refuses before reading any further if it is not the one
that the backup was made with. `--include-keys` puts the key and the server's other keys in the
backup, for a directory that you keep as safe as they are. A bucket is refused: there, one wrong
setting gives away every secret you have.

## A schedule

**Settings → Backups** in the console says where they go, and how often:

- **Where:** a directory, or a bucket on S3 or a service like it. The secret key is sealed with the
  master key.
- **Schedule:** a cron expression, such as `@daily` or `0 3 * * *`, in the server's time zone. One that
  was due while the server was down is made when it comes back.
- **How many to keep:** when a scheduled one finishes, older ones go.

With nothing said, they go to `backups/archives` in the data directory. That is a start and not a plan,
since whatever takes that disk takes them too.

A backup that fails tells administrators in the console, and a channel with a
[rule](notifications.md#rules) for `backup.failed`. So does the next one that works.

## To a bucket

From the command line, the service's details come from the usual variables:

```bash
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... AWS_REGION=eu-west-1 moor server backup --to s3://acme-backups/moorage
```

For a service other than AWS, `AWS_ENDPOINT_URL` says where it is, and buckets are then addressed by
path. Its parts go up as they are made, so that a backup needs neither memory nor disk for the whole
of it. Without a `--to`, the command uses what **Settings → Backups** says.

## See what is there

```bash
moor server backup list
```

Read one through to see that it could be restored, without changing anything:

```bash
moor server backup verify /mnt/backups/moorage-20261104T120000Z.tar.gz
```

## Restoring

Install the same version of Moorage or a newer one, configure it as before, and put the master key in
place. Then, with the server stopped:

```bash
moor server restore /mnt/backups/moorage-20261104T120000Z.tar.gz
```

```bash
moor server start
```

It reads all of the backup to one side first, so that a damaged one changes nothing, and refuses one
that did not finish or that does not hash to what was noted when it was made. On a server that already
has a database, `--force` moves what is there aside rather than deleting it. Any apps still running are
stopped just before the swap. The server starts them again when it starts, and brings an older backup's
database up to date. Everyone can sign in as before, and browsers that trusted the server's own CA still
do.

## Apps with databases

The data directories are read while the apps go on running, so that a file being written at that moment
may be caught halfway. For most files that does not matter, but a database file copied while it is
in use may not open again, and the command says how many files changed as they were read.

If an app keeps a database in its data directory, have a [cron job](cron.md) dump it there
shortly before the backup, and restore from the dump. For SQLite:

```toml
[[cron]]
name = "dump"
schedule = "45 2 * * *"
command = 'sqlite3 "$MOORAGE_DATA_DIR/app.db" ".backup $MOORAGE_DATA_DIR/app.backup.db"'
```

## The server's configuration

A backup holds everything, to bring a server back. Its settings alone are a smaller thing, and a file you can
read, keep with the rest of what you look after, or bring to another server: people and their roles, projects
and who is in them, container registries, notification channels and rules, where backups go, and sign-in.

```bash
moor server config export > moorage-config.toml
```

**Settings → System** has the same file to download. It holds no secrets. Where one belongs, it names a variable
of the environment:

```toml
format = 1

[[user]]
email = "ada@example.com"
name = "Ada Lovelace"
role = "admin"

[[project]]
name = "web"

[project.members]
"sam@example.com" = "deployer"

[[registry]]
host = "ghcr.io"
username = "robot"
password = "${MOORAGE_REGISTRY_GHCR_IO_PASSWORD}"
```

To bring it in, look first at what that would do:

```bash
moor server config import moorage-config.toml --dry-run
```

```text
same    user            ada@example.com
create  user            sam@example.com     they need a link to choose a password, or single sign-on: moor server users invite sam@example.com
create  project         web
skip    registry        ghcr.io             set MOORAGE_REGISTRY_GHCR_IO_PASSWORD and bring this in again
```

Then do it, with those secrets in the command's environment:

```bash
MOORAGE_REGISTRY_GHCR_IO_PASSWORD=... moor server config import moorage-config.toml
```

**It adds and changes, and removes nothing.** What the file does not mention stays as it is, and a server keeps
the secret it has when the variable is not set, so that bringing a file back to the server it came from changes
nothing. Something that cannot be done is said, and does not stop the rest; the command then fails.

People's passwords and second factors do not go with them, so someone new needs `moor server users invite`, or
single sign-on. A disabled user stays disabled until you enable them. `server.toml` is apart from this: it is
a file already, and is for you to copy. So are your apps: those come from their manifests and repositories, and
a rule for an app that is not deployed yet is said to have failed, until it is.

## See also

- [Installing Moorage](install.md)
- [Notifications](notifications.md)
