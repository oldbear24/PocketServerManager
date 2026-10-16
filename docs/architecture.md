# Architecture

> Status: proposal. Decisions marked **(spike)** are settled in the spike issue in milestone M0 and recorded as ADRs in `docs/adr/`.

PocketCloud has two binaries:

- **Panel** – one Go process: [PocketBase](https://pocketbase.io) embedded as a library (auth, collections, REST + realtime API, files, cron), the React UI, a gRPC gateway for agents, and the managers for commands, DNS, certificates and backups.
- **Agent** – a small container (`pocketcloud-agent`) on every managed server. It dials out to the panel, executes commands (Docker, Compose, edge proxy, site deploys, restic) and streams data back. It never replaces itself: updates are done by a short-lived **updater** container (`pocketcloud-updater`).

```mermaid
flowchart LR
  subgraph Browser
    UI[React UI]
  end

  subgraph Panel["Panel (single Go binary)"]
    PB[(PocketBase<br/>collections, auth, files)]
    API[REST + realtime API]
    GW[gRPC gateway :9443]
    CQ[Command dispatcher]
    REL[Log / exec relay]
    ACME[ACME + DNS manager]
    SCHED[Scheduler]
  end

  subgraph Server["Managed server (Docker)"]
    AG[Agent container]
    DK[Docker Engine]
    EDGE[Edge container - Caddy]
    RS[restic]
    UPD[Updater container - only during updates]
  end

  UI -- HTTPS --> API
  UI -- SSE / WebSocket --> REL
  API --- PB
  CQ --- PB
  SCHED --- PB
  AG -- "gRPC over mTLS (dial-out)" --> GW
  GW --- CQ
  GW --- REL
  AG --> DK
  AG --> EDGE
  AG --> RS
  AG -. starts on update .-> UPD
  UPD -. replaces .-> AG
  ACME -- provider API --> DNS[(DNS providers)]
  ACME -- ACME --> CA[(Let's Encrypt / ZeroSSL)]
  RS -- backups --> TGT[(S3 / SFTP / rest-server)]
```

## Principles

1. **Agents dial out.** The panel never opens connections to servers. Servers behind NAT work and need no inbound ports (except 80/443 on servers with the `edge` role).
2. **The panel is the source of truth.** Agents keep only their identity and what they need to keep serving when the panel is down (edge config, certificates).
3. **Every action is a command.** UI actions and scheduled jobs create records in `commands`. The collection is both the audit trail and the delivery queue. Delivery is at-least-once; agents deduplicate by command id.
4. **Streams bypass the database.** Logs, terminal sessions and command output are relayed agent → panel → browser and never stored in PocketBase.
5. **Secrets are encrypted at rest** with a master key kept outside the database, and are sent to an agent only inside the command that needs them.
6. **Everything on a server is a container.** The agent, the edge proxy, site builds, backup and restore jobs and the updater all run in Docker; the agent never updates itself in place.

## Ports

| Component | Port | Purpose |
|---|---|---|
| Panel | 443 (or behind a reverse proxy) | UI and REST/realtime API |
| Panel | 9443 | gRPC gateway for agents (mTLS) |
| Agent container | – | no inbound ports |
| Edge container (`edge` role) | 80, 443 | static sites and exposed containers |

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

```mermaid
sequenceDiagram
  autonumber
  participant P as Panel
  participant A as Agent (old)
  participant D as Docker Engine
  participant U as Updater (temporary)
  participant N as Agent (new)

  P->>A: agent.update (agent and updater images pinned by digest)
  A->>D: pull both images by digest
  A->>D: run updater (auto-remove, docker.sock + agent volume)
  A-->>P: progress: updater started
  U->>D: inspect pocketcloud-agent
  U->>D: stop old container, rename it
  U->>D: create new container from the same config with the new image
  D->>N: start
  N->>P: reconnect with the new version
  U->>D: wait until the new container is healthy
  alt healthy before the timeout
    U->>D: remove old container
  else unhealthy or timeout
    U->>D: remove new container, rename and start the old one
  end
  U->>U: write the result to the agent volume, exit
  Note over N,P: the agent running afterwards reports the result for the command id
```

- Images are always pulled by digest; the panel release defines which agent and updater digests belong to it.
- The new container keeps the old one's environment, mounts, networks, restart policy and labels (recreate from inspect).
- If the updater itself dies mid-way, the old container still exists (stopped and renamed); recovery is a documented one-liner, and the panel shows the host as offline until then.
- Restarting the agent does not need the updater – a plain `docker restart` from the agent is enough.

## Agent identity and enrollment

The panel runs an internal CA. Agents authenticate with client certificates; no passwords or API tokens live on servers.

```mermaid
sequenceDiagram
  autonumber
  actor Admin
  participant UI as Panel UI
  participant P as Panel
  participant A as Agent container (new server)

  Admin->>UI: Connect a server (name, roles)
  UI->>P: create enrollment token (hashed, single use, 15 min)
  P-->>UI: docker run command with token and CA pin
  Admin->>A: docker run
  A->>A: generate key pair and CSR
  A->>P: Enroll(token, CSR) over TLS, panel verified with CA pin
  P->>P: validate token, create host, sign client certificate
  P-->>A: certificate and CA chain
  A->>P: SubscribeCommands over mTLS
  A->>P: ReportInventory
  P-->>UI: realtime: host online
```

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
| `UploadArtifact` | client stream | agent uploads diagnostics bundles and restore downloads to the panel |
| `RenewCertificate` | unary | identity rotation |

Command payloads are a `oneof` per type: `container.*`, `image.*`, `stack.*`, `logs.*`, `exec.*`, `site.*`, `edge.*`, `cert.*`, `acme.*`, `backup.*`, `agent.*`. Agents advertise supported types in `capabilities`, which keeps old agents working while new command types are added.

## Command lifecycle

```mermaid
stateDiagram-v2
  [*] --> queued
  queued --> dispatched: agent connected
  queued --> cancelled: user cancels or expires
  dispatched --> running: agent accepted
  dispatched --> timed_out: no answer in time
  running --> succeeded
  running --> failed
  running --> timed_out
  succeeded --> [*]
  failed --> [*]
  timed_out --> [*]
  cancelled --> [*]
```

- The dispatcher reacts to new records (`OnRecordAfterCreateSuccess`) and to agent connects.
- Commands touching the same resource (stack, site, backup repository) run in order; others in parallel.
- After a reconnect the panel re-sends commands that were dispatched but never finished; the agent acknowledges ids it has already completed without running them again.
- Multi-step flows (new site, restore) are a parent command with children sharing a correlation id, which drives the progress timelines in the UI.

## Logs and terminal

```mermaid
flowchart LR
  B[Browser] -- "SSE (logs) / WebSocket (exec)" --> R[Panel relay]
  R -- "logs.follow / exec.start command" --> A[Agent]
  A -- "StreamLogs / Exec gRPC stream" --> R
  A -- Docker API --> D[Container]
```

The relay is a custom route registered in `OnServe`; it checks the PocketBase auth token, fans one agent stream out to all viewers and stops it when the last viewer leaves.

## Static sites, domains and TLS

Servers with the `edge` role run Caddy in its own container (`pocketcloud-edge`) managed by the agent, so agent updates never interrupt traffic **(spike confirms the integration details)**. The panel generates each edge's complete route table from sites, domains and container labels and sends it with `edge.apply`; the agent validates it, loads it and keeps the last good config.

Certificates are issued **centrally by the panel** **(spike: certmagic vs lego)**, stored encrypted and distributed with `cert.install` to every edge that serves the names. One ACME account, wildcard reuse across servers, and a single place to see expiry.

- **HTTP-01**: the panel asks the edge agent to answer `/.well-known/acme-challenge/<token>`.
- **DNS-01**: the panel creates the TXT record through the DNS provider API (required for wildcards and non-public names).
- Renewal 30 days before expiry; alert at 14 days or on failure.

New site provisioning (one parent command):

```mermaid
sequenceDiagram
  autonumber
  participant UI as Panel UI
  participant P as Panel
  participant DNS as DNS provider
  participant A as Edge agent
  participant CA as ACME CA

  UI->>P: create site (source, domain, server)
  P->>P: find zone (longest suffix match among connected zones)
  P->>DNS: create A / AAAA (+ www CNAME)
  P->>P: wait for propagation (public resolvers + authoritative NS)
  P->>A: site.deploy (build in throwaway container, atomic release switch)
  A-->>P: release active
  P->>A: acme.challenge.put
  P->>CA: order certificate (HTTP-01)
  CA->>A: GET /.well-known/acme-challenge/...
  CA-->>P: certificate
  P->>A: cert.install + edge.apply
  P->>A: availability check
  P-->>UI: done, https://domain
```

If the zone is not at a connected provider, the wizard shows the records to create at the registrar and polls until they resolve.

Site releases live on the `pocketcloud-sites` volume shared by the agent and the edge container (`<site>/releases/<n>` with a `current` symlink); builds run in job containers; rollback switches the symlink without rebuilding.

LAN-only names use DNS rewrites (AdGuard Home, Pi-hole) and certificates from the internal CA.

## Backups

- The panel owns targets, credentials, repository passwords and schedules; agents run restic.
- Docker volumes are backed up by short-lived job containers that mount the volume read-only; host paths are read through the read-only `/host` mount; restores run in job containers with only the destination mounted read-write.
- `backup.run` carries repository URL, credentials, sources and optional pre-hooks (database dumps via `restic backup --stdin-from-command`).
- Retention with `restic forget --prune`; weekly `restic check --read-data-subset`.
- The panel backs up its own `pb_data`; the master key is exported separately. Restoring the panel keeps the CA, so agents reconnect without re-enrollment.

## Diagnostics

Detailed diagnostics live in a separate PocketBase plugin, `plugins/diagnostics`, registered the same way as PocketBase's own plugins (`diagnostics.MustRegister(app, diagnostics.Config{…})`). When it is disabled it adds no overhead.

```mermaid
flowchart LR
  subgraph Panel
    GW[gRPC gateway] -- stats handler and interceptors --> DG[Diagnostics plugin]
    CQ[Command queue] -- hooks --> DG
    PB[PocketBase: realtime, cron, SQLite] -- hooks --> DG
    DG --> API["/api/diag/* (admin only)"]
    DG --> MET["/metrics (Prometheus, token)"]
  end
  AG[Agent] -- "heartbeat stats, self-checks, bundles" --> GW
  API --> UI[Diagnostika page]
```

- **Collection** – a gRPC `stats.Handler` (connection begin/end, payload sizes, RPC timing) plus unary and stream interceptors (status codes, command ids); command-queue hooks (depth, dispatch and completion latency, retries, replays, timeouts); PocketBase internals (realtime clients, cron jobs, SQLite and WAL size); Go `runtime/metrics`.
- **Per agent** – connected since, remote address, TLS version and certificate expiry, RTT p50/p95, clock skew, messages and bytes in/out, reconnects with classified reasons (EOF, keepalive timeout, TLS error, revoked certificate), last error.
- **Storage** – in memory only: counters, histograms and a ring buffer of the last 500 message metadata entries per agent, 24 h window. Connection events also go to the `events` collection so they survive restarts.
- **Agent side** – heartbeats carry light runtime stats (goroutines, heap, worker pool, Docker API latency, free space on the agent volume); periodic self-checks (Docker socket, host mount, edge container, restic, time sync); `agent.diagnostics` builds a bundle uploaded with `UploadArtifact`; `agent.set_log_level` enables debug logging for a limited time.
- **Payload capture** – off by default. An admin can capture message payloads of one agent for 10 minutes; fields marked with a custom `sensitive` proto field option are always masked.
- **Output** – the Diagnostika page (System section of the UI), an optional token-protected Prometheus endpoint, and downloadable debug bundles (`diagnostic_bundles`, with expiry).

## Security model

- Trust boundaries: browser ↔ panel (HTTPS + PocketBase auth), agent ↔ panel (mTLS), panel ↔ external APIs (DNS, ACME, storage).
- A compromised agent can only act as its own host: it receives only its own commands and the secrets those commands need.
- Access to the Docker socket is root-equivalent on that host. This is inherent to managing Docker; the host root is mounted read-only and writes go through job containers with explicit mounts.
- Enrollment tokens are single use, short-lived, stored hashed and rate limited; the install command pins the panel CA.
- Terminal access is admin-only and audited; host shells are opt-in per agent.
- Roles: admin, operator, viewer.

A full threat model is tracked in the security review issue (milestone M5).
