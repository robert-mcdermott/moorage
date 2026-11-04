# The web console

The console is at the server's console hostname: `https://moorage.apps.example.com`,
or `http://moorage.localhost:8080` on your own computer. Sign in with your account;
`moor server status` shows the link that creates the first one.

Everything in it is also available from `moor` and [the API](api.md). It is growing:
this version has the apps, the activity timeline and getting started. Deployments,
logs and variables come next, on the way to 0.3.0.

## Getting started

Claiming a server leads here, and **Getting started** in the sidebar comes back. It
says how the server looks (its DNS and certificates, for an admin), what to install
and how to sign in from your own computer, and how to deploy. It watches for your
first deployment, and says when it is live.

## Apps

Every app, with its address, its release and when it last changed. Releases change
here as they happen. Nothing is deployed yet? It says what to run.

## Activity

What happened, newest first, as it happens:

| | |
|---|---|
| **Deployments** | Queued, live, failed, superseded or canceled, and who by |
| **Releases** | A rollback, or a release for a change of variables |
| **Variables** | That they changed, and their names: never their values |
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

Every page works as plain HTML, with forms and links. Scripts add what happens by
itself: live updates, jumping around, copy buttons and the choice of theme. Pages run
under a strict Content-Security-Policy, with no inline scripts or styles.

## Live updates

A page follows the server over one connection, and a page out of sight lets go of it,
and catches up when it returns. Behind [a proxy](proxy.md), the proxy must not buffer
responses; `moor server doctor --public-url` checks that.

## Working on the console

From the top of a checkout, this reads the console's templates and assets from
`internal/console` for each request, so that a change shows when you reload:

```bash
moor server run --dev
```
