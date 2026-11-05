# Security

Moorage is meant for a team that trusts one another, on a network it controls, showing work to people outside it. This page
says what it protects, how, and what it does not. The [design document](../spec/08-security.md) has more depth.

> **Where this stands.** Moorage is young, and has not had an outside security review. It has had close reviews of its own, the latest of what
> faces the internet and of what a low-privilege account can reach, and tests for what is described here, but no one has been paid to break it. A
> penetration test is on the [roadmap](../spec/11-roadmap.md). Until then, and whatever the `public` preset makes easy, keep it on a network you control, or
> behind something that decides who reaches it.

## What it trusts, and what it does not

| Who | Trust | |
|---|---|---|
| An administrator | Complete | Runs the server, its users and settings |
| A maintainer or deployer | High | **Deploying is running code on the server.** Give it as you would give someone a shell |
| A viewer | Some | Sees status and metrics, and usually logs |
| Someone visiting an app | None | Anyone who can reach the server |
| An app and its dependencies | Some | May have bugs, or a compromised dependency |

**Moorage does not contain someone who sets out to attack it from inside.** A hostile deployer on a server with several teams
is beyond what it is for: that takes separate machines. It is not a hosting service for strangers' code.

## Keeping apps apart

What apps can do to one another depends on the system, and the [level you choose](isolation.md):

| | Linux, as root | macOS | Windows |
|---|---|---|---|
| A user of its own for each app | With `user` or `sandbox` | No | No |
| Limited to its own files | With `sandbox` | No | No |
| Limits on memory and CPU | Yes | Memory, softly | Yes |

**Without a user of its own, apps can read one another's data and variables, and the server's database and master key,** since
they run as the account that holds them. The console says so on each app's page, and on the System page. For a shared server, use
Linux, as root, with `user` or `sandbox`.

[Containers](containers.md) are another way to keep an app to itself: they run with `no-new-privileges`, with no host network, and
rootless where the engine is.

In every case, apps are told to listen on loopback, with `HOST=127.0.0.1`, so that they are reached through the server, and
`moor server doctor` warns of one that can be reached another way. Other programs on the same machine can still reach them.

## Signing in

- **Passwords** are hashed with Argon2id, are at least 12 characters, and are checked against a list of common ones.
- **Two-factor sign-in** uses a time-based code from an app, with single-use codes and recovery codes.
- **Single sign-on** uses OpenID Connect, with group-to-role mapping, and can replace passwords. See
  [How people sign in](sign-in.md).
- **Claiming a server** takes a one-time code, so that nobody can race you to a new server on your network.
- **Sessions** are random, stored hashed, and expire. You can see and end yours.
- **Tries are limited:** on sign-in, by address and by account; on gate passwords, by address and environment; and on share links and
  SSH, by address. Failed sign-ins are logged, over the web and over SSH.

API tokens start with `moor_`, so that scanners can find a leaked one. They are stored hashed, have scopes, and can be limited to one
app and made to expire. They are never accepted in a query string. See [Accounts and access](accounts.md#api-tokens).

## Secrets

- A secret is encrypted at rest with AES-256-GCM, under a key wrapped by the master key, and bound to its app, environment and
  name, so that a value cannot be moved to another variable.
- It is shown once, when set. A maintainer can reveal it, and that is recorded.
- It reaches an app in memory, and is on no command line or disk. A container's engine does keep it in the container's
  configuration, where those who can use the engine can read it.
- Its value is taken out of logs as they are captured, as far as it can be found there.
- Builds do not get secrets, unless you say so for each one.
- **A backup leaves out the master key.** Keep that safe and apart: with both, anyone can read the secrets, and without it, nobody can.

## The console and API

- The console has its own hostname, and its cookie goes nowhere else, so that an app cannot read it.
- Pages run under a strict Content-Security-Policy, with no inline scripts or styles, and cannot be framed.
- Requests that change something from a browser need a token bound to the session, and a same-origin request.
- A permission is declared for each route of the API, in one table, and a test goes through all of them for each kind of caller.
- A project can hide its apps from those with no role in it, who get the answer that an app that is not there gets. [Accounts](accounts.md#projects-and-roles) says where that stops.
- A token limited to one app reaches that app, and nothing of its project or of the others.
- `moor server doctor` and the System page warn of plain HTTP with no proxy in front, as credentials would travel unencrypted.

## Facing the internet

- Plain HTTP is redirected to HTTPS, and [`hsts`](tls.md#staying-on-https), which the `public` preset turns on, tells browsers not to try it.
- A static site does not serve files or directories whose names start with a dot, except `.well-known`, unless it says to. So a `.env` or a `.git` left in the directory stays there.
- A request cannot pass itself off as the platform. Every spelling of `X-Moorage-*` is removed, as some ways of serving apps read `X.Moorage.User` and `X_Moorage_User` as the same header.

## What the server fetches

Webhooks, mail servers, telemetry sinks and forges are named by people who may not run the server, so it refuses this machine, link-local
addresses and clouds' metadata services for them, unless an administrator allows them in
[`[notifications] allow`](configuration.md#notifications). It checks as it connects, so that a name cannot resolve to one thing and
then another, and follows no redirects. The address of an S3 service for backups and of a sign-in provider are set by administrators,
and are not limited in this way.

## What it builds

- Toolchains come over HTTPS, and are checked against their publishers' checksums, and signatures where there are any, before they are
  used.
- Builds run with the same isolation as apps, under a time limit, and without runtime secrets.
- Repository hooks are never run. The SSH server takes only git's two commands, and gives no shell.
- Code from someone who cannot push to a [connected repository](forges.md#code-from-strangers), such as a pull request from a fork,
  is not built until someone approves that commit.

## Its own releases

Each [release](https://github.com/robert-mcdermott/moorage/releases) has checksums, a list of what went into it, and an attestation of the
workflow that built it, which you can check with `gh attestation verify`. The installer and `moor server upgrade` check what they
download against the checksums, over HTTPS, before running anything. That catches a damaged download or a change on the way, and would not
catch someone who could change the release itself: see [What a release can be checked against](install.md#what-a-release-can-be-checked-against).

It is built with no cgo, with few dependencies, and checked with `govulncheck` in CI.

## On record

Every change, sign-in, secret revealed and git push is in the [audit log](accounts.md#who-did-what) for a year, with who and how it went, and
where from for all but pushes. Nothing of a secret's value goes in it.

## A checklist

[Running a server](administration.md#a-checklist-for-a-shared-server) has one.

## Reporting a problem

Please do not put the details of a security problem in a public issue. Open an
[issue](https://github.com/robert-mcdermott/moorage/issues) that says only that you have one to report, and how to reach you, and you will
get a private way to send it.
