# Architecture

> Status: proposal. Decisions marked **(spike)** are settled in the spike issue in milestone M0 and recorded as ADRs in `docs/adr/`.

PocketCloud has two binaries:

- **Panel** – one Go process: [PocketBase](https://pocketbase.io) embedded as a library (auth, collections, REST + realtime API, files, cron), the React UI, a gRPC gateway for agents, and the managers for commands, DNS, certificates and backups.
- **Agent** – one small Go process per managed server. It dials out to the panel, executes commands (Docker, Compose, edge proxy, site deploys, restic) and streams data back.

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

  subgraph Server["Managed server"]
    AG[Agent]
    DK[Docker Engine]
    EDGE[Edge proxy - Caddy]
    RS[restic]
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

## Ports

| Component | Port | Purpose |
|---|---|---|
| Panel | 443 (or behind a reverse proxy) | UI and REST/realtime API |
| Panel | 9443 | gRPC gateway for agents (mTLS) |
| Agent | – | no inbound ports |
| Agent with `edge` role | 80, 443 | static sites and exposed containers |

## Agent identity and enrollment

The panel runs an internal CA. Agents authenticate with client certificates; no passwords or API tokens live on servers.

```mermaid
sequenceDiagram
  autonumber
  actor Admin
  participant UI as Panel UI
  participant P as Panel
  participant A as Agent (new server)

  Admin->>UI: Connect a server (name, roles)
  UI->>P: create enrollment token (hashed, single use, 15 min)
  P-->>UI: install command with token and CA pin
  Admin->>A: run install command
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

Servers with the `edge` role run Caddy **(spike: managed container vs embedded)**. The panel generates each edge's complete route table from sites, domains and container labels and sends it with `edge.apply`; the agent validates it, loads it and keeps the last good config.

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

Site releases live in `/srv/pocketcloud/sites/<site>/releases/<n>` with a `current` symlink; rollback switches the symlink without rebuilding.

LAN-only names use DNS rewrites (AdGuard Home, Pi-hole) and certificates from the internal CA.

## Backups

- The panel owns targets, credentials, repository passwords and schedules; agents run restic.
- `backup.run` carries repository URL, credentials, sources and optional pre-hooks (database dumps via `restic backup --stdin-from-command`).
- Retention with `restic forget --prune`; weekly `restic check --read-data-subset`.
- The panel backs up its own `pb_data`; the master key is exported separately. Restoring the panel keeps the CA, so agents reconnect without re-enrollment.

## Security model

- Trust boundaries: browser ↔ panel (HTTPS + PocketBase auth), agent ↔ panel (mTLS), panel ↔ external APIs (DNS, ACME, storage).
- A compromised agent can only act as its own host: it receives only its own commands and the secrets those commands need.
- Enrollment tokens are single use, short-lived, stored hashed and rate limited; the install command pins the panel CA.
- Terminal access is admin-only and audited; host shells are opt-in per agent.
- Roles: admin, operator, viewer.

A full threat model is tracked in the security review issue (milestone M5).
