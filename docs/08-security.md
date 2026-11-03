# 8. Security

## Threat model

Moorage is an **internal platform for trusted teams** that also exposes apps
to people outside the team (customers, reviewers). The design protects the
platform and its users from the internet, protects apps' secrets and data
from accidents and from each other where the OS makes that practical, and is
explicit about what it does not protect against.

| Actor | Trust | Notes |
|---|---|---|
| Admin | Full | Manages the server, users and settings. |
| Maintainer / deployer | High | **Deploying is running code on the host** (as an unprivileged app user). Grant it like shell access. |
| Viewer | Medium | Read-only access to status, metrics and (optionally) logs. |
| App visitor | Untrusted | Anyone who can reach the server — initially colleagues on the internal network; later, possibly customers and other outside reviewers. |
| App code and its dependencies | Semi-trusted | May be buggy or compromised through a dependency. |

**Trust boundaries:** internet ↔ ingress; users ↔ API/console/git;
app ↔ app and app ↔ platform; build ↔ host; Moorage ↔ download sources.

**Explicit non-goal:** containing a malicious deployer. Hostile multi-tenant
hosting needs VM-level isolation and is out of scope.

## Daemon privileges

- **Linux (recommended): root, dropping privileges for all user code.** Root
  is needed to switch UIDs per app, manage the cgroup subtree and bind low
  ports. The daemon never executes repository code as root: every build, app,
  cron and one-off process runs under an app or build UID, set up by the shim
  before `exec`. The shipped systemd unit applies only hardening options that
  are compatible with this design, and documents each one: apps and the
  container engine run inside the service's mount namespace, so filesystem
  options (read-only system directories, private `/tmp`) apply to them too,
  and options that make the cgroup tree read-only would break the delegated
  subtree the daemon manages.
- **Linux unprivileged mode:** the daemon runs as a `moorage` user with
  `CAP_NET_BIND_SERVICE`. Simpler, but every app runs as that same user
  (isolation level `none`).
- **macOS / Windows:** a dedicated service account for servers; the logged-in
  user in laptop mode.

## Authentication

- **Local accounts:** email + password hashed with Argon2id; minimum length
  12; an embedded list of common passwords is rejected.
- **First-run claim:** `moor server init` prints a one-time setup URL. The
  console's first-run page requires it, so nobody can race to claim a freshly
  installed, network-reachable server.
- **Sessions:** 256-bit random IDs stored hashed; idle and absolute timeouts;
  users can see and revoke their sessions.
- **CLI login:** `moor login <url>` opens the console in a browser to approve
  the device; the CLI then receives a token. No password is typed into the
  terminal; pasting a token is the fallback for headless machines.
- **API tokens:** `moor_` prefix + 32 random bytes (the prefix lets secret
  scanners spot leaked tokens), stored as SHA-256 hashes, with scopes (`read`,
  `deploy`, `admin`, `gate`), optional app restriction, optional expiry and
  last-used tracking.
- **SSH keys:** per user, Ed25519/ECDSA/RSA ≥ 3072 bits.
- **Brute-force protection:** per-IP and per-account rate limits with backoff
  on sign-in, gate passwords, share-link redemption and SSH authentication;
  failures are logged as security events.
- **Later phases:** TOTP two-factor and passkeys; OIDC single sign-on with
  group-to-role mapping, just-in-time provisioning and an option to disable
  local passwords.

## Authorisation

Apps belong to exactly one **project**. Small installations can use a single
default project that everyone belongs to.

| Capability | Viewer | Deployer | Maintainer | Admin |
|---|:-:|:-:|:-:|:-:|
| See apps, environments, status, metrics | ✓ | ✓ | ✓ | ✓ |
| Read logs | ✓¹ | ✓ | ✓ | ✓ |
| Deploy, restart, roll back, promote, create previews and share links | | ✓ | ✓ | ✓ |
| Scale within project limits; see variable names and non-secret values | | ✓ | ✓ | ✓ |
| Manage variables and secrets, domains, gates, resource limits, members; delete apps | | | ✓ | ✓ |
| Server settings, users, all projects, audit log | | | | ✓ |

¹ Configurable per project, because logs can contain sensitive data.

Authorisation is enforced in the API layer only (the console just hides what
you cannot do). Every route declares its required permission, and a test
enumerates all routes to guarantee none is missing.

## Secrets

- Variables flagged **secret** are encrypted at rest with AES-256-GCM using
  per-value nonces, with *(app, environment, name)* as associated data so
  ciphertexts cannot be swapped between variables.
- Envelope encryption: data keys are wrapped by a master key kept in
  `<data>/keys/master.key` (`0600`) by default, or supplied through an
  environment variable, a file, or systemd encrypted credentials. External KMS
  support is a later addition. `moor server rotate-key` re-wraps data keys.
