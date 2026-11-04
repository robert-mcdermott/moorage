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
     - The guide (`docs/guide/`, moved up to `docs/` in wave 2): installation,
       quick starts for Go, Python, Node.js, static sites and other apps, and
       variables and secrets, with every example checked with
       `moor manifest explain`.
     - Version 0.1.0 and its CHANGELOG section; this roadmap.
     - Verified by unit tests of the generated unit and plist and of the
       commands they drive, and in CI, where the server is installed as a
       service on Linux and macOS, restarted with an app under load, and
       uninstalled: the app keeps its process and none of its requests fail
       (2,789 during the restart on Linux, 632 on macOS), and the router is
       back within 3 s (one or two requests refused on either system).
   - Found by slice 6:
     - Installing a real service changes the machine it runs on, so the
       service is exercised on CI's throwaway runners rather than on
       developers' machines.
     - The Windows service is part of the Windows server (wave 7); until
       then, `moor server run` can run under any service wrapper.
     - launchd starts a job at most once every 10 seconds by default,
       restarts included: a restart 4 s after the server started left the
       router down for 6 s. The plist sets `ThrottleInterval` to 2 s, like
       the unit's `RestartSec`, and CI fails if the router is down for over
       3 s.
     - A shim's private memory on Linux is 4.4 to 5.3 MB across CI runs
       (about 17 MB resident, most of it the binary's pages shared with the
       server): around the 5 MB target, with 6.2 MB on macOS. One shim per
       environment remains the fallback if the cost matters.

## 0.2.0: push to deploy (released 2026-11-03)

### Wave 2

