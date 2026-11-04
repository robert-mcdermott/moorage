# 7. Observability: Status, Logs, Metrics, Events

Developers should be able to answer *"is it up, what is it doing, and why did
it break?"* from the console in seconds, without SSH. Everything here is built
in and works without external monitoring — and can be exported to it.

## Status model

| Level | States |
|---|---|
| Instance | `starting`, `healthy`, `unhealthy`, `restarting`, `crashed`, `draining`, `stopped` |
| Environment | `deploying`, `healthy`, `degraded`, `unhealthy`, `crashed`, `sleeping`, `stopped` |
| Deployment | `queued`, `preparing`, `building`, `releasing`, `rolling_out`, `live`, `failed`, `canceled`, `superseded` |
| Server | `ok`, `warning` (e.g. disk > 90 %, certificate renewal failing), `error` |

Status changes are events (below), so the console updates live and
notifications can react to them.

## Logs

### Sources

| Source | Content |
|---|---|
| `app` | stdout/stderr of each instance |
| `build` | Output of a deployment's build and release command |
| `job` | Cron runs and one-off runs |
| `router` | Access log for the environment |
| `events` | Platform events for the environment (deploys, restarts, health) as log lines |
| `server` | Moorage's own structured log |

### Storage

- **Plain files**, one per instance, so they can be read with ordinary tools:
  `logs/apps/<app>/<env>/<process>.<index>.<instance>.log`. Each line is
  `<RFC 3339 timestamp> <O|E|M> <message>` (stdout, stderr, or a note from
  Moorage such as a start, an exit or a restart). Over-long lines are
  split with a continuation marker.
- Size-based rotation (default 10 MiB × 5 files per instance) plus age-based
  cleanup for stopped instances (default 14 days). Build logs are kept as long
  as their deployment record.
- **Structured logs:** lines that are JSON objects are recognised; the console
  renders their fields, colours by level, and filters by level and field.
- **Secret redaction:** values of secret variables (8+ characters) are
  redacted at capture time, so they never reach disk. This is a safety net,
  not a guarantee.

### Reading

