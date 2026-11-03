# 1. Vision, Goals and Principles

## What Moorage is

Moorage is a self-hosted application deployment platform that ships as **one
binary** (`moor`). Install it on a server (or your laptop), point a wildcard
DNS record at it, and every developer on the team can push code and get a
running, shareable URL in seconds — with a web console that shows what is
running, whether it is healthy, what it is logging and how it is performing.

Unlike container-only platforms, Moorage runs applications **natively** (a Go
binary, a uv-managed Python app, a Node server, a static site) *or* in
containers (Podman or Docker). A small manifest in the repository —
`moorage.toml` — tells Moorage how to build and run the app; for common
project layouts the manifest is optional because Moorage detects the stack.

Its sweet spot is the space *before* production: demo environments for
customers, per-branch previews for reviewers, internal tools and dashboards,
internal documentation sites, and long-lived dev/staging services. Think of it
as an internal "pages" service that can also run real applications.

## Who it is for

| Persona | What they want |
|---|---|
| **App developer** | `git push` a branch and get a URL to share in under a minute. See build output and logs without SSH-ing into a server. |
| **Platform owner / team lead** | Install in five minutes on a single VM, wire up DNS and TLS once, add teammates, set resource guardrails, and stop thinking about it. |
| **Reviewer / customer / stakeholder** | Click a link. Maybe type a password or sign in. Nothing else. |
| **CI pipeline** | Deploy a built artifact, image or directory with a scoped token and get a machine-readable result. |
| **Solo developer on a laptop** | Run many local projects at once at `https://<name>.localhost` without juggling ports. |

## Primary use cases

1. **Push-to-preview.** Every branch pushed to Moorage becomes its own
   environment with its own URL (`app--feature-x.apps.example.com`), its own
   logs and its own lifetime. It sleeps when idle and is cleaned up when the
   branch is deleted or its TTL expires.
2. **Internal pages.** Docs sites, reports, generated dashboards and SPAs are
   deployed by `moor deploy --static ./site`, by dragging a folder into the
   console, or by pushing a repo. Static sites cost no processes at all.
3. **Real applications for trials.** An API, a web frontend, a background
   worker and a scheduled job, deployed together with zero-downtime rollouts,
   health checks, rollback and persistent data.
4. **Customer sandboxes.** Time-limited, password-protected or share-link
   protected environments that can be handed to someone outside the company.
5. **Laptop mode.** The same binary manages local projects with `*.localhost`
   hostnames — no TLS or DNS setup required.

## Goals

- **G1 — Single binary, zero required dependencies.** The server, CLI, git
  server, reverse proxy, TLS automation, toolchain manager, process supervisor
  and web console are all in `moor`. A container engine is *optional*.
- **G2 — Any app, not just containers.** Native processes are first-class.
  Containers are one runtime among several, not the foundation.
- **G3 — Five-minute install, one-minute deploy.** From a fresh VM to a
  deployed app in about five minutes; from `git push` to a live URL in under a
  minute for typical apps on a warm cache.
- **G4 — Great web console.** Live status, health, logs, deployment history,
  metrics graphs, configuration — everything a developer needs without a
  terminal. Live updates, no page refreshes.
- **G5 — Zero-downtime by default.** New releases are started, health-checked
  and only then receive traffic. Rollback is instant because releases are kept.
- **G6 — Cross-platform.** Linux is the primary server target. macOS and
  Windows are supported for laptop mode and small teams, with documented
  differences in isolation and resource controls.
- **G7 — Boring, inspectable state.** One SQLite database plus plain files on
  disk. You can `ls` a release, `tail` a log file and `ps` an app process.
- **G8 — Secure enough for an internal platform, honest about the rest.**
  Strong authentication, encrypted secrets, access gates for apps, and a clear
  statement of the isolation level each runtime provides.

## Non-goals (for now)

