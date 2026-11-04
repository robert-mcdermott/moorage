# Moorage documentation

How to install Moorage, run it, and deploy apps to it. This documentation
describes Moorage 0.4.0; the [design documents](../spec/README.md) describe
how it works inside and where it is going.

## Start here

- [Installing Moorage](install.md): get `moor`, configure the server, run it
  as a service, back it up.
- [Accounts and access](accounts.md): claiming a server, signing in, users,
  API tokens and SSH keys.
- [TLS and trusting the server](tls.md): the certificates, and `moor trust`.
- [Behind a TLS proxy](proxy.md): nginx or anything else in front of Moorage.
- [When something is wrong](troubleshooting.md): `moor server doctor`, stopping
  everything, and logs.
- [The web console](console.md): the apps, what happened, and getting started.
- The [examples](../examples/README.md): complete apps, ready to deploy.

## Deploying apps

- [With git push](git.md): the production branch deploys, and the push
  shows the build
- [Go](go.md)
- [Python](python.md), with uv
- [Node.js](node.md), with npm
- [Static sites](static.md)
- [In containers](containers.md): a `Containerfile`, or an image from a registry
- [Anything else](other.md): your own build, a script, an app that is
  already built
- [Variables and secrets](variables.md)
- [Previews and environments](previews.md): a place of its own for every branch,
  staging, promoting, locks and sleeping
- [Cron jobs and one-off commands](cron.md)
- [Who can reach an app](access.md): passwords, signing in, share links and lists
  of addresses
- [Your own domains, and paths](domains.md): hostnames of your own, several apps on one
  hostname, and servers with no wildcard DNS

## Reference

- [`moorage.toml`](manifest.md): every setting of the app manifest.
- [The API](api.md): authentication, lists, errors, and the OpenAPI document.
- The command line: every command has `--help`; the common ones are below.

| Command | What it does |
|---|---|
| `moor deploy [DIR]` | Builds and deploys the source in DIR; prints the app's URL |
| `moor deploy --image REF` | Runs an image, pinned to its digest |
| `moor git remote` | Adds the app's git remote: `git push moorage main` deploys |
| `moor keys add` | Registers your SSH key, so that you can push over SSH |
| `moor registry login HOST` | Keeps a private container registry's credentials on the server |
| `moor trust` | Trusts the server's CA on this machine |
| `moor server doctor` | Checks the server and what it depends on, and says how to fix what it finds |
| `moor apps` | Lists the apps; `create`, `info`, `move`, `rename` and `delete` one |
| `moor events` | Shows what happened on the server; `-f` follows it |
| `moor deployments` | Lists an app's deployments; `show` and `follow` one |
| `moor releases` | Lists an app's releases; `show` one, and `diff` two |
| `moor rollback [vN]` | Serves an earlier release again, in a moment |
| `moor environments` | Lists an app's environments; `create` and `delete` one |
| `moor previews` | Lists the previews of branches; `create`, `pin`, `unpin` and `destroy` one |
| `moor promote --from ENV` | Releases what one environment runs to another, without building it again |
| `moor lock`, `moor unlock` | Stops deployments to an environment, and lets them go on |
| `moor sleep`, `moor wake` | Stops an environment's instances until a request wakes it, and starts them |
| `moor access` | Shows who can reach an environment; `set` changes it |
| `moor share` | Makes a link that lets someone reach an environment; `list` and `revoke` them |
| `moor domains` | Lists an environment's own hostnames; `add`, `verify` and `remove` one |
| `moor redeploy` | Builds the current release's source again, and rolls it out |
| `moor ps` | Lists the app's instances: status, health, port, PID |
| `moor logs [-f]` | Shows the app's output, and with `--source` its builds, requests and events; `--grep`, `--since` and `--problems` narrow it down |
| `moor restart [PROCESS]` | Replaces the instances without downtime |
| `moor run -- COMMAND` | Runs a command in the current release, and ends with its exit status |
| `moor cron` | Lists the cron jobs; `run` one now, `runs` and `log` show how they went |
| `moor scale [PROCESS=N ...]` | Shows or changes how many instances of each process run |
| `moor env` | Lists and changes variables and secrets; `reveal` reads a secret |
| `moor manifest explain` | Shows how Moorage will build and run the app |
| `moor server status` | Shows the server and its service |
| `moor login [URL]` | Signs in to a server, approving it in a browser |
| `moor whoami` | Shows who you are signed in as |
| `moor password` | Changes your password |
| `moor projects` | Lists your projects; `create`, `show`, `set`, `add-member`, `remove-member` and `delete` them |
| `moor audit` | Shows who did what, for administrators |
| `moor tokens` | Lists your API tokens; `create` and `revoke` them |
| `moor server users` | Lists the users; `create`, `invite`, `role`, `disable` and `enable` them |

An app is chosen with `--app`, `$MOOR_APP`, `[app].name` in the
`moorage.toml` of the current directory, or the repository's `moorage` git
remote.

## How Moorage works, in brief

- **The server** is the `moor` binary, run as a service. It keeps its state
  in one SQLite database and one data directory, and answers every app's
  hostname (`<app>.<domain>`) itself.
- **A deploy** uploads the app's source, or pushes it to the app's git
  repository on the server. The server works out how to build it, installs
  the toolchains it needs (checked against their published checksums) and
  builds it in the directory it will run from.
- **A release** is a build with its variables. A new release takes over only
  once its instances answer their health checks; the old ones finish their
  requests first. `moor rollback` brings an earlier release back.
- **Each instance** runs under a small supervisor of its own, which restarts
  it when it fails and keeps it running while the server restarts.