- `moor logs [-f] [--process web] [--instance 2] [--source stderr,build,access]
  [--since 1h] [--until …] [--grep text] [--problems] [--json]`. The sources are
  `stdout`, `stderr` and `moorage` (an app's own: the default), `build`, `access` and
  `events`; `job` and `server` come with what they are the logs of.
- Console: live tail with pause/resume, instance and process chips, source
  toggles, level filter, text search, time-range picker, line wrapping,
  download.
- The daemon follows files by offset and merges multiple instances by
  timestamp. Searches seek by timestamp (lines are time-ordered per file) and
  scan the bounded range — fast enough for development volumes without a
  full-text index.

### Forwarding

Optional per-server or per-app forwarding to syslog (RFC 5424), an
OpenTelemetry (OTLP) logs endpoint, or a generic HTTP endpoint (JSON batches).

The files are read rather than the lines being passed along as they are written, as an app's output goes to its shim, which
outlives the server: so what apps say while the server is away is there to catch up on. A cursor of how far it has read is kept across
restarts, and what is older than that is skipped. Each sink has a queue of its own, of 20,000 lines, so that one that is down holds
up nothing else, and backs off to a minute. This is at most once for what was queued as the server stopped: the logs here are the
record. It goes through OTLP's JSON encoding, which needs no generated code.

## Metrics

### What is collected

| Metric | Source | Granularity |
|---|---|---|
| CPU %, memory (RSS / working set) | Process-tree sampling on Linux, macOS and Windows until per-app cgroups arrive with Linux isolation; the engine's statistics for containers; containers on macOS/Windows engines: the engine's stats API | Instance |
| Restarts, uptime, OOM kills | Supervisor, cgroup `memory.events`, engine events | Instance |
| Requests by status class, latency (p50/p95/p99), bytes in/out, in-flight, upstream errors | Ingress | Environment, route |
| Build and deploy duration, success rate | Orchestrator | App |
| Host CPU, load, memory, disk (data volume), network | Host sampler | Server |
| Certificate days to expiry | TLS manager | Domain |

Samples are taken every 10 seconds by default.

### Storage

High-volume data must not grind SQLite, so storage is tiered:

| Tier | Resolution | Where | Retention |
|---|---|---|---|
| Live | 10 s raw samples | In-memory ring buffers | 2 hours |
| Recent | 1-minute roll-ups (min/avg/max/last; merged histograms) | SQLite, one row per *(series, hour)* holding packed arrays | 14 days |
| History | 1-hour roll-ups | SQLite, one row per *(series, day)* | 1 year |

- Latency uses fixed log-scale histogram buckets, which can be merged across
  instances and time, so percentiles stay correct after roll-up.
- Roll-ups are written once a minute in a single transaction. A daemon restart
  loses at most the in-memory live window.
- The query API picks the resolution from the requested range:
  `GET /api/v1/metrics?series=http.latency&env=…&from=…&to=…`.

### Console graphs

The app overview shows sparklines (requests, latency p95, CPU, memory) per
environment. The Metrics tab has full charts with range selection (live, 1 h,
24 h, 7 d, 30 d), per-instance breakdowns, folded to processes past eight lines,
and deploy markers drawn on every chart so performance changes line up with
releases.

### Export

- A Prometheus-compatible `/metrics` endpoint on the console host, with a token
  that can read, labelled by app, environment, process and instance. It says only
  what the token's holder could see, and of the server itself only to administrators.
- OTLP metrics push, once a minute: what `/metrics` has, with counts cumulative from when the server started.
- *Planned (Phase 4):* app-provided metrics — an app declares a metrics path,
  Moorage scrapes it and shows custom charts.

## Events and activity

Every meaningful change is an event:

- deployments: queued, started, succeeded, failed, superseded, canceled;
- releases: created (deploy, config change, rollback, promote);
- instances: started, exited, restarted, crashed, OOM-killed; all of them replaced
  on request (`app.restarted`);
- health: environment status changes;
- environments: created, slept, woke, expired, destroyed;
- domains and certificates: verified, issued, renewed, failing;
- configuration: variables changed (names only, never values), scale, limits,
  gates;
- security: sign-ins, token creation, permission changes (also in the audit
  log).

Events are published on the bus (live console updates through one SSE
stream), stored for 90 days, and shown as an activity timeline per
environment, per app and server-wide. A transient kind, such as a deployment
starting, goes to those following now and is not kept. Security events arrive with
the audit log, whose readers are limited, rather than in this feed that every member
can read.

## Notifications

- **Channels:** generic webhook (JSON, HMAC-signed), chat webhooks (Slack,
  Microsoft Teams and Discord payload formats), email over SMTP, and in-console
  notifications.
- **Rules:** per server, project or app — which event types go to which
  channels. Nothing goes to a channel until a rule says so. In the console,
  with no rules at all: deploy failed, crash loop and failed cron jobs go to the
  project's maintainers and the server's administrators, disk-space warnings go
  to administrators, and the person who pushed is always told about their own
  failed deploy. Certificate failures join these once certificates can fail on
  their own: Moorage does not obtain them yet.
- Deduplication so a crash-looping app sends one message, not hundreds, plus a
  "recovered" message when it heals. Like messages are held back for 15 minutes,
  per kind, app, environment and cron job, and the next says how many were.
- A webhook carries `X-Moorage-Signature: t=<unix>,v1=<hex>`, an HMAC-SHA256 of
  the time, a dot and the body, with a secret made for that channel and shown once.
- Channels are sealed with the master key. Sending is tried three times, and
  does not outlive the server: this is not a queue, and the events are the record.
- **Threshold alerts:** simple per-environment rules on the collected metrics — error rate above X % for Y minutes, p95 latency above
  T, memory above a percentage of its limit, CPU above a percentage of one core — routed through the same channels, with one when it
  comes right. They are looked at each minute, and a minute with nothing to compare counts for neither, as an app that is not being
  asked anything has no error rate. How full the server's disk may get is `[notifications] disk_percent`.

## Server health and diagnostics

- **Console → System:** version, uptime, CPU/memory/disk, database size, job
  queue, certificate status, container engine status, installed toolchains,
  active warnings.
- **`moor server doctor`:** checks the server, whether it is running or not:
  wildcard DNS resolution, its listeners, TLS configuration, the container
  engine, the data directory and its keys, cgroup v2 and Landlock availability
  (Linux), clock synchronisation (Linux), free disk space, and apps reachable
  beyond loopback, and prints the fix for each problem found.
- Moorage logs structured JSON (`log/slog`) to `<data>/logs/server/` and to
  stderr (picked up by journald / launchd / the Windows event log).
