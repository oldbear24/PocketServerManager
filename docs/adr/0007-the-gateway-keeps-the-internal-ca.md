# ADR 0007: The gRPC gateway keeps the internal CA; only the panel's HTTPS takes a certificate from ACME

**Status:** accepted · **Issue:** #63

## Context

With #63 the panel gets a certificate for its public hostname from its ACME accounts and serves its UI and API with it on `--https-addr`. The panel has a second TLS endpoint: the gRPC gateway on `--grpc-addr` (`:9443`), which only agents talk to. It serves a certificate of the panel's internal CA (`ca.ServerTLSConfig`, `internal/gateway/server.go`), and by ADR 0006 an agent trusts nothing else: the chain must lead to the root it pinned with `CA_PIN`, the certificate must have the server-auth key usage and carry the URI SAN `pocketcloud://panel`, and the dialled host name is not compared (`pki.VerifyPanel`). On enrollment the agent finds the root among the certificates the gateway sends (`pki.RootFromPin`). The gateway's trust in agents comes from the same CA: their client certificates are signed by its agents' intermediate and checked on every call.

ADR 0006 left open whether the gateway should serve a publicly trusted certificate once there is one. The options:

1. **The gateway serves the ACME certificate.** No public CA issues a `pocketcloud://` URI SAN, and its chain does not lead to the pin, so agents would have to change the way they check the panel: trust public roots, compare the host name in `PANEL` with the certificate, and give up the fixed name. That brings back what ADR 0006 avoided: a panel reached through an IP address, a changed name or a port forward would strand its agents. Every enrolled agent and every `CA_PIN` handed out in install commands would need a migration, on servers nobody may be able to reach. Pinning instead of public roots does not help: lego makes a new key for every certificate, so a leaf or key pin would change every 60 to 90 days, and a public CA's intermediates change on the CA's schedule, not ours. The agents' client certificates would still come from the internal CA, so its root would stay as a second trust anchor and nothing would be removed.
2. **Both certificates on `:9443`, picked by SNI or ALPN.** Agents and any other gRPC client send the same thing, h2 and the `PANEL` host as SNI, so the panel could not tell which certificate to offer without a protocol change in agents, for clients that do not exist: only agents use the gateway.
3. **The gateway keeps the internal CA.** No agent changes. The ACME certificate is used where browsers and scripts need public trust: the UI and the API.

## Decision

The gRPC gateway keeps serving its internal-CA server certificate, and agents keep verifying the panel as ADR 0006 says. The certificate from ACME is only for the panel's HTTPS address. It is never offered on the gateway's port, and gRPC is not carried over the HTTPS port. Nothing in `internal/gateway`, `internal/agent` or `internal/pki` changes.

The internal CA's gateway certificate, which already names the public host and the agent endpoint, is also the panel's HTTPS certificate until the first one from ACME is issued, and for handshakes that do not ask for the panel's name. So the panel can serve HTTPS from its first start.

## Consequences

- Enrolled agents, their stored root and every `CA_PIN` already handed out keep working. Install commands do not change, and `PANEL` may still be an IP address or another name of the panel.
- An agent's trust depends on no public CA and on no DNS name: a CA that issues wrongly, or a hijacked DNS record, cannot impersonate the panel to agents.
- Port 9443 stays a separate port next to 443 (and 80 for HTTP-01). A proxy in front of it must pass TCP through and must not terminate TLS, as before.
- Clients other than agents that talk to the gateway (grpcurl, monitoring) need the root from `/api/pc/ca.pem`, and a browser that opens `:9443` sees a certificate it does not trust. This is intended: the gateway is not for people.
- The internal root (20 years) remains the one long-lived secret that agents depend on, sealed with the master key as before. The ACME certificate adds no secret that agents depend on.
- Carrying gRPC over port 443 one day, for networks that allow only 443, needs a new ADR: agents would have to signal themselves (for example a dedicated ALPN or SNI), and the panel would split connections by it.
