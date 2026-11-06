# 13. Backing services *(Phase 4)*

Services are a database or object store that an app declares in its manifest, and Moorage runs and connects for it.
They arrive in 0.10.0. This document is the design that release builds. The [optional engine](05-runtime.md#the-engine)
they depend on comes first, in 0.9.0.

## The manifest

```toml
[services.db]
type = "postgres"             # postgres | s3
version = "18"                # optional: the default is recorded at creation, and does not move by itself
# image = "pgvector/pgvector:pg18"   # the way to get extensions
# env = "ANALYTICS_DATABASE_URL"     # another name, needed for a second of one type
# memory = "512MiB"
# previews = "own"            # or "none"
# seed = "production"         # opt-in: see Previews
# sanitize = "scripts/sanitize.sql"
```

## What runs

| Type | Versions | Default | Image | Gives the app |
|---|---|---|---|---|
| `postgres` | 15 to 18, and 19 after its release | 18 | `postgres:NN-trixie` | `DATABASE_URL` |
| `s3` | Versity Gateway on its POSIX backend | | `versity/versitygw` | `S3_*` and `AWS_*` |

- **Never `latest`.** An image's digest is recorded when a service is made, and an update is something you ask for.
- MySQL, MariaDB, Redis and Valkey are in the [parking lot](11-roadmap.md#parking-lot).
- **A presigned URL is signed for its host,** so a loopback address is no use to a browser. The first version gives only the
  internal endpoint. A public route through the ingress is a later opt-in.

### The S3 server

**Versity Gateway, run with `--sidecar`, with SeaweedFS as the fallback.** MinIO's repository is archived, with no new binaries. Garage has
no bucket policies. RustFS is young, with many recent advisories.

The two differ in kind. Versity Gateway translates: S3 requests become ordinary files and directories on a disk. SeaweedFS is a storage system: it keeps objects
in its own append-only volume files with a metadata store beside them, and is built to spread across many machines.

| | Versity Gateway | SeaweedFS (`weed mini`) |
|---|---|---|
| Data on disk | A bucket is a directory, and an object is a file | Volume files and a metadata store, readable only through SeaweedFS |
| What runs | One gateway | Master, volume, filer, S3, WebDAV, admin UI and a maintenance worker |
| Image, unpacked | 80 MB | 780 MB |
| Memory at rest, and after the test below | 9 MB, and 21 to 30 MB | 66 MB, and about 300 MB |
| Ready after starting | Under 0.1 second | About 4 seconds |
| Everyday S3: put, get, multipart, presigned URLs, CORS, bucket policies | Yes | Yes |
| Versioning | Refused unless given a directory for it, and marked experimental | Yes |
| Lifecycle rules | Refused: not implemented | Yes |
| Object ACLs | Refused: not implemented | Yes |
| Keys `a` and `a/b` both | Refused, as a file cannot also be a directory | Yes |
| 1,000 small objects | 2.7 seconds to write, and 0.15 to list | 3.5 seconds, and 0.11 |
| Who | Versity Software, with three main contributors | One main maintainer, and a paid edition |
| Advisories published by November 2026 | 8: 1 critical, 2 high | 25: 9 critical, 12 high |

Why it suits Moorage: a service is one for each environment, so what scales with previews is what each instance runs. Plain files make a dump a tar and seeding a preview a
copy. Moorage needs one bucket and one credential on one machine, and none of what SeaweedFS is built for.

What it costs: an app that depends on versioning, lifecycle rules, object ACLs or keys `a` and `a/b` at once needs real S3, or SeaweedFS. Its default metadata is in extended
attributes, which plain `tar` in an Alpine image would lose, which is why it runs with `--sidecar`.

The C++ in its repository is for NVIDIA's cuObject, S3 over RDMA for GPU clusters, behind the build tag `linux && amd64 && cgo && rdma`. The published image and release builds set
`CGO_ENABLED=0`. Moorage pulls an image and talks S3 to it, so that its own rule of no cgo is untouched.

Choosing wrongly is cheap to undo: the manifest says only `type = "s3"`, an app sees the same variables whichever runs, and moving data is an S3 copy.

### What the first test found

`.github/workflows/services-first-test.yml` tried the images under rootless Podman and Docker on 5 November 2026, with `boto3`, the AWS command line and `psql`. It runs when asked, or when it changes. What it settled:

- **PostgreSQL does what this design needs,** under both: on a named volume, on a loopback port and by name on a network, ready in about a second and stopped cleanly by SIGINT in under half a second, with its data surviving a new container and a dump restoring into a fresh volume. At rest it uses 27 MiB.
- **Where a volume goes depends on the image.** It is `/var/lib/postgresql` from 18 on, and `/var/lib/postgresql/data` before that, and an image of someone else's can say otherwise. So it goes by what an image declares, and is recorded when a service is made. A volume in the wrong place would hold nothing, while what the database wrote went in one that nothing looks at again.
- **An older major on a newer volume stops at once,** and does not start empty: `initdb` says that the directory is not empty. So the refusal at deploy is a kindness, and not the only thing between a mistake and lost data.
- **A plain `tar` of Versity's two directories is a dump.** Restored onto fresh volumes, an object comes back with its content type and metadata, which is the claim that decided on `--sidecar`.
- **Everyday S3 works on both,** with real clients: multipart, presigned GET and PUT, CORS, enforced bucket policies, tags, and names in other alphabets. A presigned URL is refused for another host on both, as above.
- **Versity runs as root in its image, as does PostgreSQL's** until it drops to its own user. `tar` and a shell are in Versity's image.
- **An image's digest differs between engines:** one records the list for all platforms, and the other this platform's. So a digest is recorded with the engine that holds it, and is not compared across them.

## One for each environment

A service belongs to an app's environment: production, each named environment and each preview have their own. There is no sharing in the first version. If several apps
could reach one database, then every user of each could reach all of its data.

It needs a table of its own, as `instances.release_id` cannot be empty: name, type, version, image and its digest, volume, host port, the sealed password, the engine that
holds it, and its state. A new package has a `Run` beside the runtime's, and a step like `Adopt` before the listeners start.

- **Containers:** named `moorage-svc-<id>`, so that renaming an app changes nothing. They carry a label of their own, without the instance label, so that the removal of unknown
  instances at start leaves them alone. They run with `--restart=no`, and Moorage starts them again, which avoids needing lingering and systemd units under rootless Podman.
- **Data: named volumes, and no bind mounts.** On macOS and Windows a database belongs inside the engine's VM for speed, and bind mounts there have known problems. Bind mounts
  would also put rootless-owned files in the data directory, where backups and the Files API would trip over them.
- **Stopping:** PostgreSQL gets SIGINT and 120 seconds. Its image warns that even 90 may not be enough, and the engines' default is ten.
- **Provisioning comes before the release:** in `publish`, ahead of its capture, so that a release command can migrate. It waits for a health check run by exec, and a service that does not
  become healthy fails the deployment, with the previous release still serving.
- **Sleep:** a preview's sleep stops its service and keeps its volume, and waking starts the service first. A cron job that runs while asleep starts what it needs, and stops it afterwards.
- **A service the manifest no longer names** stays, with its data, and is shown as unused until `moor services remove`. Nothing deletes data because a line went from a file. An
  environment's deletion removes its services and their volumes, as it does its data directory.
- **Seen like everything else:** in `moor logs`, in events and alert rules, in metrics, in the doctor, and on a Services tab.

## Reaching a service

Moorage's isolation limits binding, not connecting, so anything on the machine can connect to a port on 127.0.0.1. So:

- Every service has its own random password, sealed like a secret. There is no shared or default credential.
- A native app gets `127.0.0.1:<port>`. The port is fixed for the life of the service, and the allocator counts it even while the service is stopped.
- A container app reaches it by name on a network for its environment, and needs no published port.
- The security documents say that this keeps apps apart by password, and not by network, on one machine.

## Which address an app gets

Previews inherit production's variables, so a `DATABASE_URL` set on production would otherwise reach every preview. So, from weakest to strongest:

1. Variables for all environments, and what a preview inherits from production
2. The address of this environment's own service
3. A variable set on this environment itself

So production can use a real database by setting its own `DATABASE_URL`, in which case Moorage starts nothing for it and needs no engine there, while every preview still gets its own.
A preview never ends up on production's database by inheritance.

The address is given when an instance starts, in the same way as other secrets, and is not captured in the release. So it stays out of the command line, the shim's spec on disk and
each release's stored configuration. Redaction includes it, also after adoption. A release whose manifest does not name a service does not get it, so that going back behaves.

## In production

Allowed, and labelled for what it is: one container with no replica, kept by backups. It is a convenience, and not database hosting.

## Previews

- **Empty by default,** with `[release] on_create` to seed them, which runs once. Copying production into previews is how customer data leaks.
- **`seed = "production"` is an opt-in** that restores that service's latest dump, runs `sanitize` before any app connects, and is refused for a preview whose gate is public, for a coding agent's preview, or for a pull
  request from a fork, or that nothing says is not: a preview's branch says which pull request it is before a record of its environment does. Without a `sanitize`, it takes saying so in the manifest. It is done where no app can reach it, and it is on record as unfinished until it is over, so that a server stopping halfway does not leave a copy that nothing has sanitized.
- **Dumps and not clones.** They work across versions and on every platform. Cloning files is much quicker on a filesystem that can do it and useless on ext4, so it can be an optimisation later.
- `previews = "none"` for an app that needs none, and the console shows how many services are running and what they use, as they add up.

## Backups and upgrades

- **Server backups include a dump** of each production and named service: `pg_dumpall`, as an app can make other databases and roles, or a tar of the S3 directories. One that is asleep is started for it, where no app can reach it. It is written to `data/<app>/.services/<env>/`, so that the backup's format
  does not change and older versions can still read it, and not in an environment's data directory, which its app's user owns: it could leave a link there for the server to follow, and a preview's code could read production's. A dump that fails is reported, and does not lose the rest of the backup.
- **Volumes are not in the backup.** A restore to a new machine makes the service afresh and loads its dump, where no app can reach it, as one that connected to an empty database would migrate it and the dump would not then load. It is on record that a volume holds nothing to keep until that has finished, so that a try that does not get to the end starts again from nothing, and the next backup does not replace the only good dump. Only the engine saying that there is no volume means that: one having trouble says nothing of what it holds. On the machine that it ran on, a service keeps what it holds, and `moor services restore` goes back to the dump.
- **Minor updates** are explicit: `moor services update db`.
- **A major version change is refused at deploy,** with instructions. `moor services upgrade` then does a dump with the newer tools, a new volume, a restore and a check, and keeps the old
  volume until told. The images hold only their own major version, so `pg_upgrade` is not available. What can refuse it does so before anything stops. Then the app stops, with its usual drain, and commands and cron jobs are stopped and refused. The record names the new volume before it starts, once that holds everything, so that a server stopping at any moment comes back on one that is whole.

## What will surprise people

- On macOS and Windows, `podman machine rm` takes the data with it, so the documentation says to keep dumps.
- A Windows service's own account has to be able to reach an engine, which a per-user Docker Desktop will not allow.
- CI has real engines only on Linux, and the documents say so.
- Running without an engine at all is possible with relocatable PostgreSQL binaries, with limited platform coverage and unconfirmed signatures. Worth revisiting only if people ask.
