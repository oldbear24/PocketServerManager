# ADR 0001: ACME with lego, DNS records with libdns

**Status:** accepted · **Issue:** #6 · **Affects:** #22, #23, #26

## Context

The panel issues every certificate itself and pushes it to the edges that serve the names, so it needs an ACME client it can drive. It also has to create and remove the A, AAAA and CNAME records of a site, not only the TXT records of a DNS-01 challenge. Cloudflare comes first, then Hetzner DNS and WEDOS.

Checked on 2026-11-03:

| | [lego](https://github.com/go-acme/lego) v4.35.2 | [certmagic](https://github.com/caddyserver/certmagic) v0.25.6 | [libdns](https://github.com/libdns/libdns) |
|---|---|---|---|
| Purpose | ACME client library | Managed certificates: storage, renewal loop, serving | DNS record API, implemented per provider |
| Cloudflare / Hetzner / WEDOS | all three (198 DNS providers) | through libdns | `libdns/cloudflare` v0.2.2, `libdns/hetzner` v1.0.0, `libdns/wedos` v1.0.4 |
| Custom HTTP-01 and DNS-01 | `challenge.Provider` interface | solvers, built around its own maintenance loop | – |
| General DNS records | no: providers only present and clean up TXT | no | yes: get, append, set, delete |

## Decision

- **lego** is the ACME client. The panel decides when to issue and renew; lego does one issuance when it is asked to. Its `challenge.Provider` lets HTTP-01 go through a command to the edge agent.
- **libdns** provider packages are the DNS layer of #22: one adapter per provider behind one interface, with zones and all record types.
- DNS-01 does not use lego's own DNS providers. It uses a small `challenge.Provider` on top of the libdns adapter, so that a connected provider has one set of credentials, one code path and one place for errors.

certmagic is the wrong shape here: it wants to own storage and renewal, which the panel has to control so that it can show expiry, retry with backoff and distribute certificates.

## Consequences

- Only the provider packages that are imported get linked, so lego's long dependency list does not end up in the binary.
- Adding a DNS provider means adding a libdns adapter. A provider with a lego package but no libdns package would need its own adapter.
- WEDOS's API only accepts requests from allow-listed IP addresses; the provider card has to explain that when it fails (#22).

## Not verified yet

The acceptance criterion of #6 also asks for a prototype that obtains a Let's Encrypt **staging** certificate with DNS-01 on Cloudflare and with HTTP-01 through an edge. That needs a Cloudflare account and a publicly reachable edge, and has not been done. It belongs to the start of #23.
