# ADR 0004: Site builds run on the target host

**Status:** accepted · **Issue:** #6 · **Affects:** #21, #50

## Context

Static sites and PHP applications are built before they are released: `hugo`, `npm run build`, `composer install`. A build can run on the host that serves the site, or on a dedicated builder.

## Decision

Builds run **on the target host**, in a throwaway job container with CPU and memory limits, writing into `<site>/releases/<n>` on the `pocketcloud-sites` volume.

- A homelab or a few VPS rarely have a spare machine, and a dedicated builder would be one more thing to install before the first site works.
- There is no artifact to transfer: the output already is where it will be served.
- PHP builds must run on the same image as production anyway, so that the PHP version and extensions match.
- The architecture has no connection between hosts; a builder would add one.

## Consequences

- A heavy build uses resources of the host that serves traffic. The job container's limits keep that bounded, and the limits are per site.
- A small edge may be too weak to build a large site. That is the one case a builder host would help with, and nothing here prevents adding one later: a build is already a separate command that produces a directory, so it could equally well produce an artifact that the target fetches with `FetchArtifact`.
