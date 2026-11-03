# ADR 0006: Agents identify the panel by its CA and a fixed name, not a host name

**Status:** accepted · **Issue:** #7

## Context

Agents connect to the gateway over TLS. The usual check compares the certificate with the host name that was dialled. That is a poor fit here: a panel may be reached through an IP address, a changed DNS name or a port forward, and a certificate that does not list that name would strand every agent behind it, on servers nobody may be able to reach.

## Decision

An agent trusts exactly one root certificate: the panel's internal CA. It gets it from the panel on enrollment, checked against `CA_PIN` (the SHA-256 of the root certificate, part of the install command), and keeps it in the agent volume. The server certificate must chain to that root, have the server-auth key usage and carry the URI SAN `pocketcloud://panel`. The dialled host name is not compared.

The gateway requests client certificates but does not verify them during the handshake. An interceptor verifies them on every call, so that:

- `Enroll` can be called without a certificate, and nothing else can;
- a rejected agent gets a clear gRPC error (expired, revoked, removed) and an event is recorded, instead of a failed handshake that says nothing;
- a certificate revoked now stops working now, also on connections that are already open.

## Consequences

- Only someone with the CA's key can impersonate the panel. Agent certificates cannot: they only have the client-auth key usage and are signed by the agents' intermediate.
- The host name in `PANEL` can change without touching agents, as long as it reaches the same panel.
- The panel's name is not protected by public CAs. If a publicly trusted certificate is ever used for the gateway (#23), agents still need the internal one, so the gateway keeps it.
