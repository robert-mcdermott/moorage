# The web console

The console is at the server's console hostname: `https://moorage.apps.example.com`,
or `http://moorage.localhost:8080` on your own computer. Sign in with your account;
`moor server status` shows the link that creates the first one.

Everything in it is also available from `moor` and [the API](api.md). It is growing:
this version has the apps, their deployments, the activity timeline and getting
started. Logs and variables come next, on the way to 0.3.0.

## Getting started

Claiming a server leads here, and **Getting started** in the sidebar comes back. It
says how the server looks (its DNS and certificates, for an admin), what to install
and how to sign in from your own computer, and how to deploy. It watches for your
first deployment, and says when it is live.

## Apps

Every app: how it stands, its address, its release and its last deployment, changing as
they do. **New app** asks for a name, and says how to deploy it. Filter the list by
typing part of a name.

| Status | |
|---|---|
| **Healthy** | Every instance is healthy |
| **1 of 2 healthy** | Some are not, and those that are get the traffic |
| **Down** | None is |
| **Deploying** | A deployment is under way |
| **Live** | A static site being served |
| **Scaled to zero** | It was asked to run no instances |
| **Deploy failed** | Its first deployment failed, so that nothing is served |
| **Not deployed** | It has never had a release |

A badge beside the status says if the latest deployment failed, while the release before
it keeps serving.

## An app

Its page says what it serves and who deployed it, lists its instances with their health,
restarts and last exit, and shows its latest deployments and activity. Instances and
deployments change as they do. From here:

| | | |
|---|---|---|
| **Deploy** | Says how: from its directory, with `git push`, or from the page, by uploading a `.tar.gz` of its source or naming an image. It goes to the deployment when one starts. | `moor deploy` |
| **Redeploy** | Builds again what the current release was built from: the same commit, upload or image name, for when a toolchain has moved, a tag points elsewhere or a build variable has changed. A release uploaded ready to run has nothing to build. | `moor redeploy` |
| **Restart** | Replaces the instances, without downtime. | `moor restart` |
| **Roll back** | Serves the release before this one again, as a new release, in seconds. A deployment's page rolls back to that one, and can restore the variables it ran with. | `moor rollback` |
| **Scale** | Says how many instances of each process run. | `moor scale` |

### Scaling

New instances start beside those running, and take traffic once they are healthy. Those
beyond the number finish their requests and stop. If a new instance cannot start, the app
goes back to how it was, and says why. The number stays, over the manifest's `instances`,
through restarts and the releases to come, and the page says what the manifest would have
had; `0` turns a process off.

```bash
moor scale web=3 worker=1
```

## A deployment

Its page shows how it is going, as it goes:

- **Stages:** prepare, toolchains, build, release and rollout, with how long each took,
  and those it did not need. A rollback needs no more than a rollout.
- **The log,** as it is written, with what went wrong if it failed.
- **What changed** since the release before it: whether the build did, the lines of the
  resolved manifest, and which variables were added, changed or removed. Of variables it
  shows the names, never their values.
- **The resolved manifest:** what `moor manifest explain` says, for this build.

```bash
moor releases show v3
```

```bash
moor releases diff v3 v4
```

## Activity

What happened, newest first, as it happens:

| | |
|---|---|
| **Deployments** | Queued, live, failed, superseded or canceled, and who by |
| **Releases** | A rollback, or a release for a change of variables |
| **Variables** | That they changed, and their names: never their values |
| **Running** | That it was restarted or scaled, and who by |
| **Instances** | That one exited and restarted, sickened, recovered or was replaced |
| **The server** | That it started, and which version |

Choose an app to see only its own. Events are kept for 90 days. From the command line:

```bash
moor events --app shop -n 20
```

```bash
moor events -f
```

The API has them too: `GET /api/v1/events`, and `GET /api/v1/events/stream` to follow
them as server-sent events.

## Jumping around

Press `⌘K` (`Ctrl+K` elsewhere), or `/`, to find an app, a page or an action by typing
a few of its letters. Arrow keys choose, and `Enter` goes.

## Themes

The console follows your system's light or dark setting. **Light** and **Dark** at the
foot of the sidebar override it, in this browser.

## Without scripts

Every page works as plain HTML, with forms and links; a deployment under way reloads
itself every few seconds. Scripts add what happens by itself: live updates, jumping
around, copy buttons and the choice of theme. Pages run under a strict
Content-Security-Policy, with no inline scripts or styles.

## Live updates

A page follows the server over one connection, and a deployment's log over one more, and
a page out of sight lets go of them, and catches up when it returns. Behind [a proxy](proxy.md), the proxy must not buffer
responses; `moor server doctor --public-url` checks that.

## Working on the console

From the top of a checkout, this reads the console's templates and assets from
`internal/console` for each request, so that a change shows when you reload:

```bash
moor server run --dev
```