- **Clustering and orchestration.** No multi-node scheduling, no cluster
  managers, no HA control plane. One node, done well. (The architecture leaves
  a seam for remote agents later — see the roadmap.)
- **Hostile multi-tenancy.** Moorage assumes the people deploying code are
  trusted colleagues. It isolates apps from each other where the OS makes that
  cheap, but it is not a public hosting service for untrusted code.
- **A CI system.** Builds are intentionally simple (one build command per app,
  run in a prepared toolchain environment). Test matrices, pipelines and
  artifact promotion workflows belong in your CI; Moorage accepts their output.
- **Production-scale autoscaling, multi-region, CDNs.** Out of scope.
- **Replacing a database operations team.** Managed services (Postgres, Redis,
  …) are planned for convenience in dev/preview environments, not as
  production-grade database hosting.

## Design principles

1. **Convention over configuration, with an escape hatch at every level.**
   Zero-config detection for common stacks; a manifest for anything else; raw
   `command` runtimes and containers when even that is not enough.
2. **Progressive disclosure.** The first screen and the first command are
   simple. Advanced settings exist but stay out of the way until needed.
3. **Every environment gets a URL immediately.** Routing, TLS and access
   control are automatic. No "now configure your proxy" step.
4. **API-first.** The web console and the CLI are both clients of the same
   public, versioned REST API. Anything the UI can do, a script can do.
5. **Desired state + reconciliation.** The database records what *should* be
   running; a reconciler continuously converges reality toward it. Crashes,
   reboots and daemon upgrades heal themselves.
6. **Stream everything.** Builds, logs, deploy progress and status changes are
   streamed live to the CLI, to `git push` output and to the console.
7. **Pure Go, no cgo.** One `go build` produces a static binary for every
   supported OS/architecture. Dependencies are few, vetted and pure Go.
8. **Fail loudly and helpfully.** When detection fails or a manifest is wrong,
   the error says what was tried and shows the line to add.

## Experience targets

| Metric | Target |
|---|---|
| Install → first deployed app | ≤ 5 minutes |
| `git push` → live (small Go/Python/Node app, warm caches) | ≤ 30 seconds |
| Rollback | ≤ 3 seconds (no rebuild) |
| Static site deploy | ≤ 5 seconds for a few thousand files |
| Daemon idle memory | ≤ 60 MB RSS |
| Capacity on a 4 vCPU / 8 GB VM | ~100 environments with idle sleep enabled |
| Wake a sleeping app on first request | ≤ 3 seconds for typical apps |

## Glossary

| Term | Meaning |
|---|---|
| **App** | A deployable unit, usually one source repository. Has a name that becomes part of its URL. |
| **Environment** | A running copy of an app with its own URL, configuration, data and history. Every app has `production`; it may also have long-lived named environments (`staging`) and ephemeral **previews** (one per branch). |
| **Deployment** | One attempt to build and roll out a source revision to an environment. Succeeds (creating a release) or fails. |
| **Release** | The immutable result of a successful build plus its resolved configuration. Numbered `v1`, `v2`, … per environment. Rollback targets a release. |
| **Process type** | A named command declared in the manifest (`web`, `worker`, …). |
| **Instance** | One running copy of a process type. `web` may have several instances. |
| **Route** | A hostname (and optional path prefix) mapped to an environment. |
| **Runtime** | How an app is built and started: `static`, `go`, `python`, `node`, `bun`, `deno`, `binary`, `command`, `dockerfile`, `image`. |
| **Driver** | How instances are executed: `native` (a supervised OS process) or `container` (via Podman/Docker). |
| **Toolchain** | A pinned language runtime/tool version (Go 1.26, Node 22, Python 3.13, uv …) that Moorage downloads, verifies and caches. |
| **Shim** | A tiny supervisor process (`moor shim`, the same binary) that owns one running instance, captures its output and keeps it alive across daemon restarts. |
| **Gate** | An access control policy in front of an environment: public, password, sign-in, share link, IP allowlist. |
