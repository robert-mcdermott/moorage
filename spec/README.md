# Moorage Design Documentation

Moorage is a self-hosted, single-binary application deployment platform:
push code, get a URL, watch it run. These documents describe the design of
the first versions, the roadmap that schedules them, and the decisions behind
them. To install and use Moorage, read the
[documentation](../docs/README.md).

| # | Document | What it covers |
|---|---|---|
| 1 | [Vision, goals and principles](01-vision.md) | What Moorage is for, who uses it, non-goals, principles, glossary |
| 2 | [Architecture](02-architecture.md) | Components, process model (daemon + shims), reconciliation, on-disk layout, cross-platform matrix, code layout, dependencies |
| 3 | [App manifest — `moorage.toml`](03-manifest.md) | The repository file that tells Moorage how to build and run an app; detection rules |
| 4 | [Build and deploy](04-build-and-deploy.md) | Deployment sources, the built-in git server, the build pipeline, toolchains, zero-downtime rollouts, rollback, previews |
| 5 | [Runtime](05-runtime.md) | Drivers, command execution, environment variables, ports, health, restarts, limits, isolation, sleep/wake, cron |
| 6 | [Ingress, routing and TLS](06-ingress.md) | Hostnames, path routing, proxying, certificates, access gates, platform pages, static serving |
| 7 | [Observability](07-observability.md) | Status model, logs, metrics, events, notifications, diagnostics |
| 8 | [Security](08-security.md) | Threat model, privileges, authentication, authorisation, secrets, isolation, supply chain, audit |
| 9 | [Interfaces](09-interfaces.md) | CLI command tree, REST API conventions and resources, web console |
| 10 | [Data model](10-data-model.md) | SQLite schema, identifiers, files on disk, sizing |
| 11 | [Roadmap](11-roadmap.md) | Waves, slices and releases; spikes; risks; engineering practices |
| 12 | [Decisions and open questions](12-decisions.md) | Key design decisions with alternatives, and questions still open |

## Reading order

- **Evaluating the idea:** 1 → 2 → 11.
- **Writing an app for Moorage:** 3 → 5 (environment variables, health checks).
- **Operating a server:** 2 → 6 → 8 → 7.
- **Building Moorage:** all of them, then 12.

## Conventions

- *(Phase N)* marks features that are designed but scheduled after the first
  releases; the [roadmap](11-roadmap.md) maps phases onto waves and releases.
- Example domains use `apps.example.com`; example app names are fictional.
