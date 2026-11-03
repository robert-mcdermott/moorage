# 9. Interfaces: CLI, REST API and Web Console

The CLI and any automation talk to the **REST API**. The web console is
rendered by the server from the **same service layer** the API handlers call,
and every console action has an API equivalent — a test enumerates console
actions and checks the mapping. Nothing is console-only or CLI-only (except
server bootstrap commands that run before the API exists).

## CLI (`moor`)

The binary has three modes: **client** commands (talk to a server's API),
**server** commands (`moor server …`: install, run and administer a server),
and the internal **shim** mode (hidden).

### Contexts and credentials

- Client configuration lives in the OS config directory
  (`~/.config/moorage/config.toml`, `~/Library/Application Support/moorage`,
  `%AppData%\moorage`) as named **contexts** (server URL + user). Tokens are
  stored in the OS keychain when available, otherwise in a `0600` file.
- On the server itself, `sudo moor …` talks to the local admin socket and
  needs no login.

### How the CLI picks the app and environment

- **App:** `--app` › `MOOR_APP` › the app behind the `moorage` git remote in
  the current repository › `[app].name` in `moorage.toml`.
- **Environment:** `--env <name>` › `production`. `--env .` means "the
  environment of the current git branch" (its preview or mapped environment).

### Command tree

```text
Getting started
  moor login [url]                    Sign in via browser approval; save a context
  moor logout | whoami | context [list|use|rm]
  moor init                           Detect the stack and write moorage.toml
  moor manifest validate|explain|schema

Apps
  moor apps [list]
  moor apps create <name> [--project p] [--git-remote]
  moor apps info|rename|delete
  moor status                         Environments, releases, instances, health
  moor open                           Open the environment URL in a browser

Deploying
  moor deploy [DIR] [--static | --artifact | --image ref | --ref git-ref] [--detach]
  moor deployments [list|show|follow|cancel|retry]
  moor builds [list|inspect <id>]     Inspect a (failed) build directory
  moor releases [list|show|diff]
  moor rollback [vN] [--with-config]
  moor redeploy [--no-cache]
  moor promote --from <env> --to <env>
  moor lock | unlock                  Pause/resume releasing new pushes to an environment
  moor git remote [--name moorage]    Add the git remote to this repository
  moor git credential                 Git credential helper (HTTPS pushes)

Running
  moor ps                             Instances: status, port, uptime, CPU, memory
  moor restart [process] | stop | start
  moor scale web=2 worker=1
  moor sleep | wake
  moor run [-it] -- <command>         One-off instance of the current release
  moor exec <instance> -- <command>   Into a running container
  moor cron [list|run <name>|history]
  moor maintenance on|off [--message "…"]

Configuration
  moor env [list]
  moor env set KEY=VALUE … [--secret] [--build] [--all-envs] [--production-only] [--no-restart]
  moor env unset KEY … | import .env | export
  moor domains [list|add|rm|verify]
  moor access [show|set public|password|login|share] [--allow-cidr …]
  moor share [--expires 7d] [--max-uses n] | share list | share revoke <id>
  moor previews [list|create <branch>|pin|unpin|destroy]

Observing
  moor logs [-f] [--process p] [--instance n] [--source …] [--since 1h] [--grep text]
  moor metrics [--series …]           Terminal sparklines
  moor events [-f]

Account
  moor keys [list|add|rm]
  moor tokens [list|create|revoke]
  moor trust                          Trust the server's internal CA on this machine

Server administration
  moor server init|run|install|uninstall|start|stop|status
  moor server doctor|upgrade|backup|restore|config|rotate-key
  moor server proxy-config nginx      Print a config for running behind a TLS-terminating nginx
  moor server users [list|create|disable|role]
  moor toolchains [list|install|prune|import]

  moor version | completion [bash|zsh|fish|powershell]
```

### Conventions

- Human-friendly tables and live progress by default; `--json` on every
  command for scripts; `NO_COLOR` and non-TTY output respected.
- Meaningful exit codes (deploy failed ≠ authentication failed ≠ not found).
- Destructive commands ask for confirmation (`--yes` to skip).
- The CLI checks API compatibility at login and warns when the server is
  older or newer than it supports.

## REST API

### Conventions

| Topic | Rule |
|---|---|
| Base path | `/api/v1` on the console hostname |
| Contract | OpenAPI 3.1 document checked into the repo and served at `/api/v1/openapi.json`. Server request/response types are generated from it; CI fails on drift. |
| Auth | Session cookie + CSRF token (console) or `Authorization: Bearer moor_…` |
| Addressing | Resources addressed by name in paths (`/apps/acme-portal/envs/production`); responses also carry typed IDs (`app_…`, `env_…`, `dep_…`) which are accepted anywhere a name is. |
| Errors | RFC 9457 `application/problem+json` with a stable `code` and per-field details |
| Pagination | Cursor-based: `?limit=&cursor=` → `next_cursor` |
| Idempotency | `Idempotency-Key` header honoured on creates and deploys |
| Long operations | `202 Accepted` + the deployment/job resource; follow via SSE |
| Streaming | Server-sent events for logs, deployment progress and the event feed (resumable with `Last-Event-ID`); WebSocket for interactive PTYs |
| Versioning | Additive changes within `v1`; breaking changes mean `v2` |

### Main resources

```text
POST   /auth/login | /auth/logout | /auth/device (CLI device approval)
GET    /me                                     · sessions · tokens · ssh-keys

GET    /projects            POST /projects     · members
GET    /apps                POST /apps
GET    /apps/{app}          PATCH · DELETE
GET    /apps/{app}/envs     POST (named env)
GET    /apps/{app}/envs/{env}                  PATCH (formation, limits, sleep) · DELETE

POST   /apps/{app}/envs/{env}/deployments      (multipart archive | git ref | image | static)
GET    /deployments/{id}    POST …/cancel · …/retry
GET    /deployments/{id}/log                   (SSE when Accept: text/event-stream)
GET    /apps/{app}/envs/{env}/releases         · /releases/{v} · /releases/{v}/diff/{w}
POST   /apps/{app}/envs/{env}/rollback | /promote | /restart | /stop | /start | /sleep | /wake

GET    /apps/{app}/envs/{env}/instances
GET    /apps/{app}/envs/{env}/vars             PUT (bulk) · PATCH · DELETE /vars/{name}
GET    /apps/{app}/envs/{env}/domains          POST · DELETE · POST …/verify
GET    /apps/{app}/envs/{env}/access           PUT
GET    /apps/{app}/envs/{env}/share-links      POST · DELETE
GET    /apps/{app}/envs/{env}/cron             POST …/{job}/run · GET …/{job}/runs
POST   /apps/{app}/envs/{env}/runs             (one-off) · GET /runs/{id} · WS /runs/{id}/attach

GET    /apps/{app}/envs/{env}/logs             (range query; SSE follow)
GET    /metrics                                (series query)
GET    /events                                 (SSE feed, filtered by permission)

POST   /hooks/{app}/{hook-id}                  (incoming deploy webhooks, HMAC-verified)

GET    /server/status · /server/settings (PATCH) · /server/doctor
GET    /users · /toolchains · /audit · /notifications/channels · /notifications/rules
```

Git endpoints (`/git/<app>.git/…`) and platform endpoints under `/.moorage/`
are outside the versioned API.

## Web console

### Technology

- **Server-rendered HTML** from Go `html/template`, hand-written CSS and
  small plain-JavaScript modules. No framework, no bundler, no Node.js
  toolchain: templates and assets are embedded in the binary and `go build`
  is the only build step.
- **Progressive enhancement:** every page works as plain HTML with standard
  forms (CSRF-protected, post/redirect/get). JavaScript adds the live parts:
  one SSE connection updates status badges and the activity feed; log and
  deployment views stream new lines; forms can submit without a full page
  reload; confirmations, copy buttons and the command palette are scripted.
- **Charts:** sparklines are rendered on the server as inline SVG; full
  charts are drawn by a small canvas renderer written for Moorage (time
  series, hover read-outs, deploy markers).
- **Log viewer:** keeps a bounded number of lines in the page and loads older
  lines on scroll, so long tails stay fast.
- **Styling:** CSS custom properties as design tokens; light and dark themes
  via `prefers-color-scheme` with a manual override; WCAG 2.1 AA contrast;
  full keyboard navigation; a command palette (`⌘K` / `Ctrl+K`) for jumping to
  any app, environment or action; responsive down to phone width.
- **Strict CSP** (`script-src 'self'`): no inline scripts; data for scripts is
  passed in `data-` attributes or JSON blocks.
- **Web terminal (Phase 3):** needs a terminal emulator in the browser — see
  open question Q11 in [Decisions](12-decisions.md#open-questions).

### Information architecture

```text
Apps (dashboard)
 └─ App
     ├─ Overview        environments · URLs · instances & health · key graphs · recent activity
     ├─ Deployments     history · live build log · resolved manifest · release diff · rollback/promote
     ├─ Logs            live tail · filters · search · download
     ├─ Metrics         charts with deploy markers · per-instance breakdown
     ├─ Variables       scope · source (manifest / server) · secret & overridden badges · bulk edit
     ├─ Domains & Access  hostnames · custom domains + DNS instructions · certificates · gate · share links
     ├─ Processes       formation · scaling · limits · isolation level · sleep · health checks
     ├─ Cron            jobs · next/last runs · run now · run logs
     ├─ Files           browse / download / upload in the data directory (maintainers, audited; Phase 3)
     ├─ Terminal        one-off shell in the release environment (Phase 3)
     └─ Settings        production branch · previews policy · git remote · webhooks · danger zone
Previews            every preview across apps: branch, author, URL, status, last activity, expiry
Activity            server-wide event feed
Settings (admin)    general · routing & TLS · git/SSH · runtime defaults · container engine ·
                    previews · toolchains · notifications · users & projects · tokens ·
                    audit log · backups · system health & doctor · about & upgrade
Account             profile · password & 2FA · SSH keys · tokens · sessions
```

### Key screens

**Dashboard.** A host resource strip (CPU, memory, disk) above a list of apps:
status dot, environment count, primary URL, last deploy (who, when, commit
message), and sparklines for requests, p95 latency, CPU and memory. Filter by
project and status; search; group previews under their app.

**App overview.** Header with name, status, URL and a **Deploy** button. An
environment switcher (production, named environments, previews). Each
environment card shows release, commit, uptime, instances per process with
health, and quick actions: open, restart, scale, sleep/wake, share.

**Deployment detail.** A stage timeline (prepare → toolchains → build →
release → rollout) with durations, the live log beneath it, and the resolved
manifest and diff against the previous release. Actions: cancel, retry,
roll back to, promote.

**New app flow.** Name → choose a source:

- *Push with git* — copyable commands (`moor git remote`, `git push moorage
  main`), then a live "waiting for your first push…" state that jumps to the
  deployment log the moment a push arrives;
- *Upload* — drag a folder (static) or an archive;
- *Image* — an image reference;
- *Connect repository* (Phase 3).

**First-run wizard.** One-time setup URL → admin account → base domain and
routing mode with a live DNS check → TLS mode → "deploy your first app" with
commands to copy.

Every empty state shows the next command to run.
