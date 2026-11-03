# 11. Roadmap

## How we work: waves, slices and releases

- A **wave** is one release: wave 1 ships 0.1.0, wave 2 ships 0.2.0, and so
  on.
- A wave is cut into **slices**: bite-sized units of related work, each
  tested and useful on its own. A slice lands as one commit (or a few) on the
  wave's branch, with "(wave N, slice M)" in the commit message.
- Each wave has its own branch, `waveN`, created from `main`.
- The **last slice prepares the release**: the version number, the version's
  CHANGELOG section, the documentation, and this roadmap (the wave's section
  marked released, with what was found along the way). It ends in a pull
  request from `waveN` to `main`.
- After the merge, tag `vX.Y.0` on `main` and push the tag. The release
  workflow checks that the tag, the version in the code and the CHANGELOG
  agree, runs the tests, builds the six binaries (Linux, macOS and Windows on
  amd64 and arm64) with their checksums, and drafts a GitHub release whose
  notes are the version's CHANGELOG section. Review the draft and publish it.
- Fixes to a released version go on a `release-X.Y.Z` branch and ship as a
  patch release the same way.
- What a slice discovers but does not solve is written under its wave ("Found
  by slice N"). Decided-but-deferred work goes to the parking lot.

The design documents mark later features with the coarse phases they were
designed in, *(Phase N)*. Phases map onto waves like this:

| Phase | Waves | Releases |
|---|---|---|
| 0 — Foundations and spikes | 1–2 (each spike runs at the start of the slice that needs it) | 0.1.0–0.2.0 |
| 1 — First deploy (MVP) | 1–3 | 0.1.0–0.3.0 |
| 2 — Team preview platform | 4–5 | 0.4.0–0.5.0 |
| 3 — Operations and hardening | 6–8 | 0.6.0–0.8.0 |
| 4 — Services and ecosystem | 9–10 | 0.9.0–0.10.0 |
| 5 — Beyond one node | Parking lot | — |

## 0.1.0: first deploy (released 2026-11-03)

One binary that runs a server, deploys a static site or a native app from a
directory, supervises it, and serves it on its own hostname.

### Wave 1

1. **Foundations.** The `moor` binary and its command tree (`version`,
   `server init`, `server run`); `server.toml` with the `local` and `lan`
   presets, strict validation and per-OS defaults; the data directory; SQLite
   with embedded, forward-only migrations and a snapshot before migrating;
   typed IDs; structured logging; the server lifecycle with
   `/.moorage/health`; CI on Linux, macOS and Windows with cross-compilation of
   all six targets with cgo disabled; the tag-driven release workflow; the
   CHANGELOG.
   - Done when: `go vet` and `go test` pass on all three operating systems in
     CI, all six targets build with `CGO_ENABLED=0`, and
     `moor server init --preset local` followed by `moor server run` answers on
     `/.moorage/health`.
   - Done (slice 1): `moor version`, `moor server init` (presets `local` and
     `lan`, `--behind-proxy`) and `moor server run`; strict `server.toml`
     parsing that names the line of every problem; system-wide or per-user
     default locations; the data directory with private subdirectories and a
     lock against a second server; SQLite in WAL mode with one writer and
     read-only readers, forward-only migrations, snapshots before migrating
     (three kept) and refusal of a newer schema; typed ULID identifiers;
     `/.moorage/health`; CI (format, vet, tests on three operating systems, the
     race detector on Linux, six cgo-free builds, `govulncheck`) and the release
     workflow. Verified locally on macOS (tests with the race detector, the six
     builds, `go vet` for Linux and Windows, an end-to-end run of the binary)
     and in CI on Linux, macOS and Windows.
   - Found by slice 1: the server log goes to stderr only for now (journald
     and launchd capture it); the rotating file log under `logs/server/` comes
     with the log manager in slice 3.
2. **Ingress and static sites.** The routing table with atomic swaps;
   hostname routing under the base domain; static serving with the `[static]`
   rules (SPA fallback, clean URLs, headers, redirects); platform pages; TLS
   modes `internal` (certificates issued on demand by an auto-generated root
   or a corporate intermediate), `files` and `off`, with the `[proxy]` section
   for running behind a TLS proxy; a deploy API guarded by a bootstrap admin
   token; `moor deploy --static`; instant static rollback. Runs spike S4.
   - Done when: a site deployed with `moor deploy --static` is served on its
     hostname, directly over TLS and behind nginx; deploys and rollbacks under
     load fail no requests.
   - Done (slice 2):
     - Apps, environments, builds and releases in the database; app names
       checked; an app is created by its first deploy, and only once the upload
       is valid.
     - The router serves `<app>.<domain>` from an immutable table swapped
       atomically on every deploy and rollback; `/.moorage/health` and
       `/.moorage/ca.crt` on every hostname; platform pages (no such app, not
       deployed yet, not found, a placeholder on the console's hostname).
     - The static server: clean URLs, the three trailing-slash modes, SPA
       fallback, 404 pages, ordered redirect and rewrite rules with `:name` and
       `*` placeholders that existing files shadow unless forced, header rules,
       hashed assets cached as immutable, pre-compressed Brotli and gzip
       variants, strong ETags from content hashes, ranges, directory listings,
       and Moorage's own MIME table.
     - Uploads: a gzip-compressed tar streamed while it is packed; extraction
       refuses absolute paths, `..`, links and duplicates, and limits files and
       size; builds are written aside and renamed into place, then made
       read-only; half-written builds are removed at startup.
     - TLS: `internal` (a root generated on first start, name-constrained to
       the base domain and the console's hostname, or an intermediate from a
       corporate CA; 30-day certificates for the console and app hostnames
       only, renewed a week before expiry), `files` (several pairs chosen by
       server name, reloaded when they change), and `off` behind a proxy; plain
       HTTP moves to HTTPS except on this machine.
     - The API (`/api/v1`): list apps, deploy a static site, list releases,
       roll back; problem+json errors; a bootstrap admin token in
       `keys/admin-token`, stored as a hash. The CLI (`moor deploy --static`,
       `apps`, `releases`, `rollback`) finds the server and token on its own
       machine, or takes `--server`, `--token` and `--ca-cert`.
     - Spike S4: 2,046 requests across 12 deploys and rollbacks under load,
       none failed. Verified by tests (TLS in both modes with an intermediate
       chain, a TLS-terminating reverse proxy in front of Moorage, the CLI
       against a running server), the race detector, the six builds and an
       end-to-end run of the binary on macOS.
   - Found by slice 2:
     - nginx itself was not available on the test machine; a reverse proxy in
       the tests stands in for it. `moor server proxy-config nginx` (wave 2,
       slice 4) should be checked against a real nginx.
     - Forwarded headers and the PROXY protocol matter once Moorage proxies to
       apps: honouring them only from trusted addresses comes with proxying in
       slice 3, and access logs with the log manager there too.
     - Old builds are kept for now; garbage collection of builds beyond
       `keep_releases` comes with builds in slice 4.
3. **Native runtime.** Shims (spawn, log capture and rotation, restart with
   backoff, control socket, adoption after a daemon restart, no orphans on
   Linux, macOS and Windows); port allocation; TCP and HTTP health checks; the
   reconciler; rolling and recreate rollouts; proxying to healthy instances;
   `moor ps`, `moor logs -f`, `moor restart`. Runs spike S2.
   - Done when: an app keeps serving through daemon restarts, killing a shim
     leaves no orphan, and a rolling restart under load fails no requests.
   - Done (slice 3):
     - `[processes]`, `[deploy]` and `[env]` in the manifest; `moor deploy
       --artifact` uploads an app that is ready to run (a compiled binary, a
       script for an interpreter on the host) and runs its processes.
     - The shim (`moor shim`): starts the app (a simple command directly,
       anything else through the embedded shell, `moor sh -c`), writes its
       output to a rotated log with a timestamp and stream per line, restarts
       it with backoff, slows down a crash loop, stops it with its stop signal
       and kills it after the stop timeout, and takes commands over a loopback
       port guarded by a secret. No orphans: a parent-death signal on Linux, a
       Job Object on Windows, and on macOS the server kills an app whose shim
       died (identified by PID and start time).
     - The reconciler: ports picked at random and reserved in the database;
       TCP or HTTP health checks; rolling and recreate rollouts that switch
       traffic to exactly the new instances, then drain and stop the old ones;
       a release becomes current only once its instances are healthy (a
       failed one is removed, the previous release keeps serving, and the
       error shows the app's last output); unhealthy instances get no traffic
       and are restarted; dead shims are replaced; after a server restart,
       running instances are adopted before the listeners open, and missing
       ones are started.
     - The proxy: power-of-two-choices load balancing on requests in flight,
       one retry on another instance when a connection is refused, upgrades
       and streaming passed through, `X-Forwarded-*`, `Forwarded` and
       `X-Request-Id` set by Moorage (kept from trusted proxies only),
       `X-Moorage-*` headers from clients removed, and an access log per
       environment. The server's own log is also written to a rotated file.
     - `moor ps`, `moor logs [-f]` and `moor restart`; the API's instances,
       logs (NDJSON) and restart endpoints.
     - Spike S2: in tests and with the real binary running a Python app, the
       app kept serving on its own port while the server was down, and the new
       server adopted it (same PID). Spike S4 for instances: 10,000 to 58,000
       requests across a deploy, a rolling restart and a rollback, none
       failed; upgraded connections pass through. Verified by tests on macOS
       (with the race detector), `go vet` for Linux and Windows, the six
       builds, and the real binary.
   - Found by slice 3:
     - A shim's physical footprint is 6.2 MB on macOS (12.6 MB resident,
       counting shared executable pages), above the 5 MB target. Linux
       figures come from CI; one shim per environment is the fallback if the
       cost matters.
     - UNIX socket paths are limited to about 104 bytes on macOS, too short
       for deep data directories: shims take commands on a loopback port with
       a per-shim secret instead.
     - Python buffers its standard output when it is not a terminal, so its
       prints reach the log late: the Python runtime (slice 4) sets
       `PYTHONUNBUFFERED=1`.
     - On Windows the app joins its Job Object right after it starts; a child
       started in that instant escapes the job. Creating the process inside
       the job closes the gap (wave 7).
     - Right after a crash, until the next health check, the restarted process
       may not listen yet; with one instance a request can get the
       "unavailable" page for a moment.
     - The router is part of the server, so a server restart refuses
       connections for a moment even though apps keep running; listener
       hand-off is in Ideas. Log files of stopped instances are kept;
       retention arrives with observability (wave 5). Health checks by command
       are not supported yet.
4. **Builds.** Manifest v1 (parse, validate, `explain`, JSON Schema);
   detection; the `go`, `python` (uv), `node`, `command` and `binary`
   runtimes; the toolchain manager (Go, Node, uv and Python, all verified);
   in-place builds with streamed logs; `moor deploy` of a directory;
   deployments as persisted jobs; the release command; `moor rollback` of
   built apps; garbage collection of builds beyond `keep_releases`. Runs
   spike S3.
   - Done when: a sample app for each runtime (`testdata/apps/`) deploys by
     upload in CI on Linux and macOS and rolls back in seconds.
   - Done (slice 4):
     - Manifest version 1: every table is typed and checked, with every
       problem reported at once; top-level `x-*` tables are left to other
       tools. Warnings name values that look like credentials and settings
       accepted but not acted on yet. `moor manifest validate`, `explain`
       (the plan the server will make) and `schema` (a JSON Schema kept in
       step with the Go types by a test).
     - Detection and the runtime presets: `go` (the version from `go.mod`,
       the main package, `GOTOOLCHAIN=local`, cgo off), `python` (`uv sync
       --locked`, or a virtual environment and `requirements.txt`; the
       version from `.python-version` or `requires-python`; the project's one
       script, or uvicorn, Django or Flask apps found in the code), `node`
       (`npm ci`, the build script, pruning; the version from `.nvmrc`,
       `.node-version` or `engines.node`, else the current LTS; the start
       script run directly, or the build's `dist/` served as a static site),
       `static`, `command` and `binary`; a `Procfile`; extra toolchains, and
       `name@system` for the server's own.
     - The toolchain manager: Go (the download index and its SHA-256), Node.js
       (the release index and `SHASUMS256.txt`), uv (release assets and their
       `.sha256`; static builds on Linux) and Python (installed by uv into the
       toolchain directory, with no links in `bin` directories and nothing in
       the Windows registry). Partial versions and npm-style ranges resolve to
       the newest matching release; an installed version is used when the
       provider cannot be reached; installs are locked per toolchain and
       renamed into place; mirrors are set under `[toolchains]` in
       `server.toml`.
     - In-place builds: the source is unpacked where the build stays, and the
       build runs there with the toolchains and the project's tool
       directories first on `PATH`, shared caches, `CI=true` and Moorage's
       variables, within `[build].timeout`; a cancelled or timed-out step is
       killed with everything it started. `[build].exclude` and `.moorignore`
       are pruned and the files made read-only. Shell builtins (`exit`,
       `echo`, `cd`) now always run in the embedded shell.
     - Deployments are persisted jobs (`queued`, `building`, `releasing`,
       `rolling_out`, then `live`, `failed` or `superseded`), one per
       environment at a time; a newer deployment supersedes one still queued;
       builds run at most half the CPUs at once; deployments a stopped server
       interrupted are marked failed. Their logs are written to
       `logs/deployments/` and followed through the API as NDJSON. Uploads
       and rollbacks are recorded as deployments too.
     - The release command runs once per deployment as a one-off of the new
       release (not on a rollback); a failing one leaves the previous release
       serving. `on_create` runs after the first release of an environment.
     - `moor deploy [DIR]` uploads source (honouring `.gitignore` and
       `.moorignore`) and prints the build as it happens; `--detach`, or
       Ctrl-C, leaves it running. `moor deployments`, `deployments show` and
       `deployments follow`.
     - Garbage collection: builds that none of an environment's last
       `keep_releases` releases, no current release and no running
       deployment uses are removed after each deployment (their rows stay,
       and a rollback to one says why it cannot); failed builds are kept a
       day for inspection.
     - Spike S3: seven sample apps (`go`, `python`, `node`, `node-static`,
       `command`, `binary`, `static`) are built from source with toolchains
       downloaded and verified, deployed, changed, and rolled back in 0.2 to
       0.3 seconds; partial versions resolved as expected (`go 1.26` to
       1.26.10, `3.13` to 3.13.18, `24`, `>=22` to the 26.13.0 LTS). Python's
       virtual environment and editable install work from the build's final
       directory. With the real binary, the Python sample went from `moor
       deploy` on a fresh server (uv and Python downloaded) to live in 3.4
       seconds, and nothing was written outside the data directory. A CI job
       builds the sample apps on Linux and macOS.
   - Found by slice 4:
     - Node.js 25 and later no longer ship corepack, so pnpm and yarn need
       toolchains of their own; Node apps build with npm for now.
     - `SHASUMS256.txt` of Node.js is fetched over HTTPS from the official
       site, but its OpenPGP signature is not checked yet: it goes with the
       provenance checks of wave 6. A partial uv version would need the
       release list from GitHub's API; uv takes an exact version or the
       latest.
     - Not yet: the build sandbox (a build user, cgroup limits, lower
       priority) comes with Linux isolation (wave 6); build reuse by key with
       git pushes (wave 2); cancelling and retrying deployments with the
       console's deployment actions (wave 3); `moor toolchains` and pruning,
       a periodic sweep of old build directories and the disk guard (wave 6).
       Deployment logs are followed as NDJSON, like app logs; server-sent
       events arrive with the console's event stream (wave 3).
     - `TestDeadShimIsReplaced` failed once under the race detector on a
       loaded macOS machine (the replaced app still answered on its port for
       30 seconds) and passed in every later run; to be watched in CI.
     - Two traps of Python's `http.server`, found by the sample apps in CI on
       macOS, go into the per-runtime quick-start documentation (slice 6):
       - Its `HTTPServer` handles one connection at a time. The proxy's HTTP
         client may open a connection to a backend and leave it idle, and
         such a server then waits on it before answering anyone else (a
         request hung for 20 seconds). The samples use `ThreadingHTTPServer`,
         as `python -m http.server` does.
       - When it binds, it looks up the name of its address, and on the macOS
         runner (Homebrew Python 3.15) that reverse lookup of 127.0.0.1 took
         35 seconds, so each new instance needed 35 seconds to answer its
         first health check. The rollouts were correct, only slow. The
         samples bind without the lookup.
     - Rollouts now log a warning when a drain ends with requests still in
       flight and when an instance takes more than five seconds to stop; the
       sample-app test prints the apps' own logs when one fails.
5. **Configuration.** Variables and encrypted secrets (`moor env`), every
   configuration change a release.
   - Done when: setting a variable rolls out a new release without
     downtime, and secrets are encrypted at rest and never printed.
   - Done (slice 5):
     - Variables set on the server, for one environment or for every
       environment of an app (`--all-envs`; an environment's own value
       wins), override the manifest's `[env]`. `--build` makes a variable
       available to builds too. Variables can be set before the first
       deploy, which then uses them.
     - Secrets are sealed with AES-256-GCM under a data key per app, with
       the app, the scope and the name as associated data; data keys are
       sealed by the master key in `keys/master.key` (`0600`), or a file
       named by `MOORAGE_MASTER_KEY_FILE`, or a systemd credential. The
       master key is generated with the first server start; a server whose
       database holds secrets refuses to start without the key that sealed
       them, or with another one.
     - Every release records the variables it runs with (secrets sealed).
       A change of variables makes a new release of the current build,
       rolled out like a deploy (the release command does not run again);
       `--no-restart` keeps it for the next deploy. `moor rollback` keeps
       the current variables, and `--with-config` restores the earlier
       release's.
     - Secrets reach apps through their shim's standard input and stay in
       its memory: no file of the server holds one in the clear (the
       database, its WAL, specs and logs were searched in tests and with the
       real binary). Values are redacted from app, build and release-command
       logs. The API never returns them.
     - `moor env` (list, with each variable's source and whether it is
       overridden), `env set` (a secret's value read from standard input,
       or asked for without echo), `env unset`, `env import` (a `.env`
       file) and `env export`; the API's `GET` and `PATCH
       /apps/{app}/envs/{env}/vars`.
     - Verified by tests (a change of variables under load: 3,585 requests,
       none failed; secrets after restarts of the app and of the server; a
       missing master key), the race detector, `go vet` for Linux and
       Windows, the six builds, and the real binary.
   - Found by slice 5:
     - Not yet: revealing a secret (with the audit log, wave 4), `moor server
       rotate-key` (wave 6, with lifecycle), `--production-only` (with
       previews, wave 4) and declared variables (`[vars]`, wave 5).
     - Redaction is best effort: a secret shorter than six characters, or
       printed in pieces or encoded, is not caught.
     - A changed `--build` variable reaches the app with its next build; a
       way to rebuild the current commit (`moor redeploy`) comes with the
       console's deployment actions (wave 3).
6. **Release 0.1.0.** Service installation (systemd with cgroup delegation,
   launchd); install and per-runtime quick-start documentation; version
   0.1.0, its CHANGELOG section and this roadmap; pull request `wave1` →
   `main`; tag `v0.1.0`; release.
   - Done (slice 6):
     - `moor server install`, `uninstall`, `start`, `stop` (`--all` stops the
       apps too), `restart` and `status`: a systemd unit (`KillMode=process`,
       `Delegate=yes`, `TasksMax=infinity`) or a launchd daemon
       (`AbandonProcessGroup`, restarted when it fails), system-wide as root
       or for the current user, running the `moor` binary where it was
       installed from with the `PATH` it was installed with.
     - The guide (`docs/guide/`): installation, quick starts for Go, Python,
       Node.js, static sites and other apps, and variables and secrets, with
       every example checked with `moor manifest explain`.
     - Version 0.1.0 and its CHANGELOG section; this roadmap.
     - Verified by unit tests of the generated unit and plist and of the
       commands they drive, and in CI, where the server is installed as a
       service on Linux and macOS, restarted with an app under load, and
       uninstalled.
   - Found by slice 6:
     - Installing a real service changes the machine it runs on, so the
       service is exercised on CI's throwaway runners rather than on
       developers' machines.
     - The Windows service is part of the Windows server (wave 7); until
       then, `moor server run` can run under any service wrapper.

## 0.2.0: push to deploy (planned)

### Wave 2

1. **Git server.** First the open items of spike S1 (rejecting a ref from the
   pre-receive step, detecting the end of the pack, serialising ref updates
   per repository); then the embedded SSH server (git exec requests only,
   authorised on the verified key) and Smart HTTP; bare repositories;
   deploys of the production branch with streamed progress; a failed deploy
   failing the push; push to create; push options and `[skip deploy]`;
   `moor git remote` and `moor git credential`.
2. **Accounts.** Users, sessions, scoped API tokens and SSH keys, replacing
   wave 1's bootstrap token; the first-run claim URL; CLI browser login
   (`moor login`, `logout`, `whoami`, `context`); `moor keys`, `moor tokens`;
   the OpenAPI contract with problem+json errors and cursor pagination.
3. **Containers.** The container driver — one CLI lifecycle for Podman and
   Docker, an observation adapter per engine, label-based adoption — and the
   `dockerfile` and `image` runtimes, with digests pinned at release and
   registry credentials. Runs spike S6.
4. **TLS and operations.** `moor trust` (installs the internal CA into the
   OS trust store; certificate issuance itself arrived in wave 1, slice 2);
   `moor server proxy-config nginx`; `moor server doctor` (DNS, ports, TLS, the
   proxy path, the container engine, kernel features); the laptop-mode
   resolver helper. Completes spike S5.
5. **Release 0.2.0.**

## 0.3.0: the console (planned)

### Wave 3

1. **Console foundation.** Layout and CSS design tokens (light and dark),
   templates, sign-in and the first-run wizard, CSRF-protected forms, the event
   bus with one SSE stream, the activity timeline, the command palette.
2. **Apps and deployments.** The dashboard; the app overview with
   environments and instances; deployments with the live stage timeline and
   log, the resolved manifest and the release diff; deploy, rollback,
   redeploy, restart and scale actions.
3. **Logs, variables and settings.** The live log viewer (instances, sources,
   search, time range, download); variables with secret, source and override
   badges and bulk edit; account pages (SSH keys, tokens, sessions); server
   settings; the System page with the `doctor` results.
4. **Release 0.3.0.**

## 0.4.0: previews and sharing (planned)

### Wave 4

1. **Projects and roles.** Projects; viewer, deployer and maintainer roles
   enforced in the API; the audit log in the API, CLI and console.
2. **Previews and environments.** Branch previews (naming, inheritance, TTL,
   pinning, limits, destroyed with their branch), named environments,
   `promote`, deploy locks, the Previews page.
3. **Sleep and wake.** Idle sleep with the waking-up page; per-process
   opt-out.
4. **Access gates.** Password, sign-in (with identity headers), share links,
   IP allow-lists, default gates per environment kind, `noindex` outside
   production; an independent review of the gate, cookie and CSRF design.
5. **Paths and domains.** Path prefixes and path mode; custom domains with
   DNS verification.
6. **Release 0.4.0.**

## 0.5.0: real apps, observed (planned)

### Wave 5

1. **Processes.** Workers, scaling, cron jobs, one-off runs (`moor run`),
   declared and generated variables, `watch` paths.
2. **Metrics.** Collection (cgroups, process sampling, engine statistics,
   the ingress), tiered storage, console charts with deploy markers,
   sparklines, the Prometheus endpoint.
3. **Notifications.** Webhooks, chat webhooks, email and in-console
   notifications, with rules and deduplication.
4. **Static extras.** Drag-and-drop static deploys in the console; static
   release permalinks.
5. **Release 0.5.0.** Exit check: 50 or more environments on a 4 vCPU / 8 GB
   VM with sleep enabled.

## 0.6.0: hardened (planned)

### Wave 6

1. **Linux isolation.** Per-app UIDs, cgroup limits, the Landlock sandbox,
   soft enforcement elsewhere; limits and the effective isolation level in
   the console. Runs spike S7.
2. **Backups.** On demand and scheduled, to a local path or S3-compatible
   storage; restore; an automated restore drill in CI.
3. **Identity.** OIDC single sign-on, TOTP, trusted-header authentication
   behind an authenticating proxy.
4. **Lifecycle.** Self-upgrade with rollback; server configuration export and
   import; provenance checks for toolchain downloads where published.
5. **Release 0.6.0.**

## 0.7.0: Windows, forges and the terminal (planned)

### Wave 7

1. **Windows server.** Job Objects, the Windows service, console control;
   the integration suite green on Windows.
2. **Forge integration.** Connected repositories, deploy webhooks,
   pull-request previews with status checks and URL comments, approval for
   forks and unknown authors.
3. **Interactive access.** `moor run -it`, the line-mode web console,
   `moor exec`, the file browser for data directories.
4. **Telemetry export.** Log forwarding, OTLP, threshold alerts.
5. **Release 0.7.0.**

## 0.8.0: beyond the firewall (planned)

### Wave 8

1. **ACME.** Internal ACME servers and public CAs; HTTP-01, TLS-ALPN-01 and
   DNS-01 wildcard certificates; the `public` preset.
2. **Exposure hardening.** An external penetration test of every public
   surface, and its fixes.
3. **Protocols.** Cleartext HTTP/2 backends (gRPC); sticky sessions.
4. **Release 0.8.0.**

## 0.9.0: services (planned)

### Wave 9

1. **Managed services.** PostgreSQL, MySQL, Redis/Valkey and S3-compatible
   storage through the container engine, linked to apps with injected URLs.
2. **Services for previews.** Per-preview instances seeded from snapshots;
   service backups.
3. **Multi-container apps.** Apps described by a compose file, routed by
   service.
4. **Release 0.9.0.**

## 0.10.0: ecosystem (planned)

### Wave 10

1. **Native-in-container isolation** for native builds.
2. **Templates and app metrics.** One-click starter apps; scraping an
   app-provided metrics endpoint into custom charts.
3. **Extension hooks and `moor mcp`.** A small, versioned set of lifecycle
   hooks; a safe subset of the API as Model Context Protocol tools, scoped by
   a token.
4. **More toolchains.** Java, Rust, Ruby, PHP, .NET.
5. **Release 0.10.0.**

## Toward 1.0

1.0 means:

- Manifest v1 and API v1 frozen, with a promise that later versions accept
  them.
- Upgrades tested from every 0.x release.
- The security review and penetration-test findings closed.
- Complete operator and developer documentation.
- Several internal teams using Moorage every day.

## Spikes

Each spike runs at the start of the slice that needs it, ends in a short
written finding and an update to [Decisions](12-decisions.md), and changes the
slice before code is built on a failed assumption.

| # | Question | Slice | Done when |
|---|---|---|---|
| S1 | Can we receive `git push` in-process over SSH and HTTP and stream live progress back to the client? | Wave 2, slice 1 | *Largely shown by a research probe with the git CLI.* Remaining: rejecting a ref from the pre-receive step with a deployment error; end-of-pack detection for clients that keep the connection open (tested with a non-CLI git client library); per-repository ref serialisation under concurrent pushes; the beta library wrapped behind an internal interface. |
| S2 | Do shims keep apps alive across daemon restarts on all three OSes, without orphans? | Wave 1, slice 3 (and service installation in slice 6) | `systemctl restart` (with `KillMode=process` and delegation), a launchd restart and a Windows service restart under load drop no requests; killing a shim kills its app on every OS; shim RSS measured (target ≤ 5 MB). *Slice 3: apps outlive the server and are adopted (tests, and the real binary with a Python app); a killed shim leaves no app behind on any OS (tests); footprint 6.2 MB on macOS. The service managers' part comes with service installation.* |
| S3 | Do in-place builds work for uv/Python, Node and Go, including partial toolchain versions? | Wave 1, slice 4 | Sample apps build and run from their final directory; toolchain downloads verified. *Shown in slice 4 on macOS: seven sample apps built from source, deployed and rolled back, with partial versions resolved and every download verified (a CI job runs them on Linux and macOS); the Node.js checksum file's signature is checked in wave 6.* |
| S4 | Is the routing table swap really zero-downtime? | Wave 1, slices 2–3 | Rolling replacement under 200 req/s load with zero failed requests; WebSockets pass through. *Static sites shown in slice 2 (2,046 requests across 12 swaps, none failed); proxied instances in slice 3 (over 10,000 requests across a deploy, a rolling restart and a rollback, none failed; upgraded connections pass through).* |
| S5 | Does the internal CA work end to end — an auto-generated root and a provided intermediate — for browsers and CLI tools? | Wave 1, slice 2 (issuance), wave 2, slice 4 (`moor trust`) | Certificates issued on demand for routed hostnames only, renewed, trusted after `moor trust` or through the corporate chain. |
| S6 | Container driver: shared CLI lifecycle plus per-engine observation adapters. | Wave 2, slice 3 | Run, label-based adoption, exit and OOM events, cgroup-based stats on Linux and REST-based stats elsewhere, `logs --since`, rootless loopback port publishing — on current Podman and Docker releases. |
| S7 | Linux cgroup v2 delegation and Landlock applied by the exec helper. | Wave 6, slice 1 | Limits enforced, OOM detected and reported, filesystem rules effective. |

## Parking lot

- **Beyond one node** (Phase 5): remote `moor agent` processes on additional
  hosts over mutual TLS, placement by labels, dedicated build hosts, one
  console for all of them. The driver and reconciler boundaries are designed
  so this can be added without changing the API or the manifest. Waits for a
  single-node 1.0 and a real need.

## Ideas (unscheduled)

- Listener hand-off through systemd socket activation, so not even a
  connection is refused during a daemon restart.
- A few portable built-in utilities (`cp`, `mkdir`, `rm`) for manifest
  commands on stock Windows.
- Passkeys for sign-in.
- HTTP/3 for clients.

## Risks

| Risk | Why it matters | What Moorage does |
|---|---|---|
| The git library's server side is in beta | Pushes are the main way in | Pinned and wrapped behind an internal interface; tested with the git CLI and another client library; fallback to `git receive-pack` when the binary is present |
| Shim memory per instance | Many small apps on one VM | Measured in wave 1 slice 3 (target ≤ 5 MB); fallback to one shim per environment |
| Kernel and service-manager variety | cgroup delegation and Landlock vary across distributions | Best-effort controls with the effective protection reported; `doctor` checks |
| Windows parity | Process control differs the most there | CI on Windows from wave 1; Windows server support completed in wave 7 |
| Builds competing with apps | One VM runs both | Builds get limits, a lower CPU weight and a timeout |
| Running teammates' code | Deploying is running code on the host | The trusted-team model stated plainly; isolation levels; the audit log |
| Scope | A broad platform can stall before it is dependable | Every wave ends in a release; every slice is tested and useful on its own |

## Engineering practices (every wave)

- **Testing:**
  - unit tests, including table-driven detection tests against fixture
    repositories in `testdata/apps/`;
  - integration tests that start a real `moor server` in a temporary data
    directory, deploy sample apps (by upload, and by `git push` from wave 2)
    and assert on the served responses — on Linux, macOS and Windows;
  - zero-downtime load tests for rollouts;
  - chaos tests: kill the daemon mid-rollout, kill shims, fill the disk;
  - fuzzing of the manifest parser, redirect rules, archive extraction and
    git protocol handling;
  - console template tests (golden HTML) and browser end-to-end tests of key
    flows, driven from Go through the Chrome DevTools Protocol.
- **Definition of done for a slice:** tests and documentation updated (and
  the OpenAPI contract once it exists), the CHANGELOG's Unreleased section
  updated, CI green on every target.
- **Releases:** binaries for six targets with SHA-256 checksums, drafted from
  the CHANGELOG; signatures and an SBOM from wave 6.