- Secret values are write-only by default (masked in the console and API);
  maintainers can reveal a value, which is audited.
- Never logged; redacted from app logs at capture time (best effort); not
  exposed to builds unless explicitly flagged; passed to processes without
  appearing on command lines.
- Database backups contain only ciphertext — **the master key must be backed
  up separately**, and the backup tooling says so.
- Gate passwords (Argon2id), share-link tokens, session IDs and API tokens are
  stored only as hashes.

## Web security (console and API)

- The console has **its own hostname**; its cookie is host-only and never
  shared with app subdomains (see [Ingress → Cookie rules](06-ingress.md#cookie-rules-important)).
- Strict Content-Security-Policy (`script-src 'self'`), `frame-ancestors
  'none'`, `X-Content-Type-Options`, `Referrer-Policy`.
- **CSRF:** cookie-authenticated mutating requests require a CSRF token bound
  to the session *and* a same-origin `Origin`/`Sec-Fetch-Site`. Bearer-token
  requests are exempt. CORS is off by default.
- All SQL is parameterised; all input is validated against the OpenAPI schema.
- Tokens are **never accepted in query strings** (they leak into logs and
  browser history); browser streams use the session cookie, the CLI sends a
  bearer header.
- **Behind a TLS proxy**, forwarded headers and the PROXY protocol are trusted
  only from the configured proxy addresses, and cookies stay `Secure` because
  the browser still sees HTTPS. Plain HTTP *end to end* is meant for laptop
  mode on `localhost`; anywhere else the console shows a persistent warning
  that credentials travel unencrypted.
- **SSRF:** features that fetch URLs (webhooks, remote repositories, toolchain
  mirrors) refuse loopback, link-local and metadata addresses unless an admin
  allows specific ranges.

## App isolation

See [Runtime → Isolation levels](05-runtime.md#isolation-levels). Key points:

- Apps listen on loopback; only the ingress is exposed.
- With isolation `none`, all apps run as the same user and **can read each
  other's environment variables** (e.g. through `/proc/<pid>/environ` on
  Linux) and data. The console flags this. Use `user` (or `sandbox`) on any
  shared server.
- Containers: rootless where possible, `no-new-privileges`, no host network,
  no privileged mode or host mounts unless an admin enables them for a
  specific app.

## Build security

- Builds execute dependency code (install scripts, code generators), so they
  run with the same isolation as apps, under a separate build UID, with a
  timeout and resource limits, and without runtime secrets unless flagged.
- Shared build caches are acceptable under the trusted-team model; the
  `sandbox` level switches to per-app caches to rule out cache poisoning
  between apps.
- Repository hooks are never executed. The SSH server accepts only the git
  pack protocol commands for known repositories — no shell, no forwarding.

## Supply chain

- Toolchains are downloaded over HTTPS and verified against the provider's
  published checksums (and signatures where available) before use; exact
  versions are cached and never silently replaced.
- Moorage releases are signed and checksummed and ship with an SBOM;
  `moor server upgrade` verifies both before swapping the binary.
- Few, pure-Go dependencies; `govulncheck` and dependency updates in CI.

## Network exposure

- Public ports: 80, 443 and the SSH git port. Apps are loopback-only. The
  admin socket is a local UNIX socket readable only by the service user.
- `moor server doctor` and the console warn about app processes listening on
  non-loopback addresses.

## Audit log

Every mutating API call, sign-in (successful or not), privileged token use,
secret reveal and git push is recorded with time, actor (user or token), IP
address, action, target, result and metadata. Retained for a year by default;
exportable as JSON lines; visible to admins.

## Backups and recovery (Phase 3)

`moor server backup` produces a consistent snapshot — the SQLite database
(online backup), repositories, data directories, certificates and, optionally,
keys — to a local path or S3-compatible storage, on demand or on a schedule.
`moor server restore` reverses it. Restores are tested in CI.

## Operator hardening checklist

- Use isolation level `user` or `sandbox` on shared Linux servers.
- For TLS, prefer an intermediate CA issued (and name-constrained to the base
  domain) by your corporate CA, or a corporate wildcard certificate, over a
  self-generated root that every user must trust by hand.
- Keep the console hostname distinct from app hostnames (the default).
- Behind a TLS proxy, bind Moorage's HTTP listener to loopback or a private
  interface and list only the proxy's addresses as trusted.
- Restrict `admin` to a few people; give most users `deployer`.
- Default previews to the `login` gate.
- Back up the master key separately from data backups.
- Firewall everything except 80, 443 and the SSH git port.
