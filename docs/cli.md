# Command line

Every command of `moor`, as its `--help` says it. This page is made from the commands themselves, and a test keeps
it the same as they are. [The documentation](README.md) lists the common ones.

An app is chosen with `--app`, `$MOOR_APP`, `[app].name` in the `moorage.toml` of the current directory, or the repository's
`moorage` git remote.

Commands that talk to a server also take these, which are left out below:

| Flag | What it says |
|---|---|
| `--server URL` | The server. By default `$MOOR_SERVER`, or the server configured on this machine |
| `--token TOKEN` | An API token. By default `$MOOR_TOKEN`, or the admin token of the server on this machine |
| `--ca-cert FILE` | A PEM file of an extra CA to trust, such as the server's internal CA. By default `$MOOR_CA_CERT` |

## `moor access`

Each environment has a gate, which applies to everything it serves:

```text
  public    anyone
  password  a shared password; scripts send it with HTTP Basic
  login     people signed in to Moorage, with a role in its project, or with
            --any-user anyone with an account; the app hears who they are, and
            scripts send an API token with the gate scope
  share     only people with a share link
```

A share link works with every gate. A list of addresses can be combined with any of them. Unless one is set here, the gate is what the manifest asks, or else the server's default for that kind of environment.

```text
moor access
```

```text
moor access
moor access set password --env staging
moor access set login --allow-cidr 10.0.0.0/8
moor access set default --env staging
moor share --expires 3d
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--json` |  | print as JSON |

### `moor access set`

Change an environment's gate, at once. default leaves it to the manifest and the server. The password gate asks for its password, without echoing it, or reads it from standard input. Changing the gate or its password ends everyone's way in.

--allow-cidr replaces the list of addresses it can be reached from, and --anywhere takes the list away. Otherwise the list stays as it is.

```text
moor access set public|password|login|share|default
```

| Flag | Default | What it does |
|---|---|---|
| `--allow-cidr strings` |  | addresses it can be reached from, such as 10.0.0.0/8 |
| `--any-user` |  | with login, let in anyone with an account |
| `--anywhere` |  | take away the list of addresses |
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--password` |  | set the password too, for when the manifest asks for the gate |

## `moor alerts`

List an environment's thresholds. Past one for as long as it says, a notification goes to whoever a rule sends "A threshold is crossed" to, and another when it comes right.

```text
moor alerts
```

```text
moor alerts add error-rate 5 --for 5m
moor alerts add latency-p95 800 --for 10m --env staging
moor alerts add memory 90
moor alerts rm thr_01J…
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--json` |  | print JSON |

### `moor alerts add`

Add a threshold. METRIC is one of:

```text
  error-rate    the percentage of answers that are 5xx, over the last minute
  latency-p95   how long the slowest 5 in 100 requests took at least, in milliseconds
  memory        the instance using the most, as a percentage of its [resources] limit
  cpu           the instance using the most, as a percentage of one core
```

A minute with no requests in it, or fewer than 5 for an error rate, says nothing either way.

```text
moor alerts add METRIC ABOVE
```

```text
moor alerts add error-rate 5 --for 5m
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--for string` | `5m` | how long it has to go on for |

### `moor alerts rm`

Remove a threshold

```text
moor alerts rm ID
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |

## `moor apps`

List the apps

```text
moor apps
```

| Flag | Default | What it does |
|---|---|---|
| `--json` |  | print as JSON |

### `moor apps create`

Create an app, before its first deployment. Deploying also creates one, in the default project: this is how an app starts in another.

```text
moor apps create NAME
```

```text
moor apps create shop --project payments
```

| Flag | Default | What it does |
|---|---|---|
| `--project string` |  | the project it goes in (default: the default project) |

### `moor apps delete`

Delete an app: its instances, variables, releases, builds, logs and repository, and its data directory unless --keep-data keeps it. This cannot be undone. It asks you to type the app's name first, unless --yes.

```text
moor apps delete NAME
```

```text
moor apps delete shop
moor apps delete shop --keep-data --yes
```

| Flag | Default | What it does |
|---|---|---|
| `--keep-data` |  | keep its data directory |
| `--yes` |  | do not ask |

### `moor apps info`

Show an app's address and project, and how much room it takes

```text
moor apps info NAME
```

| Flag | Default | What it does |
|---|---|---|
| `--json` |  | print as JSON |

### `moor apps move`

Put an app in another project. It takes being a maintainer of both.

```text
moor apps move NAME --project PROJECT
```

```text
moor apps move shop --project payments
```

| Flag | Default | What it does |
|---|---|---|
| `--project string` |  | the project it goes in |

### `moor apps rename`

Give an app another name, which is another address and another git remote. Its instances stop and start again under the new name, so there is a moment of downtime.

Change \[app\].name in its moorage.toml, and its git remote, to match: a deploy by the old name makes a new app.

```text
moor apps rename NAME NEW
```

```text
moor apps rename shop store
```

## `moor audit`

Show who did what, newest first: every change through the API or the console, every sign-in, every push and every secret revealed, whether it worked or not, kept for a year. It takes an administrator.

```text
moor audit
```

```text
moor audit -n 20
moor audit --actor ana@example.com --action signin
```

| Flag | Default | What it does |
|---|---|---|
| `--action string` |  | only actions with this text |
| `--actor string` |  | only what this person did |
| `--json` |  | print as JSON |
| `-n, --number int` | `50` | how many, up to 200 |
| `--target string` |  | only what was done to this |

## `moor context`

A context is a server and the token for it, saved by moor login. Commands use the current one, unless --server or $MOOR\_SERVER says otherwise.

```text
moor context
```

### `moor context rm`

Forget a context, without revoking its token

```text
moor context rm NAME
```

### `moor context use`

Make a context the current one

```text
moor context use NAME
```

## `moor cron`

Cron jobs are declared in moorage.toml as \[\[cron\]\], and run as one-off instances of the current release. The last 100 runs of each are kept, with what they said.

```text
moor cron
```

```text
moor cron
moor cron run nightly-cleanup
moor cron runs nightly-cleanup
moor cron log run_01J…
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--json` |  | print as JSON |

### `moor cron log`

Show what a run said, and go on showing it until it ends