1. **Documentation and examples.** The design documents, roadmap and
   decisions move to `spec/`, and `docs/` holds the documentation for people
   who run Moorage and deploy apps to it: installation, the quick starts, a
   reference for `moorage.toml` as the current release implements it, and
   variables and secrets. A README that says what Moorage is, what works
   today and how to start. Examples in `examples/`, each small but
   realistic, with its own README: a Python app with uv, FastAPI and
   uvicorn and a web UI; a Go app with only the standard library; an
   Express API with a Vite front end; a static site with serving rules.
   - Done when: every example deploys from its directory with
     `moor deploy` and answers its pages and its API in CI on Linux and
     macOS, and no link in the README, `docs/` or `spec/` points at a file
     that does not exist.
   - Done (slice 1):
     - `spec/` holds the design documents, the roadmap and the decisions;
       `docs/` holds the documentation for people who run Moorage and deploy
       to it, with an index and a reference for `moorage.toml` written from
       the code (what 0.1.0 acts on, the variables it sets, detection, and
       what is accepted but not acted on yet).
     - The README: what Moorage is, what it does today, a quick start on
       one's own computer with an example, a server in two commands, the
       examples, the documentation and the status.
     - Four examples, each with a README: Tasks (FastAPI, uvicorn, a web
       page, SQLite, migrations as the release command), Links (Go's
       standard library, a graceful stop, an append-only data file two
       instances can share), Poll (Express and a Vite front end) and
       Handbook (a static site with clean URLs, a 404 page, headers and
       redirects).
     - The logo: SVG versions of the concept, the icon redrawn from its
       measured geometry and the wordmark traced, with a variant for dark
       backgrounds; the icon in the server's own pages and as their favicon
       (`internal/brand`, kept identical to `docs/assets` by a test).
     - Verified by `.github/scripts/examples-check.sh`, which deploys the
       four examples with the binary to a server of its own and makes 24
       checks of their pages and APIs (a variable change and a rollback
       included), locally on macOS and in a new CI job on Linux and macOS;
       and by a test that every link and heading anchor in the README, the
       changelog, `docs/`, `spec/` and `examples/` exists.
   - Found by slice 1:
     - Without a service, nothing stops the apps of a server run in the
       foreground: `moor server stop --all` goes through the service
       manager, so the examples check stops each app's shim by its PID.
       Stopping a foreground server's apps belongs with `moor server
       doctor` and the other operations of slice 5.
     - `moor manifest validate` said declared variables (`[vars]`) arrive
       in the next release; they come with wave 5. Fixed.
     - A first Python deploy on a fresh server mostly downloads uv, Python
       and the dependencies: about two minutes on a home connection, four or
       five seconds on CI's runners. Later deploys reuse them.
2. **Git server.** First the open items of spike S1 (rejecting a ref from the
   pre-receive step, detecting the end of the pack, serialising ref updates
   per repository); then the embedded SSH server (git exec requests only,
   authorised on the verified key) and Smart HTTP; bare repositories;
   deploys of the production branch with streamed progress; a failed deploy
   failing the push; push to create; push options and `[skip deploy]`;
   `moor git remote` and `moor git credential`.
   - Done (slice 2):
     - Spike S1's open items. A push's request is read by Moorage itself
       and handed to the git library ended at the pack's trailing checksum,
       so a client that keeps its side open gets its answer (shown with a
       client that does, which waits forever without it); pushes larger than
       1 GiB are refused. Ref updates are serialised per repository, and a
       branch whose push is deploying refuses a second push until it ends;
       builds run outside the lock. The library's pre-receive hook refuses
       a whole push: a failed deploy rejects every ref of its push, with the
       deployment's ID. The library is used by `internal/gitserver` only, a
       test checks it, and pinned at `v6.0.0-beta.1`.
     - The SSH server (`golang.org/x/crypto/ssh`): session channels and
       `exec` requests for `git-receive-pack` and `git-upload-pack` only;
       the user comes from the permissions of the key the client signed
       with; an Ed25519 host key in `keys/`. Smart HTTP at `/git/<app>.git`
       on the console's hostname, with the API token as the password.
     - Bare repositories under `repos/`; push to create; the production
       branch is the repository's default branch (`main`, or the first
       branch the first push creates); other branches and tags are kept;
       the production branch cannot be deleted. The commit's tree is written
       into the build directory as an upload would be (no symbolic links or
       submodules; LFS pointers warned about), and the deployment records
       its ref, commit and message. Progress is the deployment's log, as
       `remote:` lines; a client that leaves does not stop the deploy, and
       the branch moves if it succeeds. Push options `skip-deploy`,
       `detach` and `env=production`; `[skip deploy]`.
     - `moor keys` (`add`, `remove`), brought forward from slice 3: until
       accounts, every key is the administrator's. `moor git remote`
       (`--http`), `moor git credential`, and commands that find the app
       from the `moorage` remote. `/api/v1/server` gives the remotes'
       prefixes; `/api/v1/me/ssh-keys` the keys.
     - Verified by tests of the library's client over SSH and HTTP
       (pushes, clones, refusals, concurrent pushes, a client that leaves,
       the default branch, the export, a push with the connection held open)
       and of the git and ssh command lines against the whole server (a
       deploy by push, a failed one, `[skip deploy]`, another branch, HTTPS
       with the token, clones over both), with the race detector; and with
       the binary on macOS: `moor keys add`, `moor git remote`, pushes over
       SSH and over HTTPS through `moor git credential`.
   - Found by slice 2:
     - The library's own client closes its side after the pack, as the git
       command line does; the end of the pack matters for clients that do
       not, which the tests imitate.
     - git hands a working credential to every configured helper, so a
       keychain helper would keep the API token and answer with it after it
       changes. `moor git remote --http` clears the other helpers for the
       server's git URL, as `gh` does for its hosts.
     - The `local` preset listens for SSH on 127.0.0.1:2222 since 0.1.0, so
       servers from 0.1.0 start serving git over SSH once upgraded; the
       tests' servers turn it off unless they test it.
3. **Accounts.** Users, sessions, scoped API tokens and SSH keys, replacing
   wave 1's bootstrap token; the first-run claim URL; CLI browser login
   (`moor login`, `logout`, `whoami`, `context`); keys per user (`moor keys`
   arrived in slice 2, every key the administrator's), `moor tokens`;
   the OpenAPI contract with problem+json errors and cursor pagination.
   - Done (slice 3):
     - Users with an email address, a name, an Argon2id password (at least 12
       characters, common and repetitive ones refused) and a role; sessions with
       idle and absolute timeouts, in a host-only `__Host-` cookie over HTTPS;
       API tokens with scopes, an optional app and expiry, stored as hashes;
       SSH keys per user, a key having one owner.
     - The first-run claim: `moor server init`, `moor server status` and the
       server's log give a one-time link, whose page creates the first admin.
       Invitations: one-time links for new users and forgotten passwords.
     - CLI browser login: `moor login` (a device code, approved on a page that
       shows the client, its address and the code), `--with-token`, `logout`,
       `whoami`, `context`; `moor tokens`; `moor server users`.
     - One route table for the API: each route declares its permission, a test
       enumerates every route against every kind of caller, and an unset
       permission stops the server from starting. Cookie-authenticated changes
       need the session's CSRF token and the console's own origin.
     - The OpenAPI 3.1 document, generated from that table and the API's types,
       checked in as `docs/openapi.json` (a test fails on drift) and served at
       `/api/v1/openapi.json`. Field-level problem details; cursor pagination of
       apps, deployments, releases and users.
     - Git follows: a push is recorded as its user's, tokens need the `deploy`
       or `read` scope and reach only their app, a disabled user's keys and
       tokens stop at once.
     - Limits with a growing delay on sign-in (per address and per account), the
       claim code, wrong device codes and SSH handshakes; failures are logged as
       `security:` lines.
     - Minimal pages for all this on the console's hostname (`internal/console`),
       which wave 3 builds on.
     - Verified by tests of the store, the service and the pages; by the route
       enumeration; against the whole server with the API, and with the git and
       ssh command lines as users; by a test of the CLI's whole journey with a
       browser's approval imitated; and with the binary and a real browser on
       macOS, in light and dark.
   - Found by slice 3:
     - The roadmap's "replacing the bootstrap token" would break the quick start
       on one's own computer and `sudo moor`, which the interfaces promise need no
       login. The token stays, as the server's local admin token, and once the
       server has accounts it works only from the server's own machine.
     - A claim URL and browser login need pages before the console exists, so
       minimal ones arrived here rather than in wave 3.
     - New users need a way to set a password without email: one-time links an
       admin sends them. Moorage sends no email.
     - OpenAPI runs the other way from the plan: the document is generated from
       the route table and types, not the types from the document. A generator
       would add a dependency to keep two descriptions aligned; one source
       cannot drift.
     - Reaching the OS keychain from pure Go needs a helper per OS, so CLI tokens
       are in a `0600` file for now.
     - A saved context beat `$MOORAGE_CONFIG`, which names a local server on
       purpose, so a command could reach the wrong server. The explicit setting
       now comes first.
     - Lists changing to pages breaks API clients of 0.1.0. Better now, with the
       contract, than after it.
4. **Containers.** The container driver — one CLI lifecycle for Podman and
   Docker, an observation adapter per engine, label-based adoption — and the
   `dockerfile` and `image` runtimes, with digests pinned at release and
   registry credentials. Runs spike S6.
   - Done in slice 4:
     - `internal/driver/container`: one code path over both command lines. A
       goroutine per container does what a shim does for a native instance: it
       follows the logs, waits, reads the exit status and the OOM flag, applies the
       restart policy and its backoff, and starts the container again. Containers
       outlive the server and are adopted by their labels; each server manages only
       its own.
     - The reconciler asks a small interface of its drivers, so rollouts, health
       checks, the release command and `moor ps` are the same for containers.
     - The `dockerfile` and `image` runtimes; `moor deploy --image`, with a pull
       pinned to the digest; `[container]`, `[resources]` and `[storage]`; the
       `[containers]` section of `server.toml`.
     - `moor registry` and `/api/v1/registries`, with passwords sealed by the
       master key and handed to the engine in a temporary file.
     - `docs/containers.md`, and `examples/container-go`, a guestbook in an image
       with one binary in it.
     - Verified with a job per engine in CI: the driver alone, and whole deploys
       through a server, covering builds, digests, release commands, secrets,
       adoption with no output lost or recorded twice, OOM kills, crashes,
       rollbacks and loopback publishing. Also with the binary and Podman on macOS,
       by hand and through the examples check.
   - Found by slice 4:
     - An adapter per engine for events was not needed: `wait` and `inspect` say
       the same on both, and a container's exit reaches the same code however it
       was seen. Decision D3 now says so. Stats wait for wave 5, so spike S6 is
       open until then.
     - A published port accepts connections before the app in the container
       listens, and closes them: a plain TCP health check called a container
       ready at once. A TCP check of a container now reads briefly as well.
     - Podman's log timestamps have a precision of a second, so `logs --since`
       alone either repeats or loses lines when a follower starts again. The
       driver counts each line of the last second recorded.
     - The example showed that a manifest's command goes to an image's
       entrypoint as arguments: `/guestbook migrate` ran as `/guestbook /guestbook
       migrate`, started the server and never ended. A command now replaces the
       entrypoint.
     - `podman run --rm` reported success for a container removed while it ran, so
       a release command could pass without having finished. One-off containers now
       keep their record until `inspect` has read how they ended.
     - The planned env-file for secrets turned out to be worse than `-e NAME`, with
       the value in the command line's environment: nothing is written, and values
       may span lines. The cost is names the command line reads for itself, which a
       deploy refuses with a clear message.
     - An image's user cannot be known beforehand, so the data directory is
       world-writable, below directories that keep out other users of the server.
     - Podman 4.9, which Ubuntu packages, differs from current releases in two
       ways CI found: `ps` printed no labels through its template, so labels are now
       read with `inspect`; and it does not report an OOM kill when the container
       has an init process. A kill (status 137) of a container with a memory limit
       is logged as perhaps that.
5. **TLS and operations.** `moor trust` (installs the internal CA into the
   OS trust store; certificate issuance itself arrived in wave 1, slice 2);
   `moor server proxy-config nginx`; `moor server doctor` (DNS, ports, TLS, the
   proxy path, the container engine, kernel features); the laptop-mode
   resolver helper. Completes spike S5.
   - Done in slice 5:
     - `moor trust`, by `internal/truststore`, with each system's own tools: the
       keychain on macOS, the certificate store on Windows, the distribution's
       anchors on Linux and the browsers' lists there. It finds the CA in the
       server's files on its own machine, or asks the server, and then wants a
       person or `--fingerprint` to confirm it. `--remove` and `--print` go with it.
     - `moor server proxy-config nginx`, from `internal/config`.
     - `moor server doctor`, in `internal/doctor`, with `--public-url` and
       `/.moorage/proxy-check` to check the path through a proxy.
     - `moor server resolver`, on macOS, with `internal/localdns` behind `listen.dns`.
     - `moor server stop` stops a foreground server, by the PID in the data
       directory's lock, and `--all` stops apps in containers too.
     - `docs/tls.md`, `docs/proxy.md` and `docs/troubleshooting.md`.
     - Verified with: the doctor against proxies that misbehave in each way it
       looks for; tests of trust on a fake machine (no test here changes a real
       store); a CI job per system that changes the real store on a runner it throws
       away, with `moor`, a Go program using that store, as the client; the resolver
       rule on a macOS runner; and a CI job behind a real nginx, with the doctor, a
       deploy and an 8 MB push through it. By hand with the binary on macOS, with
       the examples check using `moor server stop --all` to clean up.
   - Found by slice 5:
     - `moor server stop --all` did not know about containers, which slice 4 left
       running. It does now.
     - With a server tried out on the side, `moor server stop` would have stopped the
       real service. It now acts on a service only if that runs this configuration.
     - A `moor trust` that asks a server is trusting an unauthenticated answer. Beyond
       the fingerprint, it can at least check that the server's own certificate comes
       from the CA it offers, which catches much of what interception looks like.
     - On macOS, removing a trust setting waits for someone to confirm, even as root,
       with no end on a machine with nobody at it: `moor trust --remove` hung in CI.
       It now deletes the certificate first, says if the setting stayed, and gives
       every system tool two minutes. Adding one needs no confirmation as root.
     - The macOS runner resolves `*.localhost` before any rule is added, and macOS
       15.7 here does not, so whether the rule is needed depends on the version. CI
       checks that the system takes up the rule and that the responder answers; only
       a machine that lacks the names shows the rule making the difference.
     - A Windows busy port is `WSAEADDRINUSE`, which `syscall.EADDRINUSE` does not
       match, so neither the doctor nor the server's own message said what was wrong.
     - An engine found by chance and not answering, as Docker is on a Windows runner,
       is a warning: it matters only to apps in containers. One named in `server.toml`
       is a problem.
     - The hosts-file block for Windows needs a server that runs as a service with
       rights to the file, so it waits for the Windows server of wave 7.
     - The doctor warned of 63 GiB free on a large disk, for being under 10%. Space is
       judged by what is free, not by the share.
     - Checks on Firefox on macOS and Windows would need a person: `moor trust` says
       it may keep its own list, and `--print` writes the certificate to import. Real
       browsers are not part of CI: the Go client uses the same system verifier.
6. **Release 0.2.0.** The version, its CHANGELOG section and this roadmap; pull
   request `wave2` → `main`; tag `v0.2.0`; release.
   - Done in slice 6:
     - Version 0.2.0 and its CHANGELOG section, led by what it means to someone
       upgrading; the README and the documentation describe it.
     - `.github/scripts/upgrade-check.sh` and a CI job: a server of the last
       published release, with apps and variables, taken over by this code.
     - A test that every `moor` command the documentation shows exists, with the
       flags it shows.
   - Found by slice 6:
     - That new test found `moor deploy --image` did not exist: slice 4 wrote all of
       it but the flag, its tests went through the client, and trying it by hand
       looked only at the last line of the error. It works now, with a test through
       the command itself.
     - `moor manifest validate`, and the manifest's reference, still said that
       `app.watch` and `app.root` arrive in 0.2.0. Git push did not need them, and
       they come later: `watch` with wave 5.
     - The reference for `moorage.toml` did not mention the `dockerfile` and `image`
       runtimes, or list them under detection.

## 0.3.0: the console (released 2026-11-03)

### Wave 3

1. **Installing.** One line to get `moor`: `install.sh` for macOS and Linux, piped
   from `curl`, and `install.ps1` for Windows 11, piped into `iex`. Each picks the
   file for the computer, checks it against the release's checksums and its own
   version, and only then puts it in place, needing no administrator rights. The
   README and the documentation lead with them.
   - Done in slice 1:
     - `install.sh` (POSIX sh) and `install.ps1` (Windows PowerShell 5.1 and later) at
       the top of the repository: the pattern of checks a release must pass before it
       replaces what is there, and settings through the environment, since a pipe takes
       no options.
     - As root, `install.sh` installs to `/usr/local/bin`, because a system service
       should not run from someone's home directory. `moor server install` runs the
       binary from where it finds it.
     - Upgrading says what it upgraded from, and that a server goes on with the old
       version until it restarts.
     - Verified by tests that run both scripts as they are, against a release in a
       directory with downloads standing in: each thing that can be wrong with a
       download, leaving the installed copy alone. CI runs them for real against the
       published releases: on Linux on x64 and ARM, macOS, Windows on x64 and Windows 11
       on ARM, in both PowerShells, as root, and upgrading under a running server.
   - Found by slice 1:
     - An app's supervisors keep `moor.exe` open on Windows, so it cannot be replaced,
       or stopped first, as a program with no such helpers can. It can be renamed: the
       installer moves it aside, and a later run clears it away.
     - A step's `shell` in a workflow cannot read the matrix, which made GitHub reject
       the whole file with no jobs at all. The job's `defaults` can.
     - Windows PowerShell started from PowerShell 7 inherits a module path where it
       cannot find `Get-FileHash`. The tests start it that way, and a person could. The
       installer computes the hash with .NET then.
     - Until a release follows 0.2.0, CI on Windows can only show moving a running file
       aside for a copy of the same version.
2. **Console foundation.** Layout and CSS design tokens (light and dark),
   templates, the first-run wizard, grown from the account pages of wave 2,
   slice 3 (sign-in, the claim, CSRF-protected forms), the event
   bus with one SSE stream, the activity timeline, the command palette.
   - Done in slice 2:
     - `internal/events`: the bus and its store, with 90 days' retention, and what
       publishes: deployments, releases, changes of variables, instances that exit,
       sicken, recover or are replaced, and the server starting.
     - `GET /api/v1/events`, `GET /api/v1/events/stream` as server-sent events that
       catch up after a reconnection, and `moor events [-f]`.
     - The console's frame: templates and fingerprinted assets embedded in the binary,
       CSS design tokens with light, dark and a manual choice, phone width, and a
       strict CSP with no inline scripts or styles, which the account pages keep too.
     - Pages: the apps list, Activity, and Getting started; the command palette;
       copying commands; times that stay true; live updates over one stream.
     - `moor server run --dev`, which reads the console from the source tree.
     - Verified by tests of the pages (that each keeps to the policy, and that a signed-out
       visitor is sent to sign in), of the bus, and of the stream through a whole server:
       replay, restricted tokens, and a server stopping with followers. And in a real
       browser against a real server, in both themes and at phone width, with events
       arriving live and no console errors. WCAG AA contrast was computed for the text
       colours on their surfaces, in both themes; a screen reader was not tried.
   - Found by slice 2:
     - The wizard was to set the domain and TLS. They are in `server.toml`, chosen by
       `moor server init`, and the server needs them before anyone can reach the page. It
       shows them, with the doctor's checks, and says how to fix what is wrong.
     - The console needed the doctor, which needed the server, which serves the console.
       The data directory's layout moved to `internal/layout`, below all three.
     - A deployment's "started" comes a moment after its "queued", and made the timeline
       three lines per deployment. It is now transient: for those following now, not kept.
     - Over plain HTTP, a browser allows six connections to a server, and each page
       following events holds one, so a seventh tab would hang. A page out of sight lets
       its connection go, and catches up when it returns.
     - Security events (sign-ins, tokens) wait for the audit log of wave 4: until roles
       arrive, this feed is one that every member can read.
3. **Apps and deployments.** The dashboard; the app overview with
   environments and instances; deployments with the live stage timeline and
   log, the resolved manifest and the release diff; deploy, rollback,
   redeploy, restart and scale actions.
   - Done in slice 3:
     - The apps list says how each app stands: healthy, how many of how many, down,
       deploying, scaled to zero, and a failed deployment behind a release that keeps
       serving. An app's page has its instances, deployments and activity. A deployment's
       page has its stages, read from the log's own markers, the log as it is written, the
       resolved manifest and what changed since the release before.
     - The parts of a page that change are drawn again by the server, when an event or a
       poll says so, so that one place knows how they look; a part someone is typing in
       stays as it is. Without scripts, every form works and a deployment under way
       reloads itself.
     - Deploy (a `.tar.gz` of source or an image's name, from the page), rollback, restart,
       redeploy and scale, each a form with the session's token, doing what the API does:
       the work moved from the API's handlers to the deployer, so that there is one of it.
     - `moor scale` and `PATCH /apps/{app}/envs/{env}`; `moor redeploy` and its route;
       `moor releases show` and `diff`, and their routes. The new-app flow is a name, then
       the ways to deploy, waiting for the deployment that creates it.
     - Verified with real instances from the console's pages (scaling up and down, a scale
       that fails and goes back, rollback, redeploy, upload), a real push, Podman for an
       image; and in a real browser against a real server, live and at phone width.
   - Found by slice 3:
     - Scaling was planned for wave 5, and nothing had it: the console's Scale button
       needed it, so it is here. Workers already ran; a number that cannot be met goes back,
       or the reconciler would retry it for ever.
     - A build changes its source in place, so nothing could be rebuilt from one. An upload
       is now kept beside its build, as is an image's manifest, whose build otherwise keeps
       nothing. Builds from before this version say that they cannot be redeployed.
     - Rollbacks and changes of variables wrote no log, which left their pages empty. They
       do now.
     - Without labels, the instances table was a column of bare numbers at phone width.
     - Not yet: canceling a deployment and retrying a failed one (the deployer cannot stop
       a build that has started, and has no way to rebuild but from what serves); restarting
       one process; handing a process back to the manifest's number, other than by setting
       it. Environment switching and `promote` wait for wave 4, and the dashboard's host
       strip and sparklines for wave 5's metrics.
     - Safari and Firefox are still untested, and a screen reader untried.
4. **Logs, variables and settings.** The live log viewer (instances, sources,
   search, time range, download); variables with secret, source and override
   badges and bulk edit; account pages (SSH keys, tokens, sessions); server
   settings; the System page with the `doctor` results.
   - Done in slice 4:
     - The logs page, on a reader (`internal/applogs`) shared with the API and `moor logs`:
       instances, builds, the access log and events, merged by time and followed, narrowed
       by process, instance, source, text, time and problems. JSON lines read as their
       message and fields, and lines that name a level are coloured. Pause, wrap, older
       lines, and a download of up to 200,000 lines.
     - The variables page, with where each comes from and what overrides what, saving
       without releasing, and a banner while something is unreleased. Editing as text shows
       what would change, and only then does it; a secret stays masked, and keeps its value.
     - The account pages: password (`moor password`, ending other sessions), SSH keys, API
       tokens and sessions. Settings for administrators: what `server.toml` says, users,
       registries, and the System page with what `moor server doctor` finds, also in the
       API. Keys, tokens and passwords moved to one place in `internal/auth`, for the API
       and the pages alike.
     - A dialog of the console's own asks before what cannot be undone.
     - Verified from the pages against a real server, with real instances running: every
       form, every refusal, a member kept out of the settings, secrets never on a page. In
       a real browser with an app logging structured and plain lines.
   - Found by slice 4:
     - Every release left a log file per instance, for ever, and the log reader reads them
       all. The logs of stopped instances are now removed after 14 days, as designed, which
       had not been built.
     - Server settings live in `server.toml`, read before the database opens, and nothing at
       run time needs setting yet: the page shows them, and says how to change them.
     - Not yet: revealing a secret (with the audit log, wave 4); a level filter beyond
       "only problems"; the logs of one-off runs and cron (with them, wave 5); and deleting
       or renaming an app, which the design has and no slice did: added to wave 4.
5. **Release 0.3.0.** The version, its CHANGELOG section and this roadmap; pull request
   `wave3` → `main`; tag `v0.3.0`; release.
   - Done in slice 5:
     - Version 0.3.0 and its CHANGELOG section, led by what it means to someone upgrading;
       the README and the documentation describe it, with pictures of the console.
     - `.github/scripts/console-check.sh` and a CI job: a real server with a real app, and
       the console driven as a person would in Chromium, Firefox and WebKit, at desktop and
       phone width. Anything a page complains of fails it, above all what the
       Content-Security-Policy refuses.
   - Found by slice 5:
     - All three browsers passed as they were, which closes what slices 2 and 3 left open.
       A screen reader is still untried.
     - WebKit does not resolve `*.localhost` by itself, as Chromium and Firefox do, so the
       check gives it `localhost` as the console's address. That is what
       `moor server resolver` is for on a Mac.
     - From here, a release goes through without stopping to ask: pull request, merge once
       CI is green, tag, and publish the draft once its binaries are there.

## 0.4.0: previews and sharing (planned)

### Wave 4

1. **A home of its own.** `~/.moorage` by default for a server set up as oneself on macOS
   and Linux, and for `moor`'s configuration; existing servers stay, and are found.
   - Done in slice 1:
     - One folder, the same on both, with `XDG_CONFIG_HOME` and `XDG_DATA_HOME` winning on
       Linux if they are set. `moor` looks where earlier versions put things when
       `~/.moorage` has no configuration, so nothing moves under anyone.
     - The examples check now runs under a path with a space, as the system-wide location on
       macOS has.
   - Found by slice 1:
     - Nothing had ever built under a path with a space, which the default on macOS was. With
       real toolchains, Go, Python with uv, Node.js, a static site and a container all build
       and run under one, so the system-wide location on macOS can stay. The check keeps
       the space, so that this stays true.
2. **Projects and roles.** Projects; viewer, deployer and maintainer roles
   enforced in the API; the audit log in the API, CLI and console.
   - Done in slice 2:
     - Projects, with members, and viewer, deployer and maintainer roles; a role for everyone
       else, or none; whether viewers read logs. Every existing app goes in a `default`
       project where everyone is a maintainer, so that an upgrade changes nothing.
     - One place decides (`auth.Service.AppAccess`), and the API's guard, the console's forms
       and pages, and git over SSH and HTTPS all ask it. Those with no role cannot tell an
       app exists: 404s, and lists, feeds and the palette leave it out. A token's scope still
       narrows its owner's role.
     - The audit log: every change through the API or the console, sign-ins, pushes and
       secrets revealed, kept for a year, searched and exported from the API, `moor audit` and
       Settings. Nothing of what was sent goes in it. Revealing a secret, which waited for
       it, is here: `moor env reveal`, the API, and a button for maintainers.
     - `moor projects`, `moor apps create` and `moor apps move`, with their routes; the
       Projects pages; New app asks which project.
     - Verified with someone going through each role in the pages, with real instances, and
       every route as each kind of caller.
   - Found by slice 2:
     - The palette offered links that a role would refuse. It asks now.
     - A first deployment makes its app in `default`, and with no role there it is refused:
       an app elsewhere has to be made first, with `moor apps create --project`.
     - Not yet: limits and quotas per project; moving an app in the console, which comes with
       its settings in the next slice; two-factor sign-in, and single sign-on.
3. **Apps' lifecycle.** Renaming and deleting an app, from the console's danger
   zone and with `moor apps rename` and `delete`: its instances, builds, logs and
   repository go, and what it keeps in its data directory is said first.
   - Done in slice 3:
     - `moor apps rename`, `delete` and `info`, their routes, and an app's Settings page, with
       moving it to another project. Renaming moves its builds, data, logs and repository, the
       paths the store holds and its events, and starts it again under the new name, where a
       rollback still finds the builds from before. Deleting waits for no deployment, says how
       much each part takes, and asks for the app's name. A later app of that name starts
       afresh, with the data directory if it was kept.
     - Verified with real instances: their processes gone, their files gone, and a new app
       of the same name owing nothing to the old one.
   - Found by slice 3:
     - The router keeps a static site by its build's ID, and a rename moves it: the site then
       could not read its files. The router now lets go of them.
     - A rename is not without downtime: an instance's paths are in its environment, so it
       stops before its files move. The address changes anyway, so the page says so.
4. **Previews and environments.** Branch previews (naming, inheritance, TTL,
   pinning, limits, destroyed with their branch), named environments,
   `promote`, deploy locks, the Previews page.
   - Done in slice 4: a pushed branch gets its own preview at `<app>--<env>`,
     with production's variables except those marked `--production-only`, its own
     data, `noindex`, and an end when its branch goes or after `[previews] ttl` of
     no deployment or request; pinning, a limit of ten; named environments from
     `[environments.NAME]`, `moor environments create` or `moor deploy --env`;
     `moor promote`, with a warning if build variables differ; `moor lock`; roles
     on all of them; an **Environments** tab, a **Previews** page and an
     environment chosen on every page of an app.
   - Found by slice 4: a lock refuses deployments, where the design had them
     built and held until unlocking. Holding needs a further state of a
     deployment and something to release what is held, and git keeps the commit
     anyway, so the design now says what was built. A glob's `*` must cross
     slashes, since branches have them, so `path.Match` would not do. A deployer
     can destroy a preview by deleting its branch, so the console and the API let
     them do so directly; removing a named environment still takes a maintainer.
5. **Sleep and wake.** Idle sleep with the waking-up page; per-process
   opt-out.
   - Done in slice 5: an environment idle for `sleep_after` stops its instances,
     unless a request is going on, and a process with `sleep = false` stays up.
     A request wakes it and waits: two seconds for a browser, then a page that
     reloads itself; 30 seconds for anything else, then a `503` with `Retry-After`.
     `moor sleep`, `moor wake`, buttons and a status in the console. Previews sleep
     after 30 minutes by default; other environments only when they say so.
   - Found by slice 5: an environment must keep saying it is asleep until it has
     finished waking, or a request arriving meanwhile finds no instance, and is told
     the app is down. A release must wake it without the separate step, since the
     release holds the environment's lock. Idle time must count from the end of a
     deployment, not its start, or a slow build is followed at once by sleep. Those
     waiting are not those who began the wake: it goes on if they leave.
6. **Access gates.** Password, sign-in (with identity headers), share links,
   IP allow-lists, default gates per environment kind, `noindex` outside
   production; an independent review of the gate, cookie and CSRF design.
7. **Paths and domains.** Path prefixes and path mode; custom domains with
   DNS verification.
8. **Release 0.4.0.**

## 0.5.0: real apps, observed (planned)

### Wave 5

1. **Processes.** Cron jobs, one-off runs (`moor run`), restarting one process,
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
| S1 | Can we receive `git push` in-process over SSH and HTTP and stream live progress back to the client? | Wave 2, slice 2 | *Largely shown by a research probe with the git CLI. Slice 2: a failed deploy rejects the push with the deployment's ID (the hook refuses every ref of a push); the end of the pack is found by Moorage, so a client that keeps its side open gets its answer (tested with such a client, and with the library's own client, which closes its side); ref updates serialised per repository, with a second push to a deploying branch refused; the library behind `internal/gitserver`, checked by a test. Done.* |
| S2 | Do shims keep apps alive across daemon restarts on all three OSes, without orphans? | Wave 1, slice 3 (and service installation in slice 6) | `systemctl restart` (with `KillMode=process` and delegation), a launchd restart and a Windows service restart under load drop no requests; killing a shim kills its app on every OS; shim RSS measured (target ≤ 5 MB). *Slice 3: apps outlive the server and are adopted (tests, and the real binary with a Python app); a killed shim leaves no app behind on any OS (tests); footprint 6.2 MB on macOS. Slice 6: through `systemctl restart` and a launchd restart under load, the app keeps its process and none of its requests fail; shim private memory 4.4–5.3 MB on Linux. The Windows service comes with the Windows server (wave 7).* |
| S3 | Do in-place builds work for uv/Python, Node and Go, including partial toolchain versions? | Wave 1, slice 4 | Sample apps build and run from their final directory; toolchain downloads verified. *Shown in slice 4 on macOS: seven sample apps built from source, deployed and rolled back, with partial versions resolved and every download verified (a CI job runs them on Linux and macOS); the Node.js checksum file's signature is checked in wave 6.* |
| S4 | Is the routing table swap really zero-downtime? | Wave 1, slices 2–3 | Rolling replacement under 200 req/s load with zero failed requests; WebSockets pass through. *Static sites shown in slice 2 (2,046 requests across 12 swaps, none failed); proxied instances in slice 3 (over 10,000 requests across a deploy, a rolling restart and a rollback, none failed; upgraded connections pass through).* |
| S5 | Does the internal CA work end to end — an auto-generated root and a provided intermediate — for browsers and CLI tools? | Wave 1, slice 2 (issuance), wave 2, slice 5 (`moor trust`) | Certificates issued on demand for routed hostnames only, renewed, trusted after `moor trust` or through the corporate chain. *Slice 2: issuance and renewal, with both a generated root and an intermediate chain. Slice 5: `moor trust` changes the real store on Linux, macOS and Windows runners, after which `moor`, using that store through each system's verifier, accepts the certificates (curl too on Linux), and `--remove` undoes it. Browsers themselves were not driven: the store and the verifier are theirs on macOS and Windows.* |
| S6 | Container driver: shared CLI lifecycle plus per-engine observation adapters. | Wave 2, slice 4 (stats: wave 5) | Run, label-based adoption, exit and OOM events, cgroup-based stats on Linux and REST-based stats elsewhere, `logs --since`, rootless loopback port publishing — on current Podman and Docker releases. *Slice 4: all but stats, with both engines in CI and Podman on macOS, by one code path: exits and OOM kills come from `wait` and `inspect`, not per-engine events (decision D3). Stats, with their adapters, remain for the metrics of wave 5.* |
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
| Shim memory per instance | Many small apps on one VM | Measured in wave 1: 4.4–5.3 MB private on Linux, 6.2 MB on macOS (target ≤ 5 MB); fallback to one shim per environment |
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
