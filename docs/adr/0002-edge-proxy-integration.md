# ADR 0002: Driving the Caddy edge through its admin API

**Status:** accepted · **Issue:** #6 · **Affects:** #20, #21, #23, #25, #50

## Context

Servers with the `edge` role serve sites and exposed containers on ports 80 and 443. Caddy runs in its own container, `pocketcloud-edge`, because the agent container is replaced on every update and traffic must not notice. The agent has to load generated configuration, get certificates to Caddy, and answer HTTP-01 challenges.

## Decision

**Configuration: the admin API, with the complete JSON config.** The panel generates the whole configuration of an edge and sends it with `edge.apply`. The agent posts it to Caddy's `/load`. Caddy validates a new config before it replaces the old one and reloads gracefully, which gives both acceptance criteria of #20: an invalid config is rejected with the previous one still active, and adding a route drops no connections.

**The admin API listens on a Unix socket** in a volume shared by the agent and the edge container, never on a TCP port. Nothing else can reach it, not even other containers on the same network.

**The last good config survives restarts** through Caddy's own `--resume`, so an edge keeps serving while the panel or the agent is down. The agent also stores it, for reinstalling the edge container.

**Certificates: PEM files in the shared volume**, referenced with `tls.certificates.load_files`, with automatic HTTPS disabled. `cert.install` writes the files first (key mode 0600, atomic rename) and the next `edge.apply` refers to them. Caddy never talks to an ACME server by itself.

**HTTP-01: files in the shared volume.** The generated config has a route for `/.well-known/acme-challenge/*` on port 80, served from a directory in the volume. `acme.challenge.present` writes `<token>` there, `acme.challenge.cleanup` removes it.

#20 originally said challenges are served "from agent memory". That would need the agent to listen on a port that Caddy can reach, while the principle is that agents have no inbound ports, and a challenge would fail if the agent were being updated at that moment. A file needs neither.

## Consequences

- The agent needs no reverse proxy of its own and no extra listening port.
- Agent updates cannot interrupt traffic or a running ACME challenge.
- The panel must always send complete configs; there are no partial route updates to get out of sync.
- A Caddy version with a different JSON schema has to be handled by the panel's generator, so the panel release pins the edge image by digest, like the agent.