```text
moor cron log RUN
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |

### `moor cron run`

Run a cron job now, whatever its schedule says

```text
moor cron run JOB
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--detach` |  | start it and return |
| `--env string` | `production` | environment |

### `moor cron runs`

List an environment's runs, newest first; a job's, if it is named

```text
moor cron runs [JOB]
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--json` |  | print as JSON |

## `moor deploy`

Deploy to an app's production environment. The app is created if it does not exist yet.

Without a flag, DIR (default: the current directory) is source: it is uploaded, leaving out what .gitignore and .moorignore exclude, and the server builds it as moorage.toml says (or as it detects: Go, Python, Node.js, a static site), runs the release command, and rolls the new release out. The build's output is shown as it happens; Ctrl-C stops following, not the deployment.

With --static, the files under DIR are uploaded and served as they are, following the \[static\] rules of moorage.toml in the current directory. DIR defaults to \[static\].dir in moorage.toml, or the current directory.

With --artifact, DIR (default: the current directory) holds an app that is ready to run, such as a compiled binary; the \[processes\] of moorage.toml in the current directory say how to run it.

With --image, the server fetches an image and runs it in a container, pinned to the digest it has now. The moorage.toml in DIR (default: the current directory) is sent with it, if there is one, and no other file. A private registry needs its credentials on the server: see moor registry login.

A new release takes over once its instances are healthy; until then the previous release keeps serving.

```text
moor deploy [DIR]
```

```text
moor deploy                       # build and deploy the current directory
moor deploy ./api --app api
moor deploy --static ./public --app docs
moor deploy --artifact ./dist --app api
moor deploy --image ghcr.io/acme/api:1.4 --app api
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app to deploy to (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--artifact` |  | upload an app that is ready to run and run its \[processes\] |
| `--detach` |  | return once the deployment is queued, without following it |
| `--env string` | `production` | environment to deploy to |
| `--image string` |  | run this image in a container, pinned to its digest, instead of building DIR |
| `--static` |  | upload a directory of files and serve them as they are |

## `moor deployments`

List an app's latest deployments: what was deployed, how it went, and the release it made. Show one with 'moor deployments show ID', and follow one that is running with 'moor deployments follow ID'.

```text
moor deployments
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--json` |  | print as JSON |
| `--limit int` | `20` | how many to list |

### `moor deployments follow`

Print a deployment's log from its start and, while it runs, as it is written. Fails if the deployment failed.

```text
moor deployments follow ID
```

### `moor deployments show`

Show a deployment

```text
moor deployments show ID
```

| Flag | Default | What it does |
|---|---|---|
| `--json` |  | print as JSON |

## `moor domains`

Attach a hostname of your own to an environment, or a path on one, so that several apps can share a hostname. Its DNS must point here before it serves: a CNAME to the environment's own hostname, or an address of this server.

A path is kept in what the app hears, which X-Forwarded-Prefix also says, unless --strip-prefix takes it away. Redirects and cookies from the app are made to fit it. Links in what the app sends are its own affair.

```text
moor domains
```

```text
moor domains add portal.example.com
moor domains add tools.example.com/reports --app reports
moor domains verify portal.example.com
moor domains remove portal.example.com
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--json` |  | print as JSON |

### `moor domains add`

Attach a domain, which serves once its DNS points here

```text
moor domains add HOSTNAME[/PATH]
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--strip-prefix` |  | take the path away from what the app hears |

### `moor domains remove`

Detach a domain, at once

```text
moor domains remove HOSTNAME[/PATH]
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |

### `moor domains verify`

Look now at where a domain points

```text
moor domains verify HOSTNAME[/PATH]
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |

## `moor env`

Variables set on the server configure an app without changing its code. They override the \[env\] of moorage.toml, and a variable set for one environment overrides one set for every environment (--all-envs).

Secret values (--secret) are encrypted on the server and never shown again; they are kept out of the logs. Every change makes a new release of the current build, rolled out without downtime (--no-restart keeps the change for the next deploy).

```text
moor env
```

```text
moor env
moor env set LOG_LEVEL=debug FEATURE_X=on
moor env set --secret STRIPE_KEY        # asks for the value
moor env set --secret DB_PASSWORD < password.txt
moor env unset FEATURE_X
moor env import .env --secret
moor env export > production.env
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--json` |  | print as JSON |

### `moor env export`

Print the variables set on the server for an environment, as a .env file. Secrets are listed as comments, without their values.

```text
moor env export
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |

### `moor env import`

Set the variables of a .env file (NAME=value lines; \# comments; values may be quoted, and "export " in front of a line is allowed) in one change. Use - to read standard input.

```text
moor env import FILE
```

| Flag | Default | What it does |
|---|---|---|
| `--all-envs` |  | change the variables of every environment of the app |
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--build` |  | make the variables available to builds too |
| `--env string` | `production` | environment |
| `--no-restart` |  | keep the change for the next deploy instead of releasing it now |
| `--production-only` |  | keep previews from inheriting the variables |
| `--secret` |  | store the values encrypted and never show them again |

### `moor env list`

List an app's variables

```text
moor env list
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--json` |  | print as JSON |

### `moor env reveal`

Print a secret's value. It takes being a maintainer of the app's project, and is recorded in the audit log.

```text
moor env reveal NAME
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |

### `moor env set`

Set variables and release the change: a new release of the current build, rolled out without downtime.

A secret given as NAME alone, without a value, is read from standard input, or asked for without echoing it, so that it stays out of the shell's history.

```text
moor env set NAME=value… | set --secret NAME
```

| Flag | Default | What it does |
|---|---|---|
| `--all-envs` |  | change the variables of every environment of the app |
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--build` |  | make the variables available to builds too |
| `--env string` | `production` | environment |
| `--no-restart` |  | keep the change for the next deploy instead of releasing it now |
| `--production-only` |  | keep previews from inheriting the variables |
| `--secret` |  | store the values encrypted and never show them again |

### `moor env unset`

Unset variables and release the change

```text
moor env unset NAME…
```

| Flag | Default | What it does |
|---|---|---|
| `--all-envs` |  | change the variables of every environment of the app |
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--no-restart` |  | keep the change for the next deploy instead of releasing it now |

## `moor environments`

An environment is a place an app runs, with its own address, variables, data and releases. Every app has production. A named environment, such as staging, stays until it is removed; a preview comes and goes with its branch.

An environment other than production is at \<app\>--\<environment\> beside the app's own address, and asks search engines not to index it.

```text
moor environments
```

```text
moor environments --app shop
moor environments create staging --branch develop
moor deploy --env staging
moor promote --from staging
moor environments delete staging
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--json` |  | print as JSON |

### `moor environments create`

Make a named environment. It starts with the variables an app has for every environment and an empty data directory, and has nothing to serve until something is deployed to it. With --branch, a push to that branch deploys to it.

```text
moor environments create NAME
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--branch string` |  | the branch whose pushes deploy to it |

### `moor environments delete`

Remove an environment, with its data, logs and releases. Production cannot be removed: delete the app for that.

```text
moor environments delete NAME
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |

## `moor events`

Show what happened on the server, oldest first: deployments, releases, instances that exited or sickened, and changes of variables (their names, never their values). Events are kept for 90 days. -f goes on showing them as they happen.

```text
moor events
```

```text
moor events
moor events --app shop -n 20
moor events -f
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | only what happened to this app |
| `--env string` |  | only what happened to this environment |
| `-f, --follow` |  | go on showing them as they happen |
| `--json` |  | print as JSON |
| `-n, --limit int` | `50` | how many to show |

## `moor exec`

Run a command in a container that is running, as seen in moor ps, in among what it has open and has written, which a new one-off instance would not be. moor ends with the command's own exit status. What is said in it is not kept.

An app that is not in a container has nothing of its own to go into, as a process sees the same files from anywhere: moor run has them, with its variables and its user.

```text
moor exec INSTANCE [flags] -- COMMAND [ARG...]
```

```text
moor exec ins_01J… -it -- /bin/sh
moor exec ins_01J… -- ps aux
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `-i, --interactive` |  | send what is typed, or standard input, to it |
| `--timeout string` |  | how long it may take, such as 10m (default 1h) |
| `-t, --tty` |  | give it a terminal |

## `moor files`

An environment's data directory is where its app keeps what it means to keep, and each environment has its own. This lists a directory within it, from the top if you do not say which. A link that leads out of it is not followed, and neither is anything else an app might leave there to mislead the server.

```text
moor files [PATH]
```

```text
moor files
moor files uploads --env staging
moor files get backups/nightly.sql
moor files put seed.sql
moor files rm old.log
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--json` |  | print JSON |

### `moor files get`

Copy a file out of a data directory, to the name it has here, or to DESTINATION. - is standard output.

```text
moor files get PATH [DESTINATION]
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |

### `moor files mkdir`

Make a directory in a data directory

```text
moor files mkdir PATH
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |

### `moor files put`

Copy a file into a data directory, at the top with the name it has unless PATH says where. It appears all at once, belongs to the app, and stops at something already there unless --replace says otherwise. Up to 1 GB. The directory has to be there: moor files mkdir.

```text
moor files put FILE [PATH]
```

```text
moor files put seed.sql
moor files put build/index.bin indexes/current.bin --replace
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--replace` |  | take the place of what is already there |

### `moor files rm`

Remove a file, a link or an empty directory from a data directory, or with -r a directory and all that is in it. There is no way back but a backup.

```text
moor files rm PATH
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `-r, --recursive` |  | a directory and all that is in it |

## `moor forge`

Connect an app to a repository on a forge. A push to its default branch deploys to production, a push to a branch that an environment names deploys to that environment, and each pull request gets a preview of its own, which reports back to the pull request if the server has a token to do it with.

A pull request from a fork, or from someone the forge does not say may push there, is not built until someone approves that commit: building it would run their code here.

```text
moor forge
```

```text
moor forge connect https://github.com/acme/shop.git --token-stdin
moor forge pulls
moor forge approve 42 1f3a9c07
moor forge sync
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--json` |  | print JSON |

### `moor forge approve`

Have a pull request built, at the commit that you looked at, as moor forge pulls shows it. Building it runs its code on this server, and its preview has the app's preview variables, so look at it first. This is for that one commit: if it has moved on, nothing is built, and another commit needs approving too. It also builds again one that failed.

```text
moor forge approve NUMBER COMMIT
```

```text
moor forge approve 42 1f3a9c07
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |

### `moor forge connect`

Connect an app to a repository, by its https address. It answers with an address and a secret for the forge's webhook settings: the secret is shown this once.

A token reads a private repository, and sets the checks and comments on pull requests. Give it on standard input, or be asked for it, so that it stays out of your history. A server that a forge cannot reach can look for itself, every minute, with --poll: it then sees branches, and no pull requests.

Connecting deploys nothing. The next push does, or: moor forge sync

```text
moor forge connect URL
```

```text
moor forge connect https://github.com/acme/shop.git --token-stdin < token
moor forge connect https://git.example.com/acme/shop.git --kind gitea
moor forge connect https://github.com/acme/shop.git --poll
```

| Flag | Default | What it does |
|---|---|---|
| `--api-url string` |  | where the forge's API is, if not where it usually is |
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--branch string` |  | its production branch (default: what the repository says) |
| `--kind string` |  | github, gitlab, gitea or generic (github.com and gitlab.com need not say) |
| `--no-previews` |  | leave pull requests alone |
| `--poll` |  | look every minute, for a server that the forge cannot reach |
| `--token-stdin` |  | read a token from standard input, or ask for it |

### `moor forge disconnect`

Let go of a repository, its secret and its token. What it deployed stays, previews too.

```text
moor forge disconnect
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |

### `moor forge pulls`

List pull requests and their previews

```text
moor forge pulls
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--json` |  | print JSON |

### `moor forge sync`

Have the server look at the repository now, and deploy what has changed. This is how what is there already gets its first deployment, and how one that failed gets another try.

```text
moor forge sync
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |

## `moor git`

Push to an app's repository on the server to deploy it: the production branch (main, unless the first push named another) deploys to production.

```text
  moor git remote            add the "moorage" remote to this repository
  git push moorage main      deploy; the build is shown as it happens
```

### `moor git remote`

Add a git remote for the app to the repository in the current directory, so that git push deploys it. The remote uses SSH, with a key registered with moor keys add; with --http, it uses HTTPS and the API token, which moor git credential gives git.

The app is --app, $MOOR\_APP, \[app\].name in moorage.toml, or the directory's name. It need not exist yet: the first push creates it.

```text
moor git remote
```

```text
moor git remote
moor git remote --http
git push moorage main
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP, \[app\].name in moorage.toml, or the directory's name) |
| `--http` |  | push over HTTPS with the API token instead of SSH |
| `--name string` | `moorage` | the remote's name |

## `moor keys`

List the SSH public keys that may push to the apps' repositories over SSH. Register yours with 'moor keys add', remove one with 'moor keys remove'.

```text
moor keys
```

| Flag | Default | What it does |
|---|---|---|
| `--json` |  | print as JSON |

### `moor keys add`

Register an SSH public key, so that git pushes over SSH signed with it are accepted. FILE is a .pub file, or - for standard input; without it, the first of ~/.ssh/id\_ed25519.pub, id\_ecdsa.pub and id\_rsa.pub. The key is named after its comment unless --name says otherwise.

```text
moor keys add [FILE]
```

```text
moor keys add
moor keys add ~/.ssh/work.pub --name work-laptop
```

| Flag | Default | What it does |
|---|---|---|
| `--name string` |  | the key's name (default: its comment) |

### `moor keys remove`

Remove an SSH key

```text
moor keys remove NAME|FINGERPRINT
```

## `moor lock`

Stop deployments to an environment until it is unlocked, as after a rollback, while what went wrong is looked into. Rollbacks and changes of variables go on working.

```text
moor lock
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |

## `moor login`

Sign in to a server, and save it as a context for later commands. moor login opens the server's approval page in your browser, and receives a token when you approve it; no password is typed into the terminal.

On a machine without a browser, open the address it prints on any other machine, or pass an API token on standard input with --with-token.

URL is the server's console, such as https://moorage.apps.example.com. It defaults to $MOOR\_SERVER, the current context's server, or the server on this machine.

```text
moor login [URL]
```

```text
moor login https://moorage.apps.example.com
moor login https://moorage.apps.example.com --no-browser
moor login https://moorage.apps.example.com --with-token < token.txt
```

| Flag | Default | What it does |
|---|---|---|
| `--name string` |  | the context's name (default: the server's hostname) |
| `--no-browser` |  | print the approval page's address without opening a browser |
| `--with-token` |  | read an API token from standard input instead of opening a browser |

## `moor logout`

Sign out of the current server, revoking the token

```text
moor logout
```

## `moor logs`

Show the output of an app's instances, merged by time. Lines marked with \* are notes from Moorage (starts, exits, restarts); lines marked with ! came from standard error.

--source chooses what to show, from stdout, stderr, moorage, build, access and events; the first three unless asked.

```text
moor logs
```

```text
moor logs --app api
moor logs -f --app api --process worker
moor logs --app api --source stderr,build --since 1h --grep timeout
moor logs --app api --problems --since 24h
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `-f, --follow` |  | keep showing new lines |
| `--grep string` |  | show the lines with this text, whatever its case |
| `--instance string` |  | show one instance only: its number or its ID |
| `--json` |  | print each line as JSON |
| `--problems` |  | show the lines that report a problem: errors, warnings, standard error, failed requests |
| `--process string` |  | show one process only, such as web |
| `--since string` |  | show lines since then: how long ago (15m, 2h, 7d) or a moment |
| `--source string` |  | sources to show, separated by commas: stdout, stderr, moorage, build, access, events |
| `-n, --tail int` | `100` | lines to show from the end |
| `--until string` |  | show lines until then |

## `moor manifest`

moorage.toml tells Moorage how to build and run an app. These commands check it, show what Moorage will do with an app, and print the manifest's JSON Schema for editors.

### `moor manifest explain`

Show the plan Moorage makes for the app in DIR (default: the current directory) from its moorage.toml and its files: the runtime, the toolchains, the build commands, the processes and their environment. The server makes the same plan when it builds the app; toolchain versions are resolved then.

```text
moor manifest explain [DIR]
```

| Flag | Default | What it does |
|---|---|---|
| `--json` |  | print the plan as JSON |
| `--os string` | this system | operating system of the server (linux, darwin or windows) |

### `moor manifest schema`

Print the JSON Schema of moorage.toml, for editors that validate and complete TOML files (for example with a "\#:schema ./moorage.schema.json" comment at the top of the manifest).

```text
moor manifest schema
```

### `moor manifest validate`

Check the moorage.toml in DIR (default: the current directory): its syntax, keys and values. Warnings point at values that look like credentials and at settings this version accepts but does not act on yet.

```text
moor manifest validate [DIR]
```

## `moor metrics`

Show an environment's requests, response times, CPU and memory, as a line of text for each, with its latest value. --range is live, 1h, 24h, 7d or 30d. The console's Metrics tab has the same as charts.

```text
moor metrics
```

```text
moor metrics
moor metrics --range 24h --series latency
moor metrics --env staging --json
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--json` |  | print as JSON |
| `--range string` | `1h` | live, 1h, 24h, 7d or 30d |
| `--series string` |  | only requests, latency, cpu or memory |

## `moor notifications`

Show what you have been told: of failed deployments, apps that keep crashing and cron jobs that fail in the projects you maintain, and of your own deployments that fail. Channels and rules send the same to webhooks, chat and email.

```text
moor notifications
```

```text
moor notifications
moor notifications --read
moor notifications channels add alerts --kind slack
moor notifications rules add --channel alerts --project web
```

| Flag | Default | What it does |
|---|---|---|
| `--json` |  | print as JSON |
| `--read` |  | mark all you have been told as read |

### `moor notifications channels`

List where notifications can go. Adding, testing and removing channels takes an admin, who also sees where each one goes.

```text
moor notifications channels
```

| Flag | Default | What it does |
|---|---|---|
| `--json` |  | print as JSON |

#### `moor notifications channels add`

Add somewhere notifications can go. A webhook, slack, teams or discord channel asks for its address, which is a secret, unless --url gives it, and also reads it from standard input. Email asks for the mail server's password if --username gives one to sign in with. A webhook's signing secret is shown once, here. All of it is sealed with the server's master key.

```text
moor notifications channels add NAME
```

```text
moor notifications channels add alerts --kind slack
moor notifications channels add ops --kind webhook --url https://example.com/hook
moor notifications channels add mail --kind email --host smtp.example.com --from moorage@example.com --to ops@example.com
```

| Flag | Default | What it does |
|---|---|---|
| `--from string` |  | the address mail comes from |
| `--host string` |  | the mail server |
| `--kind string` | `webhook` | webhook, slack, teams, discord or email |
| `--port int` |  | the mail server's port (587 if not given; 465 means TLS from the start) |
| `--to strings` |  | the addresses mail goes to |
| `--url string` |  | the webhook's address |
| `--username string` |  | whom to sign in to the mail server as |

#### `moor notifications channels remove`

Remove a channel, and the rules that use it

```text
moor notifications channels remove NAME
```

#### `moor notifications channels test`

Send a channel a test, and say how it went

```text
moor notifications channels test NAME
```

### `moor notifications rules`

List which events go to which channel, of what you can see. A rule of the whole server takes an admin, and one of a project or an app takes a maintainer of it.

```text
moor notifications rules
```

| Flag | Default | What it does |
|---|---|---|
| `--json` |  | print as JSON |

#### `moor notifications rules add`

Send events to a channel: of an app, or a project, or the whole server if neither is given. Without --events it asks for failures: deployment.failed, instance.crashloop, cron.failed, server.disk\_low, backup.failed and server.certificate\_failed. It can also ask for instance.unhealthy and deployment.live, or for \* to have them all. A channel that is told something went wrong is also told when it has come right.

```text
moor notifications rules add
```

```text
moor notifications rules add --channel alerts
moor notifications rules add --channel alerts --project web
moor notifications rules add --channel ops --app shop --events deployment.failed,deployment.live
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | only for this app |
| `--channel string` |  | the channel it goes to |
| `--events strings` |  | what it asks for |
| `--project string` |  | only for this project |

#### `moor notifications rules remove`

Remove a rule

```text
moor notifications rules remove ID
```

## `moor password`

Change your password. It asks for the current one and a new one, without showing them, or reads both from standard input, one on each line. Your other sessions end; API tokens and SSH keys go on working.

```text
moor password
```

## `moor previews`

Pushing a branch makes a preview of it: its own build, at its own address, with the variables of production (except those marked --production-only) and an empty data directory. Deleting the branch destroys it, and so does going unused for the time \[previews\].ttl says, seven days unless told otherwise, unless it is pinned. An app may have ten at a time.

```text
moor previews
```

```text
moor previews
moor previews create feature/new-pricing --app shop
moor previews pin feature-new-pricing --app shop
moor previews destroy feature-new-pricing --app shop
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | only the previews of this app |
| `--json` |  | print as JSON |

### `moor previews create`

Make a preview of a branch that was pushed

```text
moor previews create BRANCH
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--detach` |  | queue the deployment and return |

### `moor previews destroy`

Destroy a preview, with its data, logs and releases. Its branch stays, and a new push to it makes another.

```text
moor previews destroy NAME
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |

### `moor previews pin`

Keep a preview from expiring

```text
moor previews pin NAME
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |

### `moor previews unpin`

Let a preview expire again

```text
moor previews unpin NAME
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |

## `moor projects`

Apps belong to projects, whose members have roles: viewers see an app and its logs, deployers also deploy, restart, roll back and scale it, and maintainers also change its variables, its members and its fate. Administrators of the server can do everything everywhere.

Every app starts in the default project, which everyone is a maintainer of until that is changed.

```text
moor projects
```

| Flag | Default | What it does |
|---|---|---|
| `--json` |  | print as JSON |

### `moor projects add-member`

Give someone a role in a project

```text
moor projects add-member NAME EMAIL
```

| Flag | Default | What it does |
|---|---|---|
| `--role string` | `deployer` | viewer, deployer or maintainer |

### `moor projects create`

Create a project

```text
moor projects create NAME
```

### `moor projects delete`

Delete a project that has no apps

```text
moor projects delete NAME
```

### `moor projects remove-member`

Take someone out of a project

```text
moor projects remove-member NAME EMAIL
```

### `moor projects set`

Change who may do what in a project without being a member

```text
moor projects set NAME
```

| Flag | Default | What it does |
|---|---|---|
| `--everyone string` |  | the role of those who are not members: viewer, deployer, maintainer or none |
| `--viewers-see-logs` | `true` | whether viewers may read logs |

### `moor projects show`

Show a project, its apps and its members

```text
moor projects show NAME
```

## `moor promote`

Release the build one environment runs to another, without building it again, with the variables of the environment it goes to. What was tried is what runs.

```text
moor promote --from ENVIRONMENT
```

```text
moor promote --from staging
moor promote --from feature-new-pricing --to staging --app shop
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--from string` |  | the environment whose build to release |
| `--to string` | `production` | the environment to release it to |

## `moor ps`

List an app's running instances

```text
moor ps
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--json` |  | print as JSON |

## `moor redeploy`

Build what the current release was built from again, and roll it out: the same commit, the same upload or the same image name. Use it when a toolchain has moved, an image's tag points elsewhere, or a build variable has changed.

A release that was uploaded ready to run has nothing to build.

```text
moor redeploy
```

```text
moor redeploy --app docs
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--detach` |  | queue the deployment and return |
| `--env string` | `production` | environment |

## `moor registry`

List the container registries the server has credentials for. It uses them to pull images and to build from private base images. Managing them takes an admin.

```text
moor registry
```

| Flag | Default | What it does |
|---|---|---|
| `--json` |  | print as JSON |

### `moor registry login`

Store a container registry's credentials on the server, encrypted with its master key. The password is asked for, or read from standard input. Nothing checks it now: the next pull does.

```text
moor registry login HOST
```

```text
moor registry login ghcr.io --username acme-deploy
moor registry login registry.example.com:5000 --username ci < password.txt
```

| Flag | Default | What it does |
|---|---|---|
| `--username string` |  | the user to sign in as |

### `moor registry logout`

Remove a container registry's credentials from the server

```text
moor registry logout HOST
```

## `moor releases`

List an app's releases

```text
moor releases
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--json` |  | print as JSON |

### `moor releases diff`

Show what changed from one release to another: whether the build did, the lines of the resolved manifest, and which variables. Of variables it shows the names only.

```text
moor releases diff vN vM
```

```text
moor releases diff v3 v4 --app docs
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--json` |  | print as JSON |

### `moor releases show`

Show a release and its resolved manifest

```text
moor releases show vN
```

```text
moor releases show v3 --app docs
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--json` |  | print as JSON |

## `moor restart`

Start new instances of the current release, move traffic to them once they are healthy, and stop the old ones after their requests finish. With a process, only its instances are replaced.

```text
moor restart [PROCESS]
```

```text
moor restart
moor restart worker
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |

## `moor rollback`

Publish an earlier release again, as a new release. Without a version, the release before the current one. No rebuild is needed: it takes seconds.

The new release runs with the current variables; --with-config restores the variables the earlier release ran with.

```text
moor rollback [vN]
```

```text
moor rollback --app docs
moor rollback v3 --app docs
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--with-config` |  | restore the variables the earlier release ran with |

## `moor run`

Run a command as a one-off instance of an environment's current release, with its variables and data directory, and show what it says. moor ends with the command's own exit status, and interrupting moor stops it. It takes an hour at the most, unless --timeout says otherwise, up to a day.

With -it, it has a terminal of its own that you are at, for a shell or anything else that asks you things. All the keys go to it, Ctrl-C too, and leaving stops it. What is said in one is not kept, since a password can be typed into it, and what it says is not kept from you either: its secrets are there to be seen. With -i and no -t, it has standard input, without a terminal.

```text
moor run [flags] -- COMMAND [ARG...]
```

```text
moor run -- python manage.py migrate
moor run --env staging --timeout 10m -- ./seed.sh
moor run --detach -- ./rebuild-index
moor run -it -- /bin/sh
moor run -i -- ./import.sh < data.csv
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--detach` |  | start it and return |
| `--env string` | `production` | environment |
| `-i, --interactive` |  | send what is typed, or standard input, to it |
| `--timeout string` |  | how long it may take, such as 10m (default 1h) |
| `-t, --tty` |  | give it a terminal |

## `moor scale`

Set how many instances of each named process run. New ones start beside those running; those beyond the number finish their requests and stop. If a new instance cannot start, the app goes back to how it was.

The setting stays, over the manifest's instances, for the releases to come. Without arguments, it shows how many run.

```text
moor scale [PROCESS=N ...]
```

```text
moor scale --app shop
moor scale web=3 worker=1 --app shop
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |

## `moor server`

Set up and run a Moorage server

### `moor server backup`

Make a backup of this server, which can go on running: its database, repositories, builds, apps' data and certificates. It goes where --to says, or where the server has been told: a path, or s3://bucket/prefix, with the service's details in AWS\_ACCESS\_KEY\_ID, AWS\_SECRET\_ACCESS\_KEY, AWS\_REGION and AWS\_ENDPOINT\_URL.

It leaves out the master key, without which its secrets cannot be read, so that a backup that goes astray gives them away to nobody. Keep that key safe, and apart. --include-keys puts it and the server's other keys in the backup, which then has to be kept as safe as they are: only to a path.

```text
moor server backup
```

```text
moor server backup
moor server backup --to /mnt/backups
moor server backup --to s3://acme-backups/moorage
```

| Flag | Default | What it does |
|---|---|---|
| `--config string` |  | the server's configuration file |
| `--include-keys` |  | include the master key and the server's other keys |
| `--to string` |  | where it goes: a path, or s3://bucket/prefix |

#### `moor server backup list`

List the backups that are there

```text
moor server backup list
```

| Flag | Default | What it does |
|---|---|---|
| `--config string` |  | the server's configuration file |
| `--to string` |  | where to look: a path, or s3://bucket/prefix |

#### `moor server backup verify`

Read a backup through, and say whether it could be restored: the path of its file, or s3://bucket/prefix/name. Nothing changes.

```text
moor server backup verify SOURCE
```

```text
moor server backup verify /mnt/backups/moorage-20261104T120000Z.tar.gz
```

### `moor server config`

The server's settings, as a file to keep, to review, or to bring to another server: people, projects, registries, notifications, backups and sign-in. It has no secrets in it: where one belongs, it names a variable of the environment. server.toml is apart from this, and apps come from their manifests.

#### `moor server config export`

Write out the server's settings

```text
moor server config export
```

```text
moor server config export > moorage-config.toml
moor server config export --output moorage-config.toml
```

| Flag | Default | What it does |
|---|---|---|
| `-o, --output string` |  | write to this file, and not to the standard output |

#### `moor server config import`

Import makes the server match a file from moor server config export, in what the file mentions. It adds and changes, and removes nothing. A secret written as ${NAME} is read from this command's environment, and if that is not set, the server keeps the secret it has.

```text
moor server config import FILE
```

```text
moor server config import moorage-config.toml --dry-run
MOORAGE_SSO_CLIENT_SECRET=... moor server config import moorage-config.toml
```

| Flag | Default | What it does |
|---|---|---|
| `--dry-run` |  | only say what this would do |
| `--json` |  | print as JSON |

### `moor server doctor`

Check a server on this machine, whether it is running or not, and say how to fix what is wrong: its listeners, DNS, certificates, the container engine, disk space, the data directory, the clock, kernel features, and apps that can be reached without the router.

--public-url also checks the whole path from outside to the server, through any TLS proxy in front of it: the headers that arrive, an app's hostname, WebSockets, streaming and large uploads. --timeouts adds a request that stays silent for over a minute. The exit status is 1 if anything failed.

```text
moor server doctor
```

```text
moor server doctor
moor server doctor --public-url https://moorage.apps.example.com --timeouts
```

| Flag | Default | What it does |
|---|---|---|
| `--config string` |  | server.toml (default: $MOORAGE\_CONFIG or the per-platform location) |
| `--json` |  | print as JSON |
| `--public-url string` |  | the server's address from outside, such as https://moorage.apps.example.com |
| `--timeouts` |  | with --public-url, also check that a request may stay silent for over a minute |

### `moor server init`

Write server.toml, the bootstrap configuration of a Moorage server.

```text
Presets:
  lan     a server on your network: apps at https://<app>.<domain> with
          certificates from Moorage's own CA (ports 80, 443 and 2222), or plain
          HTTP on 127.0.0.1:8080 behind your TLS proxy with --behind-proxy
  local   this computer only: apps at http://<app>.localhost:8080
  public  a server on the internet: apps at https://<app>.<domain> with
          certificates from a public CA (ports 80, 443 and 2222), and everything
          but production behind a sign-in
```

The file is written to --config, $MOORAGE\_CONFIG, or a default location: system-wide when run as root (Administrator on Windows), per-user otherwise.

```text
moor server init
```

```text
moor server init --preset local
sudo moor server init --domain apps.example.com
sudo moor server init --domain apps.example.com --behind-proxy
sudo moor server init --preset public --domain apps.example.com --email ops@example.com --agree-terms
```

| Flag | Default | What it does |
|---|---|---|
| `--acme-directory string` |  | another ACME server's directory than the default CA's (public) |
| `--agree-terms` |  | you have read and accept the CA's terms, which it asks for (public) |
| `--behind-proxy` |  | serve plain HTTP on 127.0.0.1:8080 behind a TLS-terminating proxy such as nginx (lan) |
| `--config string` |  | where to write server.toml (default: $MOORAGE\_CONFIG or a per-platform location) |
| `--console-host string` |  | the console's hostname (default: moorage.\<domain\>) |
| `--data-dir string` |  | data directory (default: $MOORAGE\_DATA\_DIR or a per-platform location) |
| `--domain string` |  | base domain; apps are served at \<app\>.\<domain\> (required for lan) |
| `--email string` |  | whom the CA tells of the account (public) |
| `--force` |  | replace an existing server.toml |
| `--preset string` | `lan` | starting point: lan or local or public |
| `--routing string` | `host` | how apps are told apart: host, as \<app\>.\<domain\>, or path, as \<domain\>/\<app\>, for where there can be no wildcard DNS record |

### `moor server install`

Install moor server run as a service — a systemd unit on Linux, a launchd job on macOS, a Windows service — that starts at boot (as root or an administrator) or at login (otherwise, on Linux and macOS), and start it.

The service is set up so that stopping or restarting it leaves the apps running: the server adopts them when it starts again. It runs this moor binary from where it is now, with the current PATH.

```text
moor server install
```

```text
sudo moor server install
moor server install --config ~/moorage/server.toml
```

| Flag | Default | What it does |
|---|---|---|
| `--config string` |  | server.toml (default: $MOORAGE\_CONFIG or the per-platform location) |
| `--no-start` |  | install the service without starting it |

### `moor server proxy-config`

Print an nginx configuration for this server: its hostnames, the certificate, WebSockets, streaming, long requests and large uploads. Any other proxy must do the same; see the documentation. Check the result with moor server doctor --public-url.

```text
moor server proxy-config nginx
```

```text
moor server proxy-config nginx --cert /etc/nginx/tls/wildcard.crt --key /etc/nginx/tls/wildcard.key
moor server proxy-config nginx > /etc/nginx/conf.d/moorage.conf
```

| Flag | Default | What it does |
|---|---|---|
| `--cert string` |  | the proxy's certificate, for the console's and the apps' hostnames |
| `--config string` |  | server.toml (default: $MOORAGE\_CONFIG or the per-platform location) |
| `--http-port int` | `80` | the port nginx redirects to HTTPS from |
| `--https-port int` | `443` | the port nginx serves HTTPS on |
| `--key string` |  | its private key |

### `moor server resolver`

Show whether this machine resolves \<app\>.localhost, the addresses of a server on your own computer. Chromium- and Firefox-based browsers manage that on their own, as does systemd-resolved on Linux, and some versions of macOS. Where macOS does not, for Safari and command-line tools, moor server resolver install adds a rule for it, pointing at a small DNS responder of the server's, which answers only on loopback and only for that domain. On Windows, it adds a block to the hosts file, which has the names of the apps that are deployed now.

```text
moor server resolver
```

| Flag | Default | What it does |
|---|---|---|
| `--config string` |  | server.toml (default: $MOORAGE\_CONFIG or the per-platform location) |

#### `moor server resolver install`

Make this machine resolve \<app\>.localhost for every program. On macOS, that adds a rule to /etc/resolver, for which it uses sudo, and turns on the server's DNS responder (listen.dns in server.toml), which the server needs restarting to take up. On Windows, it adds a block to the hosts file, for which it takes an elevated terminal, with the names of what is deployed now: a server that may write to that file keeps it up to date, and otherwise, run this again after adding an app.

```text
moor server resolver install
```

| Flag | Default | What it does |
|---|---|---|
| `--config string` |  | server.toml (default: $MOORAGE\_CONFIG or the per-platform location) |

#### `moor server resolver remove`

Undo moor server resolver install

```text
moor server resolver remove
```

| Flag | Default | What it does |
|---|---|---|
| `--config string` |  | server.toml (default: $MOORAGE\_CONFIG or the per-platform location) |

### `moor server restart`

Restart the server's service (the apps keep running)

```text
moor server restart
```

| Flag | Default | What it does |
|---|---|---|
| `--config string` |  | server.toml (default: $MOORAGE\_CONFIG or the per-platform location) |

### `moor server restore`

Restore a backup: the path of its file, or s3://bucket/prefix/name. The server has to be stopped. It reads all of the backup first, so that a damaged one changes nothing, and then stops any apps still running, and moves what the data directory held aside rather than deleting it.

A backup without keys needs the master key it was made with to be in place first.

```text
moor server restore SOURCE
```

```text
moor server restore /mnt/backups/moorage-20261104T120000Z.tar.gz
moor server restore s3://acme-backups/moorage/moorage-20261104T120000Z.tar.gz --force
```

| Flag | Default | What it does |
|---|---|---|
| `--config string` |  | the server's configuration file |
| `--force` |  | move aside what the data directory holds, and restore over it |

### `moor server run`

Run the Moorage server in the foreground until interrupted.

The configuration is read from --config, $MOORAGE\_CONFIG, or the default location used by 'moor server init'.

```text
moor server run
```

| Flag | Default | What it does |
|---|---|---|
| `--config string` |  | server.toml to read (default: $MOORAGE\_CONFIG or a per-platform location) |
| `--dev` |  | read the console's templates and assets from the source tree, for working on them |
| `--log-format string` |  | text or json (overrides server.toml) |
| `--log-level string` |  | debug, info, warn or error (overrides server.toml) |

### `moor server sso`

Say how people sign in through an OpenID Connect provider, which is set up in the console, in Settings → Sign-in.

```text
moor server sso
```

#### `moor server sso off`

Turn off signing in through a provider, which gives passwords back: the way in if your provider has gone away. On the server's own machine this needs no sign-in, and those who signed in only through the provider need a new link for a password, from an administrator.

```text
moor server sso off
```

### `moor server start`

Start the server's service

```text
moor server start
```

| Flag | Default | What it does |
|---|---|---|
| `--config string` |  | server.toml (default: $MOORAGE\_CONFIG or the per-platform location) |

### `moor server status`

Show whether the server's service runs and answers

```text
moor server status
```

| Flag | Default | What it does |
|---|---|---|
| `--config string` |  | server.toml (default: $MOORAGE\_CONFIG or the per-platform location) |

### `moor server stop`

Stop the server: its service, or one started with moor server run. The apps keep running, and the server adopts them when it starts again; --all stops them too, whether or not a server is running.

```text
moor server stop
```

| Flag | Default | What it does |
|---|---|---|
| `--all` |  | stop the apps too |
| `--config string` |  | server.toml (default: $MOORAGE\_CONFIG or the per-platform location) |

### `moor server telemetry`

Send what apps say to syslog, an OpenTelemetry collector or any HTTP endpoint, and what they do to an OpenTelemetry collector. What is kept here stays, and a sink that is down costs nothing but its own place in the queue, of 20,000 lines: past that, the oldest go, and it says how many did.

```text
moor server telemetry
```

```text
moor server telemetry add central --kind syslog --address tls://logs.example.com:6514
moor server telemetry add collector --kind otlp --address https://otel.example.com:4318 --logs --metrics --header "Authorization: Bearer abc"
moor server telemetry test central
moor server telemetry rm central
```

| Flag | Default | What it does |
|---|---|---|
| `--json` |  | print JSON |

#### `moor server telemetry add`

Add somewhere to send what apps say and do, or change the one of that name. Logs go unless it is given --metrics alone.

An address is udp://host:514, tcp://host:601 or tls://host:6514 for syslog. For OTLP it is what comes before /v1/logs, as in https://otel.example.com:4318. For HTTP, it is where JSON batches are sent.

--header is sent with each request, sealed here and not shown again, and is left as it is if you leave it out, unless the address changes. To be sent in the clear across the internet is not for a header, so it takes an https address, or something on the local network.

```text
moor server telemetry add NAME
```

```text
moor server telemetry add central --kind syslog --address tls://logs.example.com:6514 --app shop
moor server telemetry add collector --kind otlp --address https://otel.example.com:4318 --metrics --logs
```

| Flag | Default | What it does |
|---|---|---|
| `--address string` |  | where it is |
| `--app string` |  | only this app (default: all of them) |
| `--header stringArray` |  | a header to send, as Name: value (more than one can be given) |
| `--kind string` |  | syslog, otlp or http |
| `--logs` |  | send what apps say (the default, unless --metrics is on its own) |
| `--metrics` |  | send what they do, to an OTLP collector |

#### `moor server telemetry rm`

Stop sending to a sink

```text
moor server telemetry rm NAME
```

#### `moor server telemetry test`

Send something to a sink now, and say how that went

```text
moor server telemetry test NAME
```

### `moor server uninstall`

Stop the server, remove its service, and stop the apps (unless --keep-apps). The configuration and the data directory are left in place: installing the service again brings the server, and its apps, back.

```text
moor server uninstall
```

| Flag | Default | What it does |
|---|---|---|
| `--config string` |  | server.toml (default: $MOORAGE\_CONFIG or the per-platform location) |
| `--keep-apps` |  | leave the apps running |

### `moor server upgrade`

Upgrade downloads a release, checks it against its published checksum, keeps a copy of the database and the binary, puts the new binary in place and restarts the server. Apps keep running. If the server does not come up on the new version, everything goes back by itself.

With --rollback it goes back later on. The database only goes back if the new version changed it, since what has happened since is then lost from it.

```text
moor server upgrade
```

```text
moor server upgrade --check
sudo moor server upgrade
sudo moor server upgrade --version 0.6.1
sudo moor server upgrade --from /mnt/releases/0.6.1
sudo moor server upgrade --rollback
```

| Flag | Default | What it does |
|---|---|---|
| `--check` |  | only say whether there is a newer version |
| `--config string` |  | server.toml (default: $MOORAGE\_CONFIG or the per-platform location) |
| `--force` |  | with --rollback, go back even if that loses what has happened since |
| `--from string` |  | a directory or address holding the release's files, for a mirror or a machine with no way out |
| `--rollback` |  | go back to the version before the last upgrade |
| `--version string` |  | the version to upgrade to (default: the newest) |
| `--wait duration` | `2m0s` | how long the new version has to come up before going back |

### `moor server users`

List the server's users. An admin manages users; until projects arrive, every user may do everything with apps.

```text
moor server users
```

| Flag | Default | What it does |
|---|---|---|
| `--json` |  | print as JSON |

#### `moor server users create`

Create a user, and print a link for them to choose a password

```text
moor server users create EMAIL
```

```text
moor server users create ada@example.com --name "Ada Lovelace"
moor server users create ops@example.com --admin
```

| Flag | Default | What it does |
|---|---|---|
| `--admin` |  | make them an admin |
| `--name string` |  | their name (default: the first part of the address) |

#### `moor server users disable`

Disable a user: their sessions end, and their tokens and keys stop working

```text
moor server users disable EMAIL
```

#### `moor server users enable`

Enable a disabled user again

```text
moor server users enable EMAIL
```

#### `moor server users invite`

Print a new one-time link for a user to choose a password: for one who has forgotten theirs, or whose first link has expired. It replaces any earlier link, and using it ends their sessions.

```text
moor server users invite EMAIL
```

#### `moor server users role`

Change a user's role

```text
moor server users role EMAIL admin|member
```

## `moor share`

Make a link that lets someone reach an environment, whatever its gate, without an account or a password. It is shown only now. It lasts seven days unless you say otherwise, up to a year, and can be revoked.

A link with --max-uses asks for a click, so that what fetches addresses to preview them does not use it up.

```text
moor share
```

```text
moor share --expires 3d --env staging
moor share --max-uses 1
moor share list
moor share revoke shr_01J…
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--expires string` |  | how long it lasts, such as 3d or 12h (default 7d) |
| `--max-uses int` |  | how many times it can be used (default: no limit) |

### `moor share list`

List an environment's share links

```text
moor share list
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |
| `--json` |  | print as JSON |

### `moor share revoke`

End a share link, and within a minute everyone it let in

```text
moor share revoke ID
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |

## `moor sleep`

Stop an environment's instances, except processes with sleep = false, until a request wakes it. A preview does this by itself after 30 minutes with no requests, and any environment can with sleep\_after in \[environments.NAME\].

```text
moor sleep
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |

## `moor tokens`

List your API tokens: for scripts and CI, and for git over HTTPS. Create one with 'moor tokens create NAME', revoke one with 'moor tokens revoke NAME'.

```text
moor tokens
```

| Flag | Default | What it does |
|---|---|---|
| `--json` |  | print as JSON |

### `moor tokens create`

Create an API token. Its scope is what it may do, up to what you may do:

```text
  read    see apps, deployments, releases and logs; fetch their repositories
  deploy  also deploy, restart, roll back, push, and see variables
  admin   also change variables, and manage your tokens and keys
  gate    only get through the sign-in gates of apps, as you
```

--app restricts it to one app, and --expires makes it stop working after a time, such as 90d.

```text
moor tokens create NAME
```

```text
moor tokens create ci --scope deploy --app shop --expires 90d
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | restrict it to one app |
| `--expires string` |  | stop working after this long, such as 90d |
| `--scope strings` | `[deploy]` | what it may do: read, deploy, admin, or gate to get through sign-in gates |

### `moor tokens revoke`

Revoke an API token

```text
moor tokens revoke NAME
```

## `moor trust`

Trust the certificate authority of a server on this machine, so that browsers and command-line tools accept the certificates it issues. The CA goes into the operating system's list and, on Linux, into the browsers' own lists.

A CA is powerful: whoever holds its key can pretend to be any site it may sign for. Moorage's own CA may sign only for the server's domain, and moor trust says what the one it finds may sign for. It comes from the server's own files on the server's machine, and otherwise from the server, over a connection nothing can vouch for yet: compare its fingerprint with what moor server status shows on the server. --fingerprint does that without asking.

On macOS and Windows, moor trust changes your own list, and asks you to confirm; as root or an Administrator, the machine's. On Linux it uses sudo.

```text
moor trust
```

```text
moor trust
moor trust --server https://moorage.apps.example.com
moor trust --server https://moorage.apps.example.com --fingerprint AB:CD:…
moor trust --file corporate-root.pem
moor trust --print > moorage-ca.crt
moor trust --remove
```

| Flag | Default | What it does |
|---|---|---|
| `--file string` |  | trust this PEM file, instead of asking a server |
| `--fingerprint string` |  | the SHA-256 fingerprint the CA must have; asks nothing if it does |
| `--print` |  | print the CA certificate, and change nothing |
| `--remove` |  | stop trusting the CA |

## `moor unlock`

Let an environment take deployments again.

```text
moor unlock
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |

## `moor version`

Print the version of moor

```text
moor version
```

| Flag | Default | What it does |
|---|---|---|
| `--json` |  | print as JSON |

## `moor wake`

Start an environment that is asleep, and return when it is ready.

```text
moor wake
```

| Flag | Default | What it does |
|---|---|---|
| `--app string` |  | app (default: $MOOR\_APP or \[app\].name in moorage.toml) |
| `--env string` | `production` | environment |

## `moor whoami`

Show who you are signed in as, and on which server

```text
moor whoami
```

| Flag | Default | What it does |
|---|---|---|
| `--json` |  | print as JSON |

