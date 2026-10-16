// The screens of the PocketCloud UI design, in canvas order.
// `source` is the artboard file name in design/source (without .dc.html),
// `slug` names the screenshot in design/screenshots.
export const screens = [
  {
    source: 'Main',
    slug: '01-overview',
    title: 'Overview',
    description: 'Agents, containers, sites and backups at a glance; servers with live load, alerts, command queue.'
  },
  {
    source: 'Server',
    slug: '02-server-detail',
    title: 'Server detail',
    description: 'Agent connection (gRPC, mTLS, heartbeat), usage charts, containers, edge proxy routes, disks, events.'
  },
  {
    source: 'Stack',
    slug: '03-stack',
    title: 'Compose stack',
    description: 'Services, compose.yaml editor with unsaved changes, revisions, secrets and live logs.'
  },
  {
    source: 'Sites',
    slug: '04-static-site',
    title: 'Static site',
    description: 'Deployments with instant rollback, domains with DNS and TLS state, build source and HTTPS options.'
  },
  {
    source: 'SiteNew',
    slug: '05-new-site-wizard',
    title: 'New site wizard',
    description: 'Source, domain and certificate in one form; DNS zone detection and a live provisioning timeline.'
  },
  {
    source: 'Domains',
    slug: '06-domains-tls',
    title: 'Domains, DNS and certificates',
    description: 'DNS providers, certificates with expiry and challenge type, zones, ACME account, internal CA.'
  },
  {
    source: 'Backups',
    slug: '07-backups',
    title: 'Backups',
    description: 'Backup jobs with 14-day history, failed run banner, snapshots for restore, backup targets.'
  },
  {
    source: 'Enroll',
    slug: '08-connect-server',
    title: 'Connect a server',
    description: 'One-time enrollment token, Docker install command (docker run, Compose, script) and live connection checklist.'
  },
  {
    source: 'Diagnostics',
    slug: '09-diagnostics',
    title: 'Diagnostics',
    description: 'gRPC connection health per agent, agent state and self-checks, live message trace, panel internals.'
  },
  {
    source: 'Audit',
    slug: '10-audit',
    title: 'Change audit',
    description: 'Append-only log of every configuration change with author, source and field or YAML diff; revert, export, integrity check.'
  },
  {
    source: 'SitePhp',
    slug: '11-php-site',
    title: 'PHP site',
    description: 'PHP-FPM runtime per site (version, extensions, php.ini), deployments, database on a shared server, persistent data, cron, mail, PHP error log.'
  },
  {
    source: 'Databases',
    slug: '12-databases',
    title: 'Databases',
    description: 'Shared database servers per host with a database and user per app, connection details, consolidation of dedicated instances.'
  },
  {
    source: 'Import',
    slug: '13-coolify-import',
    title: 'Import from Coolify',
    description: 'Migration plan from a Coolify installation: sources, in place or onto another server, resource mapping with compose diff and variables, cutover steps and rollback.'
  }
];
