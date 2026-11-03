# 12. Decisions and Open Questions

A lightweight decision log. Each entry records the context, the decision, the
alternatives considered and the consequences. Entries marked **(spike)** are
provisional until the corresponding Phase 0 spike reports back.

## Decisions

### D1 — One daemon process, one shim per native instance

- **Context:** apps must keep running when Moorage restarts or upgrades, and
  their output must not be lost.
- **Decision:** the daemon launches every native instance through
  `moor shim` (same binary), which owns the app's output, applies OS controls,
  restarts it per policy and takes commands over a loopback port guarded by a
  per-shim secret. The daemon adopts shims
  on startup. **(spike S2)**
- **Alternatives:** apps as direct children of the daemon (simplest, but every
  daemon restart is an outage); systemd transient units (excellent on Linux,
  but Linux-only); a separate long-lived supervisor daemon (one process
  instead of N, but upgrading it still restarts every app).
- **Consequences:** a few MB of memory per instance (measured in wave 1:
  5.3 MB private on Linux and a 6.2 MB physical footprint on macOS, against
  a 5 MB target);
  package `init()` code must stay cheap; fallback to one shim per environment
  if the per-instance cost matters in practice. The control channel is a
  loopback TCP port with a per-shim secret rather than a UNIX socket, whose
  paths are limited to about 104 bytes on macOS. Each OS needs its own "no orphans, but survive the daemon" wiring:
  on Linux the systemd unit must use `KillMode=process` with a delegated
  cgroup subtree (otherwise stopping the service kills every app), and the
  daemon handles host shutdown itself; on macOS launchd must not reap the
  daemon's process group and orphaned apps are cleaned up by PID + start time;
  on Windows the Job Object belongs to the shim, not the daemon.

### D2 — Native first, containers as a runtime

- **Context:** the goal is to run any app, including ones without a container
  image, on hosts that may not have a container engine.
- **Decision:** native runtimes are the default; `dockerfile` and `image`
  runtimes use the container engine when present. Both share one driver
  interface, one health checker, one router and one log pipeline.
- **Consequences:** Moorage must manage toolchains (D9) and provide process
  isolation itself (Runtime → Isolation levels).

### D3 — Containers: CLI for lifecycle, per-engine adapters for observation (spike S6)

- **Context:** both engines accept the same lifecycle commands and flags, but
  their machine-readable `ps`, `stats` and `events` outputs are *not*
  interchangeable (different JSON shapes, human-formatted numbers). Their
  Docker-compatible REST APIs are close enough for lifecycle and observation,
  but the API service is not always running (rootless Podman in particular).
- **Decision:** lifecycle (`build`, `pull`, `run`, `stop`, `rm`, `inspect`)
  goes through the engine CLI with one shared code path. Observation goes
  through a small adapter per engine: container CPU/memory read directly from
  cgroups on Linux (shared with native instances), exits and OOM kills from
  the engine's event stream parsed per engine, and the Docker-compatible REST
  API whenever its socket is available (always the case for the VM-based
  engines on macOS and Windows).
- **Alternatives:** REST only (needs the API service everywhere); CLI only
  (fragile parsing of two output dialects for metrics); vendor SDKs (large
  dependency trees, Docker-specific).
- **Consequences:** works out of the box with rootless Podman and Docker, and
  every lifecycle action is a command an operator can reproduce by hand. The
  adapters are covered by integration tests against each supported engine
  version.

### D4 — Built-in git server (spike S1 — largely de-risked)

- **Decision:** accept pushes over an embedded SSH server and Smart HTTP using
  the server-side support in the next major version of the pure-Go git
  library (`go-git/v6`: Smart HTTP and stream serving, pre-/post-receive
  hooks, push options, progress side-band), and store bare repositories
  ourselves.
- **Evidence:** a research probe served pushes and clones to a stock `git`
  client over SSH and Smart HTTP — new, fast-forward, forced and deleted refs,
  tags, a 30 MB pack, shallow clones, push options — with streamed `remote:`
  progress lines and a clean `git fsck`.
