# PocketCloud

Self-hosted control panel for a homelab or a few VPS: servers, Docker containers and Compose stacks, shared databases, static and PHP websites with automatic DNS and TLS, backups and a full change audit – all from one web UI.

PocketCloud is a single Go binary built on [PocketBase](https://pocketbase.io) (embedded as a library) with a React UI. Every managed server runs a small **agent container** that dials out to the panel over gRPC with mutual TLS, so servers behind NAT need no open ports. Agent updates are done by a short-lived updater container with automatic rollback.

> **Status: design phase.** This repository holds the UI design, the architecture notes and the backlog. No code yet – start with the [Roadmap](https://github.com/oldbear24/PocketServerManager/issues/40).

<img src="design/screenshots/01-overview.png" width="860" alt="PocketCloud overview screen">

## What it will do

- **Servers and agents** – connect a Docker host with one `docker run` and a one-time token; live heartbeat, resource usage and events; one-click agent updates through a temporary updater container that rolls back if the new agent does not come up.
- **Containers and stacks** – Docker inventory and actions, Compose editor with revisions and encrypted secrets, live logs, web terminal.
- **Static sites** – deploy from Git, a ZIP upload or an empty placeholder; builds run in a throwaway container; atomic releases and instant rollback.
- **PHP sites** – WordPress, Laravel or Nette in an isolated PHP-FPM container per site behind the same edge proxy; PHP 8.2–8.5, Composer builds, atomic releases, persistent data outside releases, cron, outgoing mail through a relay.
- **Databases** – shared MariaDB and PostgreSQL servers per host, so apps don't each need their own instance; one database, one user and one private network per app, password rotation, Adminer on demand, per-database backups.
- **Domains and TLS** – connect DNS providers (Cloudflare first); records and certificates (Let's Encrypt or ZeroSSL, HTTP-01 or DNS-01, wildcards) are created, renewed and pushed to the servers automatically. Containers get a domain through a `pocketcloud.domain` label.
- **Backups** – restic jobs for volumes, folders and per-database dumps to S3/B2, SFTP or a backup server; retention, integrity checks, restore tests, and a backup of the panel itself.
- **Overview and alerts** – what needs attention, the command queue, notifications via e-mail, ntfy, Gotify or webhooks.
- **Diagnostics** – a PocketBase plugin with live gRPC connection health per agent, a message trace, agent self-checks, debug bundles and an optional Prometheus endpoint.
- **Change audit** – every configuration change from the UI, the API or the PocketBase dashboard with author, source and field-level diff; append-only, hash-chained, exportable, with one-click revert.
- **Import from Coolify** – read a Coolify installation into a reviewed plan and move it in place (reusing the volumes) or onto a new server, with rollback.

## Architecture

<img src="docs/diagrams/overview.png" width="994" alt="Architecture overview: the browser talks to the panel, agents on each server dial out to the panel over gRPC with mTLS">

Every action becomes a record in the `commands` collection – the command history and the delivery queue at once – and every configuration change lands in an append-only audit log. Logs and terminal sessions stream agent → panel → browser without touching the database.

- [docs/architecture.md](docs/architecture.md) – components, enrollment, gRPC API, command lifecycle, sites/DNS/TLS flow, PHP sites, database servers, backups, diagnostics, change audit, security model
- [docs/data-model.md](docs/data-model.md) – proposed PocketBase collections
- [docs/migration-from-coolify.md](docs/migration-from-coolify.md) – how a Coolify installation moves to PocketCloud, by hand or with the importer

## Design

Thirteen screens: overview, server detail, Compose stack, static site, new-site wizard (static or PHP), domains & certificates, backups, connecting a server, diagnostics, change audit, PHP site, databases, import from Coolify. See [design/README.md](design/README.md) for screenshots, clickable static previews and the canvas sources.

## Roadmap

| Milestone | Focus |
|---|---|
| [M0 · Foundations](https://github.com/oldbear24/PocketServerManager/milestone/1) | repo and CI, gRPC contract, PocketBase schema, change audit, panel and UI skeletons, key technical decisions |
| [M1 · Agent & connectivity](https://github.com/oldbear24/PocketServerManager/milestone/2) | internal CA and mTLS, agent container, enrollment, command queue, heartbeats, updates, diagnostics plugin, audit page |
| [M2 · Containers, stacks & databases](https://github.com/oldbear24/PocketServerManager/milestone/3) | inventory and actions, Compose stacks, live logs, metrics, terminal, server page, shared database servers |
| [M3 · Sites, domains & TLS](https://github.com/oldbear24/PocketServerManager/milestone/4) | edge proxy, static and PHP sites, DNS providers, ACME, new-site wizard, scheduled tasks, container and LAN domains |
| [M4 · Backups](https://github.com/oldbear24/PocketServerManager/milestone/5) | targets and jobs, retention, restore, integrity checks, per-database backups, panel backup |
| [M5 · Overview, alerts & hardening](https://github.com/oldbear24/PocketServerManager/milestone/6) | dashboard, alerts, search, roles and API tokens, security review, i18n, install guide |
| [M6 · Import from Coolify](https://github.com/oldbear24/PocketServerManager/milestone/7) | migration plan from a Coolify installation, in-place cutover or move to a new server, rollback |

The [Roadmap issue](https://github.com/oldbear24/PocketServerManager/issues/40) tracks every issue by milestone.

## Repository layout

```
cmd/panel     panel binary (PocketBase + gRPC gateway + UI)      – planned
cmd/agent     agent (runs as a container)                         – planned
cmd/updater   temporary container that swaps the agent on update  – planned
proto/        gRPC API (buf)                                      – planned
images/php/   PHP-FPM runtime images, one tag per PHP version     – planned
plugins/      PocketBase plugins, e.g. diagnostics                – planned
internal/     shared Go packages                                  – planned
web/          React UI (Vite + TypeScript)                        – planned
design/       UI design: screenshots, static previews, sources
docs/         architecture and data model
```

## License

Not chosen yet.
