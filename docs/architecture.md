# Architecture

> Status: proposal. Decisions marked **(spike)** are settled in the spike issue in milestone M0 and recorded as ADRs in `docs/adr/`.
>
> Diagrams are PNG renders of the Mermaid sources in [`diagrams/`](diagrams/), so they show up everywhere, including the GitHub mobile app. Edit the `.mmd` file and run [`diagrams/render.sh`](diagrams/render.sh).

PocketCloud has two binaries:

- **Panel** – one Go process: [PocketBase](https://pocketbase.io) embedded as a library (auth, collections, REST + realtime API, files, cron), the React UI, a gRPC gateway for agents, and the managers for commands, DNS, certificates and backups.
- **Agent** – a small container (`pocketcloud-agent`) on every managed server. It dials out to the panel, executes commands (Docker, Compose, edge proxy, site deploys, PHP and database containers, restic) and streams data back. It never replaces itself: updates are done by a short-lived **updater** container (`pocketcloud-updater`).

<img src="diagrams/components.png" width="1036" alt="Panel and managed server components and how they connect">

## Principles

1. **Agents dial out.** The panel never opens connections to servers. Servers behind NAT work and need no inbound ports (except 80/443 on servers with the `edge` role).
2. **The panel is the source of truth.** Agents keep only their identity and what they need to keep serving when the panel is down (edge config, certificates).
3. **Every action is a command.** UI actions and scheduled jobs create records in `commands`. The collection is both the command history and the delivery queue. Delivery is at-least-once; agents deduplicate by command id.
4. **Streams bypass the database.** Logs, terminal sessions, admin tool sessions and command output are relayed agent → panel → browser and never stored in PocketBase.
5. **Secrets are encrypted at rest** with a master key kept outside the database, and are sent to an agent only inside the command that needs them.
6. **Everything on a server is a container.** The agent, the edge proxy, PHP runtimes, database servers, site builds, backup and restore jobs and the updater all run in Docker; the agent never updates itself in place.
7. **Every change is audited.** Configuration changes – from the UI, the API, the scheduler or the PocketBase dashboard – are written to an append-only, hash-chained audit log with field-level diffs.

## Ports

| Component | Port | Purpose |
|---|---|---|
| Panel | 443 (or behind a reverse proxy) | UI and REST/realtime API |
| Panel | 9443 | gRPC gateway for agents (mTLS) |
| Agent container | – | no inbound ports |
| Edge container (`edge` role) | 80, 443 | static and PHP sites, exposed containers |
| PHP-FPM and database containers | – | internal Docker networks only |

## Agent container

The agent ships as a multi-arch image (`ghcr.io/oldbear24/pocketcloud-agent`, linux/amd64 and linux/arm64) with the agent binary, the Docker CLI with the Compose plugin, and restic. It is started once with `docker run` (or a Compose snippet) and managed by PocketCloud afterwards.

| Mount | Mode | Why |
|---|---|---|
| `/var/run/docker.sock` | read-write | manage containers; start edge, job and updater containers |
| volume `pocketcloud-agent` → `/var/lib/pocketcloud` | read-write | identity (key, certificate), config, idempotency state, updater results |
| `/` → `/host` | read-only, `rslave` | host metrics (`/host/proc`, `/host/sys`), disk usage, reading host paths for backups |

- First start reads `PANEL`, `CA_PIN`, `TOKEN`, `NAME` and `ROLES` from the environment, enrolls, and stores its identity in the volume. The token is single use, so leaving it in the container environment is harmless.
- The image `HEALTHCHECK` reports healthy only while the agent is connected to the panel; the updater relies on it.
- The agent labels everything it runs with `pocketcloud.role` (`agent`, `updater`, `edge`, `job`) so the inventory can recognise and protect these containers.
- Work that needs more access than the read-only host mount – writing restored files, building sites, backing up a volume – runs in short-lived **job containers** that mount exactly what they need.

## Agent updates

A container cannot safely replace itself: stopping its own container kills the process halfway through. The agent therefore starts a tiny, purpose-built `pocketcloud-updater` container (a static Go binary on `scratch`) that swaps the agent container and exits.

<img src="diagrams/agent-update.png" width="1279" alt="Agent update sequence with the temporary updater container and rollback">

- Images are always pulled by digest; the panel release defines which agent and updater digests belong to it.
- The new container keeps the old one's environment, mounts, networks, restart policy and labels (recreate from inspect).
- If the updater itself dies mid-way, the old container still exists (stopped and renamed); recovery is a documented one-liner, and the panel shows the host as offline until then.
- Restarting the agent does not need the updater – a plain `docker restart` from the agent is enough.

## Agent identity and enrollment

The panel runs an internal CA. Agents authenticate with client certificates; no passwords or API tokens live on servers.

<img src="diagrams/enrollment.png" width="1012" alt="Server enrollment sequence with a one-time token and a client certificate">

- Certificate SAN `pocketcloud://host/<hostId>` identifies the host on every call.
- Agent certificates are valid for 180 days and renewed over the existing connection when 30 days remain (`RenewCertificate`).
- Removing a host revokes its certificate (serial denylist checked on connect).

## gRPC API (v1, draft)

| RPC | Kind | Purpose |
|---|---|---|
| `Enroll` | unary | token + CSR → client certificate (only RPC without client certificate) |
| `SubscribeCommands` | server stream | long-lived command delivery; request carries version, capabilities, recently finished ids |
| `ReportCommandUpdate` | unary | progress and results, idempotent by command id + sequence |
| `Heartbeat` | unary | every 10 s; vitals, RTT, clock skew |
| `ReportInventory` / `ReportEvents` | unary / client stream | Docker inventory and incremental events |
| `PushMetrics` | client stream | batched metrics |
| `StreamLogs` | client stream | log chunks for an active `logs.follow` |
| `Exec` | bidi stream | terminal sessions |
| `FetchArtifact` | server stream | agent downloads uploads and site releases |
| `UploadArtifact` | client stream | agent uploads diagnostics bundles, SQL exports and restore downloads to the panel |
| `Relay` | bidi stream | admin-only byte stream to a temporary tool container (Adminer); opened by a command and bound to one browser session, like `Exec` |
| `RenewCertificate` | unary | identity rotation |

Command payloads are a `oneof` per type: `container.*`, `image.*`, `stack.*`, `logs.*`, `exec.*`, `site.*` (static and PHP), `task.*` (scheduled tasks), `db.*`, `edge.*`, `cert.*`, `acme.*`, `backup.*`, `agent.*`. Agents advertise supported types in `capabilities`, which keeps old agents working while new command types are added.

## Command lifecycle

<img src="diagrams/command-lifecycle.png" width="531" alt="Command states: queued, dispatched, running, succeeded, failed, timed out, cancelled">

- The dispatcher reacts to new records (`OnRecordAfterCreateSuccess`) and to agent connects.
- Commands touching the same resource (stack, site, backup repository) run in order; others in parallel.
- After a reconnect the panel re-sends commands that were dispatched but never finished; the agent acknowledges ids it has already completed without running them again.
- Multi-step flows (new site, restore) are a parent command with children sharing a correlation id, which drives the progress timelines in the UI.

## Logs and terminal

<img src="diagrams/logs-terminal.png" width="436" alt="Logs and terminal relay between browser, panel, agent and container">

The relay is a custom route registered in `OnServe`; it checks the PocketBase auth token, fans one agent stream out to all viewers and stops it when the last viewer leaves.

## Static sites, domains and TLS

Servers with the `edge` role run Caddy in its own container (`pocketcloud-edge`) managed by the agent, so agent updates never interrupt traffic **(spike confirms the integration details)**. The panel generates each edge's complete route table from sites, domains and container labels and sends it with `edge.apply`; the agent validates it, loads it and keeps the last good config.

Certificates are issued **centrally by the panel** **(spike: certmagic vs lego)**, stored encrypted and distributed with `cert.install` to every edge that serves the names. One ACME account, wildcard reuse across servers, and a single place to see expiry.

- **HTTP-01**: the panel asks the edge agent to answer `/.well-known/acme-challenge/<token>`.
- **DNS-01**: the panel creates the TXT record through the DNS provider API (required for wildcards and non-public names).
- Renewal 30 days before expiry; alert at 14 days or on failure.

New site provisioning (one parent command):

<img src="diagrams/new-site.png" width="1076" alt="New site provisioning sequence: DNS records, deploy, certificate, availability check">

If the zone is not at a connected provider, the wizard shows the records to create at the registrar and polls until they resolve.

Site releases live on the `pocketcloud-sites` volume shared by the agent and the edge container (`<site>/releases/<n>` with a `current` symlink); builds run in job containers; rollback switches the symlink without rebuilding.

LAN-only names use DNS rewrites (AdGuard Home, Pi-hole) and certificates from the internal CA.

## PHP sites

A PHP site is a site with `type = php`. Its code is deployed exactly like a static site (releases and a `current` symlink on the `pocketcloud-sites` volume); requests for PHP scripts are executed by a dedicated PHP-FPM container per site, everything else is served by the edge directly.

<img src="diagrams/php-site.png" width="615" alt="PHP site request flow: edge proxy, PHP-FPM container, sites volume, database and mail relay">

- **Runtime image** – `ghcr.io/oldbear24/pocketcloud-php:<version>-fpm`, built in CI from the official `php:<version>-fpm` images for every supported PHP version (currently 8.2–8.5; the UI warns before a version reaches end of life), multi-arch. It adds the common extensions (opcache, mysqli, pdo_mysql, pdo_pgsql, gd, intl, zip, exif, imagick, redis, bcmath), `msmtp` as `sendmail`, WP-CLI, Composer and a small status helper that reports the FPM pool and OPcache state. Sites that need more extensions use an image derived from it.
- **One container per site** – `<site>-php`, labelled `pocketcloud.role=site`, attached to the `pocketcloud-edge` network under the alias `<site>-php`. It runs as `www-data` with CPU and memory limits, without the Docker socket and without published ports. It mounts only its own site directory from the sites volume (Docker volume subpaths, Engine 26+) at the same path the edge uses (`/srv/sites/<site>`), read-only, with `shared/` mounted read-write on top; `open_basedir` is limited to the site directory and `/tmp`.
- **Edge route** – Caddy `php_fastcgi <site>-php:9000` with `root /srv/sites/<site>/current/<docroot>` and `resolve_root_symlink`, so switching a release is atomic and OPcache never mixes files from two releases.
- **Deployments** – the build runs in a job container from the same runtime image, so PHP version and extensions match production (`composer install --no-dev --optimize-autoloader`, optional asset build, optional release hook such as `php artisan migrate --force`). The edge and FPM pick up the new release when `current` switches; rollback switches it back. Persistent paths (`wp-content/uploads`, Laravel `storage/`, Nette `log/`) live in `<site>/shared/` and are symlinked into every release.
- **Configuration** – PHP version, FPM pool (`pm`, `pm.max_children`, `pm.max_requests`), `php.ini` overrides, environment variables and limits per site; secrets referenced as `${secret:…}` are resolved at deploy time. Changing the PHP version is deployed as a new release on the new image, so it can be rolled back with one click.
- **Classic mode** – for sites that update themselves (e.g. WordPress managed from wp-admin) the code directory can be writable without release history; backups then cover the whole site directory.
- **Scheduled tasks** – cron expressions per site (`wp cron event run --due-now`, `php artisan schedule:run`) are scheduled by the panel and executed by the agent with `docker exec` in the site's container; every run records exit code, duration and the tail of its output. Stacks use the same mechanism.
- **Mail** – outgoing mail goes through the SMTP relay configured in the panel (msmtp); sent and failed messages are counted on the site page.
- **Observability** – pool and OPcache statistics from the status helper, latency percentiles and the 5xx rate from the edge's JSON access log, the PHP error log streamed like container logs, WP-CLI or `artisan` through the web terminal as `www-data`.
- **Templates** – WordPress, Laravel and Nette presets set the document root, build steps, shared paths, cron entries, `php.ini` defaults and a health-check URL. WordPress in release mode gets `DISALLOW_FILE_MODS` and `DISABLE_WP_CRON`; core, plugin and theme updates are built as a new release with WP-CLI, so a broken update is one rollback away.

## Database servers

Applications on the same host share a database server instead of each running its own instance. Each application still gets its own database, its own user and its own network path to the server.

<img src="diagrams/database-servers.png" width="908" alt="Shared database server with a private network per application">

- **Servers** – a shared server is a container managed by PocketCloud (MariaDB 11 LTS or PostgreSQL 17) with its data in a named volume, labelled `pocketcloud.role=db`, with no published ports. Admin credentials are generated by the panel and stored encrypted; memory settings (`innodb_buffer_pool_size`, `shared_buffers`) start from presets based on the host's RAM.
- **Dedicated servers** – database containers found inside stacks are listed as dedicated (read-only until the stack's credentials are linked). They stay dedicated when they need something a shared server does not offer (e.g. immich with VectorChord) and can otherwise be consolidated.
- **Networking** – for every application that uses a server the agent creates an internal network `pcdb-<app>` and attaches the server (alias = server name) and the application's containers. Applications reach `db-main:3306` but not each other.
- **Databases and users** – `db.database.create` creates a database and a user with privileges on that database only; the password is generated by the panel and stored as the secret `db/<name>`. Applications get `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER` and `DB_PASSWORD=${secret:db/<name>}` (or `DATABASE_URL`) in their environment.
- **Operations** – rotate a password (`ALTER USER`, update the secret, restart the application), clone a database (staging, restore tests), export SQL (uploaded to the panel with `UploadArtifact`), drop with a final dump kept. The agent runs the server's own client inside its container with `docker exec`; credentials are passed in the exec environment, never on a command line.
- **Adminer** – started on demand as a temporary container on the database's network, removed after 30 minutes and opened through the panel with the `Relay` stream; it is never published on a port.
- **Monitoring** – size per database, connections against `max_connections`, server version; alerts on connection saturation and disk usage.
- **Upgrades** – minor versions by pulling the image and recreating the container (MariaDB with `MARIADB_AUTO_UPGRADE`); PostgreSQL major versions by dump and restore into a new server, guided by the UI.
- **Consolidation** – moving a dedicated instance to a shared server: stop the application, dump, restore into a new database on a shared server with the same engine and the same or newer major version, switch the application's connection settings, start it, keep the old container stopped for 7 days before it is removed.

## Backups

- The panel owns targets, credentials, repository passwords and schedules; agents run restic.
- Docker volumes are backed up by short-lived job containers that mount the volume read-only; host paths are read through the read-only `/host` mount; restores run in job containers with only the destination mounted read-write.
- `backup.run` carries repository URL, credentials, sources and optional pre-hooks (database dumps via `restic backup --stdin-from-command`).
- Every database on a database server gets its own dump job (`mariadb-dump --single-transaction` or `pg_dump --format=custom --compress=0`, uncompressed so restic can deduplicate), tagged with the database name, so one application can be restored without touching the others. Restore tests restore the latest dump into a temporary database that is dropped after two hours.
- PHP sites back up their `shared/` directory; code is reproducible from releases and sources.
- Retention with `restic forget --prune`; weekly `restic check --read-data-subset`.
- The panel backs up its own `pb_data`; the master key is exported separately. Restoring the panel keeps the CA, so agents reconnect without re-enrollment.

## Diagnostics

Detailed diagnostics live in a separate PocketBase plugin, `plugins/diagnostics`, registered the same way as PocketBase's own plugins (`diagnostics.MustRegister(app, diagnostics.Config{…})`). When it is disabled it adds no overhead.

<img src="diagrams/diagnostics.png" width="776" alt="Diagnostics plugin data flow from the gRPC gateway, command queue and PocketBase to the API">

- **Collection** – a gRPC `stats.Handler` (connection begin/end, payload sizes, RPC timing) plus unary and stream interceptors (status codes, command ids); command-queue hooks (depth, dispatch and completion latency, retries, replays, timeouts); PocketBase internals (realtime clients, cron jobs, SQLite and WAL size); Go `runtime/metrics`.
- **Per agent** – connected since, remote address, TLS version and certificate expiry, RTT p50/p95, clock skew, messages and bytes in/out, reconnects with classified reasons (EOF, keepalive timeout, TLS error, revoked certificate), last error.
- **Storage** – in memory only: counters, histograms and a ring buffer of the last 500 message metadata entries per agent, 24 h window. Connection events also go to the `events` collection so they survive restarts.
- **Agent side** – heartbeats carry light runtime stats (goroutines, heap, worker pool, Docker API latency, free space on the agent volume); periodic self-checks (Docker socket, host mount, edge container, restic, time sync); `agent.diagnostics` builds a bundle uploaded with `UploadArtifact`; `agent.set_log_level` enables debug logging for a limited time.
- **Payload capture** – off by default. An admin can capture message payloads of one agent for 10 minutes; fields marked with a custom `sensitive` proto field option are always masked.
- **Output** – the Diagnostika page (System section of the UI), an optional token-protected Prometheus endpoint, and downloadable debug bundles (`diagnostic_bundles`, with expiry).

## Change audit

Every configuration change is written to the append-only `audit_log` collection: who made it, from where, what exactly changed, and which command it caused.

<img src="diagrams/change-audit.png" width="626" alt="Change audit flow from record hooks through the audit writer to the audit log">

- **Scope** – configuration collections: hosts, stacks, sites, domains, DNS providers, certificates, backup targets and jobs, database servers and databases, scheduled tasks, users and roles, API tokens, alert rules, PocketCloud and PocketBase settings, collection schema. High-volume or derived data (inventory, metrics, events, diagnostics) is not audited; commands are shown from `commands` in their own tab.
- **Writer** – one component registered on PocketBase's record hooks (after a successful create, update or delete) for the audited collections. It compares the record with `Original()` and stores field-level diffs; a deletion stores the last state. The actor (user, API token or superuser), IP, user agent and source (UI, API, PocketBase dashboard) come from the request; changes without a request are attributed to the component that made them (scheduler, ACME manager, agent report, migration).
- **Secrets** – values of fields marked 🔒 never reach the audit; the entry only records that the field changed.
- **Links** – an entry points to the command it caused (a saved stack → `stack.deploy`), to the revision (stack r7 → r8) and can carry a note from the user.
- **Integrity** – each entry stores `prev_hash` and `hash = SHA-256(prev_hash ‖ canonical JSON of the entry)` (RFC 8785). A daily job and the “Ověřit integritu” button verify the chain; a break raises an alert. Updates and deletions of audit entries are rejected in hooks, for superusers too; only retention removes old entries, and it first writes a checkpoint entry with the hash the chain continues from.
- **Retention and export** – `audit.retention_days` (default 365); JSONL export with hashes from the UI or API, optionally pushed daily to a backup target to keep an off-site copy.
- **Revert** – objects with revisions (stacks) are reverted by deploying the old revision as a new one; other records get their previous non-secret values back as a new change. Reverts are audited like any other change.
- **Security events** – sign-ins and failed sign-ins, MFA changes, API tokens created, used and revoked, enrollment tokens, certificate revocations, terminal and Adminer sessions, diagnostics payload capture. They form the Bezpečnost tab.

## Security model

- Trust boundaries: browser ↔ panel (HTTPS + PocketBase auth), agent ↔ panel (mTLS), panel ↔ external APIs (DNS, ACME, storage).
- A compromised agent can only act as its own host: it receives only its own commands and the secrets those commands need.
- Access to the Docker socket is root-equivalent on that host. This is inherent to managing Docker; the host root is mounted read-only and writes go through job containers with explicit mounts.
- Enrollment tokens are single use, short-lived, stored hashed and rate limited; the install command pins the panel CA.
- Terminal access is admin-only and audited; host shells are opt-in per agent.
- PHP containers run as `www-data`, see only their own site directory and have no Docker socket; each application reaches its database server over its own internal network with a user limited to its own database.
- Roles: admin, operator, viewer. Automation (CI) uses scoped, expiring API tokens stored hashed; their use is attributed in the audit log.
- Configuration changes are audited with tamper-evident hash chaining (see [Change audit](#change-audit)).

A full threat model is tracked in the security review issue (milestone M5).