- **Known gaps to handle:**
  - the library major version is still in beta: pin it and keep it behind an
    internal interface;
  - no atomic multi-ref pushes and no cross-ref transactions: serialise ref
    updates per repository;
  - the library reads the pack until the client closes its side; the git CLI
    does, but other git client libraries may not, so Moorage must detect the
    end of the pack itself (an upstream fix is in progress);
  - ref status is settled *before* post-receive hooks run, so a deployment
    that must be able to fail the push runs in the pre-receive step (see
    [Build and deploy → Live output](04-build-and-deploy.md#live-output)).
- **Alternatives:** require system `git` and hook into the host's `sshd`
  (forced commands in `authorized_keys`) — proven, but adds host dependencies,
  is Unix-centric and couples Moorage to host SSH configuration.
- **Consequences:** no host dependencies and identical behaviour on all OSes.
  Fallback if the library stalls: run `git receive-pack` when the binary is
  present, keeping the embedded SSH server and the same hook logic.

### D5 — Built-in ingress instead of configuring an external proxy

- **Decision:** routing, TLS, gates, static serving, load balancing and
  wake-on-request live in the daemon.
- **Alternatives:** generate configuration for a separate reverse proxy.
- **Consequences:** no version skew or config-reload failures between
  separately upgraded components; health and draining are exact because the
  router and the supervisor share state; the daemon is on the request path, so
  daemon restarts must be fast and listener handoff is worth doing on Linux.

### D6 — SQLite (pure Go) for metadata, files for bulk data

- **Decision:** `modernc.org/sqlite` in WAL mode for all metadata; logs,
  builds, repositories and app data as files; raw metrics in memory with
  roll-ups in SQLite.
- **Alternatives:** an embedded key-value store (no SQL, harder ad-hoc
  inspection); a WebAssembly-based SQLite driver (also pure Go, but pre-1.0
  and recently re-architected); an external database server (contradicts the
  single-binary goal).
- **Consequences:** single writer; high-volume paths must batch. The driver is
  mature, releases frequently and covers all six targets, but requires its
  exact `modernc.org/libc` version to be pinned in `go.mod`. It sits behind
  `database/sql`, so switching drivers later is cheap.

### D7 — TOML for the manifest

- **Decision:** `moorage.toml`, versioned, with a published JSON Schema. One
  format only — no parallel `moorage.yaml`.
- **Why:** the manifest is a small, mostly flat, hand-written file — TOML's
  sweet spot. Types are explicit (strings are quoted, so `"3.10"` stays a
  version string and `no` is never silently a boolean), there is no
  significant indentation, the specification is small enough that parsers
  agree, and Python, Go and Rust developers already know it from
  `pyproject.toml` and `Cargo.toml`.
- **Alternatives:** YAML — familiar from CI and orchestration tools and has
  anchors for reuse, but brings implicit-typing surprises, indentation errors
  and behaviour that differs between parsers; JSON — no comments, noisy to
  write by hand.
- **Downsides accepted:**
  - Deep nesting is verbose (`[environments.staging.processes.web]`). The
    manifest is designed to stay shallow, and environment overrides inherit
    rather than repeat whole blocks.
  - A key belongs to the most recent `[table]` header, so a key appended at
    the bottom of a file can silently land in the wrong table. Moorage rejects
    unknown keys, which turns this into an immediate, precise error.
  - Two bracket forms (`[table]` and `[[array-of-tables]]`) and single-line
    inline tables take a moment to learn; the examples and `moor init` output
    model the idioms.
  - No anchors or references for reuse; inheritance rules cover the real
    cases.
  - Less familiar to people who mostly write YAML.
- **Consequences:** the manifest decodes into typed Go structs described by a
  JSON Schema, so a second syntax could be added cheaply later if it were
  ever truly needed.

### D8 — Embedded POSIX shell for manifest commands

- **Decision:** interpret commands with `mvdan.cc/sh`; execute simple commands
  directly.
- **Alternatives:** `/bin/sh -c` (not on Windows; behaviour differs across
  systems); tokenising without a shell (surprises users who write `&&`).
- **Consequences:** identical semantics on every OS; `shell = "system"` remains
  available for scripts that need bash features.

### D9 — Managed, verified toolchains

- **Decision:** Moorage downloads and verifies language runtimes per exact
  version and records them per build.
- **Alternatives:** require runtimes on the host (drift between apps, "works on
  my server" problems); delegate to an external version manager (another host
  dependency).
- **Consequences:** reproducible builds and painless upgrades per app; Moorage
  owns a provider per toolchain and must keep up with upstream download
  formats. `name@system` remains as an escape hatch.

### D10 — Builds are built in place; builds and releases are separate

- **Decision:** build in the final immutable directory; a release is build +
  configuration snapshot; config changes, rollbacks and promotions create
  releases without rebuilding.
- **Consequences:** relocation problems (virtualenvs, absolute paths) disappear;
  rollback and promote take seconds; builds can be shared and reused across
  environments.

### D11 — Hostnames are single labels: `<app>--<env>.<base-domain>`

- **Decision:** previews and named environments use a double-dash separator
  inside one DNS label.
- **Alternatives:** nested subdomains (`env.app.base`) — need a wildcard record
  and certificate per app.
- **Consequences:** one wildcard DNS record and one wildcard certificate cover
  everything; app names cannot contain `--`; long names are truncated with a
  hash.

### D12 — Level-triggered reconciliation

- **Decision:** desired state in the database; a reconciler converges actual
  state; rollouts are sequences of desired states.
- **Consequences:** crash and restart recovery are the normal code path rather
  than special cases; behaviour is testable by asserting on convergence.

### D13 — API-first with a checked-in OpenAPI contract

- **Decision:** the OpenAPI document is the source of truth; server types are
  generated from it; the CLI uses the API; the console and the API handlers
  share one service layer, and every console action maps to an API operation.
- **Consequences:** no UI-only features (a test enforces the mapping);
  contract drift fails CI; third-party automation is supported from day one.

### D14 — Console: server-rendered Go templates + plain HTML/CSS/JavaScript

- **Decision (project owner):** the console is rendered by the Go server with
  `html/template`, styled with hand-written CSS and enhanced with small
  plain-JavaScript modules; everything is embedded with `go:embed`.
- **Alternatives:** a JavaScript framework single-page app (Svelte, React) —
  rich components, but a Node.js toolchain and build step for contributors;
  a small partial-update library — an extra dependency for little gain; a
  framework-free single-page app calling the JSON API — duplicates rendering
  logic in the browser.
- **Consequences:** a Go-only toolchain and a single `go build`; pages work
  without JavaScript and get live behaviour through one SSE connection;
  charts, the log viewer and the command palette are written in-house (small,
  bounded pieces); the Phase 3 web terminal needs a decision about a terminal
  emulator (Q11).

### D15 — Linux daemon runs as root and drops privileges for all user code

- **Decision:** default on Linux; an unprivileged mode exists with reduced
  isolation.
- **Consequences:** per-app UIDs, cgroup management and low ports work out of
  the box; the daemon itself never runs repository code as root; systemd
  hardening limits the daemon's own reach.

### D16 — Secrets with envelope encryption and a local master key

- **Decision:** AES-256-GCM per value, data keys wrapped by a master key on
  disk (or supplied by the service manager), KMS later.
- **Consequences:** database backups alone never expose secrets; operators must
  back up the master key separately.

### D17 — Internal-first, flexible TLS

- **Context (project owner):** the first installations are internal-only, and
  Moorage must work both terminating TLS itself and as plain HTTP behind an
  existing TLS proxy such as nginx.
- **Decision:** the default TLS mode is `internal` — an auto-generated
  self-signed root, or preferably an intermediate issued (and name-
  constrained) by the corporate CA — with `files` for certificate files such
  as a corporate wildcard, and `off` plus a `[proxy]` section (trusted proxy
  addresses, external scheme) for running behind a TLS-terminating proxy.
  Both topologies are first-class: `moor server proxy-config nginx` and
  `moor server doctor` support the proxied one. ACME (internal ACME servers
  and public CAs, including DNS-01 wildcards) moves to Phase 3 with the
  `public` preset.
- **Consequences:** Phase 1 needs no ACME library — the internal CA and
  certificate selection use the standard library (`crypto/x509`,
  `crypto/tls`). With a corporate intermediate, company machines trust
  Moorage URLs without any per-user setup.

### D18 — Service installation written in-house

- **Context:** keeping apps alive across daemon restarts depends on exact
  service-manager settings (systemd delegation and `KillMode=process`,
  launchd `AbandonProcessGroup`), which generic installer libraries do not
  model; the leading library is also slow-moving.
- **Decision:** generate the systemd unit and launchd plist from templates in
  Moorage and drive `systemctl`/`launchctl`; use `golang.org/x/sys/windows/svc`
  for the Windows service.
- **Consequences:** a little more code, full control over the settings that
  matter, one dependency fewer.

## Open questions

These change scope or defaults and are best answered by the project owner.

| # | Question | Status / current assumption |
|---|---|---|
| Q1 | Manifest format: TOML or YAML? | **Resolved:** TOML (D7) |
| Q2 | Console stack? | **Resolved:** server-rendered Go templates + plain HTML/CSS/JavaScript (D14) |
| Q3 | Internet-facing or internal-only? | **Resolved:** internal-only for now (D17) |
| Q4 | Are deployers always trusted colleagues, or will untrusted code (e.g. customer-submitted) ever be deployed? | Trusted only; VM-level isolation is out of scope |
| Q5 | Is single sign-on (OIDC with the corporate identity provider) needed at launch? | Phase 3; local accounts before that. An internal-only rollout makes corporate sign-in more attractive early. |
| Q6 | Where is source code hosted (GitHub, GitLab, Gitea, other)? Should pull-request previews with status checks come earlier than Phase 3? | Push-based previews in Phase 2, forge integration in Phase 3 |
| Q7 | How important is Windows as a *server* (vs. developer laptop) platform? | Server support completes in Phase 3 |
| Q8 | How important are managed databases for previews? | Phase 4; apps bring their own or use SQLite in the data directory until then |
| Q9 | Which toolchains matter after Go, Python and Node? | Bun and Deno in Phase 1–2; Java, Rust, Ruby, PHP, .NET in Phase 4 |
| Q10 | Licence and distribution model (open source? which licence? release signing identity?) | To be decided before the first public release |
| Q11 | The Phase 3 web terminal needs a terminal emulator in the browser — a large piece of code. Vendor one pinned third-party file (an exception to "no third-party browser code"), or ship a simpler in-house line-mode console (send a command, stream its output)? | Line-mode console first; revisit at Phase 3 |
