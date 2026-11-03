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

## 0.1.0: first deploy (planned)

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
2. **Ingress and static sites.** The routing table with atomic swaps;
   hostname routing under the base domain; static serving with the `[static]`
   rules (SPA fallback, clean URLs, headers, redirects); platform pages; TLS
   modes `files` and `off`, with the `[proxy]` section for running behind a TLS
   proxy; a deploy API guarded by a bootstrap admin token;
   `moor deploy --static`; instant static rollback. Runs spike S4.
   - Done when: a site deployed with `moor deploy --static` is served on its
     hostname, directly over TLS and behind nginx; deploys and rollbacks under
     load fail no requests.
3. **Native runtime.** Shims (spawn, log capture and rotation, restart with
   backoff, control socket, adoption after a daemon restart, no orphans on
   Linux, macOS and Windows); port allocation; TCP and HTTP health checks; the
   reconciler; rolling and recreate rollouts; proxying to healthy instances;
   `moor ps`, `moor logs -f`, `moor restart`. Runs spike S2.
   - Done when: an app keeps serving through daemon restarts, killing a shim
     leaves no orphan, and a rolling restart under load fails no requests.
4. **Builds.** Manifest v1 (parse, validate, `explain`, JSON Schema);
   detection; the `go`, `python` (uv), `node`, `command` and `binary`
   runtimes; the toolchain manager (Go, Node, uv and Python, all verified);
   in-place builds with streamed logs; `moor deploy` of a directory; the
   release command; `moor rollback`; variables and encrypted secrets, every
   configuration change a release. Runs spike S3.
   - Done when: a sample app for each runtime (`testdata/apps/`) deploys by
     upload in CI on Linux and macOS and rolls back in seconds.
5. **Release 0.1.0.** Service installation (systemd with cgroup delegation,
   launchd); install and per-runtime quick-start documentation; version
   0.1.0, its CHANGELOG section and this roadmap; pull request `wave1` →
   `main`; tag `v0.1.0`; release.

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
4. **TLS and operations.** `internal` TLS (an auto-generated root or a
   corporate intermediate, `moor trust`); `moor server proxy-config nginx`;
   `moor server doctor` (DNS, ports, TLS, the proxy path, the container
   engine, kernel features); the laptop-mode resolver helper. Runs spike S5.
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
   `on_create` seeding, declared and generated variables, `watch` paths.
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
| S2 | Do shims keep apps alive across daemon restarts on all three OSes, without orphans? | Wave 1, slice 3 | `systemctl restart` (with `KillMode=process` and delegation), a launchd restart and a Windows service restart under load drop no requests; killing a shim kills its app on every OS; shim RSS measured (target ≤ 5 MB). |
| S3 | Do in-place builds work for uv/Python, Node and Go, including partial toolchain versions? | Wave 1, slice 4 | Sample apps build and run from their final directory; toolchain downloads verified. |
| S4 | Is the routing table swap really zero-downtime? | Wave 1, slice 2 | Rolling replacement under 200 req/s load with zero failed requests; WebSockets pass through. |
| S5 | Does the internal CA work end to end — an auto-generated root and a provided intermediate — for browsers and CLI tools? | Wave 2, slice 4 | Certificates issued on demand for routed hostnames only, renewed, trusted after `moor trust` or through the corporate chain. |
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
