# Services

An app can ask for a database or an object store in its manifest, and Moorage runs it and connects the app to it. Each environment gets its own, so that a branch's preview has a database of its own to break.

```toml
[services.db]
type = "postgres"
```

That is all it takes. On the next deployment, Moorage starts PostgreSQL 18 for that environment, waits for it to be ready, and then gives your app `DATABASE_URL`. A release command can migrate, as the service is ready before it runs.

**This is the one thing in Moorage, apart from apps in containers, that needs [a container engine](containers.md#the-engine).** Podman or Docker has to be there for an app that asks for a service, and nothing else cares. A deployment that asks for one with no engine answering says so before it builds anything.

## In the manifest

| Key | Means | Default |
|---|---|---|
| `type` | `postgres` or `s3` | Required |
| `version` | For `postgres`, `15` to `19` | `18`. It is recorded when the service is made, and does not move by itself |
| `image` | For `postgres`, another image based on the official one, such as `pgvector/pgvector:pg18`, which is the way to get extensions. It needs a tag other than `latest` | The official image |
| `env` | Another name for the variable, or for `s3` the start of each, needed for a second of one type | `DATABASE_URL`, or `S3_ENDPOINT` and its fellows |
| `memory` | A limit, such as `512MiB` | None |
| `previews` | `own`, or `none` for previews to have none | `own` |
| `seed` | `production`, for a new preview to start with a copy. See [what a preview starts with](#what-a-preview-starts-with) | Empty |
| `sanitize` | With `seed`, a script in the app that removes what should not be copied, or `none` to say that nothing should | |

Two of a type need different variables:

```toml
[services.db]
type = "postgres"

[services.analytics]
type = "postgres"
version = "16"
env = "ANALYTICS_DATABASE_URL"
```

## An object store

```toml
[services.files]
type = "s3"
```

It gets one bucket, and one key that can do anything in it:

| Variable | Is |
|---|---|
| `S3_ENDPOINT` and `AWS_ENDPOINT_URL` | Its address, such as `http://127.0.0.1:27014` |
| `S3_BUCKET` | `app` |
| `S3_REGION` and `AWS_REGION` | `us-east-1`, which means nothing here and which clients want |
| `S3_ACCESS_KEY_ID` and `AWS_ACCESS_KEY_ID` | Its key |
| `S3_SECRET_ACCESS_KEY` and `AWS_SECRET_ACCESS_KEY` | Its secret key, kept out of what the app prints |

With `env = "UPLOADS"`, they are `UPLOADS_ENDPOINT` and so on, and the `AWS_` names are left for another. Clients need path-style addresses, as there is no name for a bucket to go in front of: `force_path_style` or `addressing_style = "path"`, depending on the client.

It is [Versity Gateway](https://github.com/versity/versitygw), which keeps a bucket as a directory and an object as a file. Everyday S3 works: uploading in parts, presigned URLs, CORS and bucket policies. **What does not:** versioning, lifecycle rules, object ACLs, and keys `a` and `a/b` at once, as a file cannot also be a directory. An app that needs those needs real S3, which it can have by setting these variables on the environment itself, as below.

A presigned URL is signed for its host, so that a browser elsewhere cannot use one for `127.0.0.1`. For now, this is for what an app reads and writes for itself, and it has no public address.

## Which address an app gets

From weakest to strongest:

1. Variables for all environments, and what a preview inherits from production
2. The address of this environment's own service
3. A variable set on this environment itself

So production can use a real database by setting its own `DATABASE_URL`, in which case Moorage starts nothing for it and needs no engine for it, while every preview still gets its own. **A preview never ends up on production's database by inheritance:** with `previews = "none"`, what it would inherit of that name is left out.

The address is given as an instance starts, as a secret is, along with its password alone as `DATABASE_URL_PASSWORD`. Neither is in a release or on a command line, and both are kept out of what the app prints. An app that runs natively gets `127.0.0.1` and a port that stays the same for the life of the service. One in a container reaches it by name, on a network for its environment.

## What happens to it

| When | What happens |
|---|---|
| You deploy | It is made if it is not there, and started if it has stopped. If it does not become ready, the deployment fails, and the previous release goes on serving |
| It stops by itself, or the machine restarts | Moorage starts it again |
| The server stops or is upgraded | It goes on running, like your apps |
| The environment goes to sleep | It stops, and keeps what it holds, unless a process with `sleep = false` or a command is still using it. Waking starts it first, and a command or a cron job starts it for as long as it runs |
| The manifest stops naming it | It stays, with what it holds, marked as unused. A release that does not name it does not get its address |
| The environment is deleted | It goes, with what it holds |
| The engine stops answering | What is running is left alone. Deploying, restarting, putting to sleep or deleting says why it cannot, and changes nothing. `server.engine_down` says so, to [the rules that ask](notifications.md) |

A manifest that asks for another major version than a service holds is refused, as that takes moving its data. PostgreSQL itself also refuses to start on a newer version's files, so this is not all that stands between a mistake and lost data.

## What a preview starts with

**Empty,** unless the manifest says otherwise, with `[release] on_create` to put something in it, which runs once. Copying production into previews is how customer data leaks.

To copy it anyway, it takes saying so, and saying what removes what should not be copied:

```toml
[environments.preview]
access = "login"

[services.db]
type = "postgres"
seed = "production"
sanitize = "scripts/sanitize.sql"
```

A new preview then starts with production's latest dump, which [a backup](backups.md) makes. The copy and the script happen where nothing can connect, and if either fails, or the server stops halfway, nothing is left of the copy, and the deployment fails. The script is in the app, and a link in its place that leads out of the app is refused. `sanitize = "none"` says that nothing needs removing.

A preview starts empty all the same, and its deployment's log says why, if anyone can reach it, if it is the preview of a pull request that comes from a fork, or that nothing says does not, or if a [coding agent's token](agents.md) made it. An existing preview is not seeded again: destroy it, or remove its service. A later commit that would let anyone reach a preview holding such a copy fails to deploy, and says so.

## Backups

A [backup](backups.md) holds a dump of each service of production and of the named environments, made just before: `pg_dumpall`, or a tar of the object store. One that is asleep is started for that, where no app can reach it, and one that has lost its volume is left with the dump that it has. A dump that fails or does not get to its end is reported, the last good one stays, and the rest of the backup goes on. Volumes are not in a backup, and previews are not dumped. Dumps go in `.services` in the app's directory of data, where apps that run as [users of their own](isolation.md) cannot reach them. Without that, an app's code can read them, as it can read anything else of the server's.

After a restore to a new machine, a service comes up on a fresh volume and loads its dump by itself, before anything can connect to it. So does one whose volume has gone for any other reason, and if that does not get to the end, it starts again from nothing on the next try. What was written after the last dump is gone.

On the machine that it ran on, a service still has its volume, and so keeps what it holds, which may be newer than the backup. To go back to the dump:

```bash
moor services restore db --yes
```

PostgreSQL's dump goes into a new volume first, so that one that will not load changes nothing. The old volume stays until you are happy, with `moor services upgrade db --finish`. An object store's dump is checked, and replaces what it holds.

If one of an object store's two volumes has gone, it does not start, and says so: the other may be all that there is of what it held, so that going back to its dump is for you to ask for.

## Updates and upgrades

Nothing changes an image by itself: which one it runs is recorded when a service is made.

```bash
moor services update db
```

That takes the newest image of its version, such as 18.7 where it ran 18.6, and restarts it. One that is asleep stays asleep, and runs it when it wakes.

A new major version takes moving its data, as an image holds only its own version:

```bash
moor services upgrade db --to 19
```

The app stops for that, as what it wrote after the dump would be lost, and commands and cron jobs are stopped and refused until it is over. Moorage dumps the service with the newer version's tools, restores into a new volume, checks that the database `app` holds as many rows and tables and that the same databases are there, and switches. Anything that would refuse it does so before anything stops. Anything going wrong after that leaves the old one as it was, and the app starts again however it goes. Say the new version in the manifest too, if it names one. A service with an `image` of its own has to say which holds the new version, with `--image`. The old volume stays until you are happy:

```bash
moor services upgrade db --finish
```

## Seeing and removing them

```bash
moor services --app shop
```

```bash
moor services logs db --env staging
```

```bash
moor services remove db --env my-branch --yes
```

They are on the Services tab of an app in the console too, and `moor server doctor` says if one that should be running is not. Removing one takes what it holds, for good. If the manifest still names it, the next deployment makes a new one, which starts empty.

## How far to trust this

**In production, this is one container with no replica.** It is a convenience, and not database hosting. For a database that matters, set `DATABASE_URL` on production to one that is looked after, and let previews use their own.

**Apps on a machine are kept apart from a service by its password, and not by the network.** Each has a random password of its own, but a port on the loopback address can be reached by anything on that machine. See [how apps are kept apart](isolation.md).

On macOS and Windows, the data lives inside the engine's machine, so that `podman machine rm` takes it with it.
