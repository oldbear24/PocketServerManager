# Data model

PocketBase collections, created by Go migrations in [`migrations/`](../migrations/). The collections of M0 to M2 exist and are described as implemented; the others are reserved and land with their features. `internal/schema` lists the names, the encrypted fields and what the audit covers, and this document is updated together with the migrations.

Conventions:

- Every collection has PocketBase's `id`, `created` and `updated`.
- Fields marked 🔒 are text fields, hidden from the API, sealed with the master key before they are stored, and never written to the audit log.
- Agents never use the REST API; everything agents report arrives over gRPC and is written by the panel.

## Identity and access

| Collection | Purpose | Key fields |
|---|---|---|
| `users` (auth, built-in) | People using the panel. There is no public sign-up: the first account comes from the setup link in the panel's log, later ones from admins | `role` (admin / operator / viewer), MFA settings |
| `api_tokens` | Scoped tokens for automation (CI) | `name`, `token_hash`, `scopes[]`, `expires_at`, `last_used_at`, `last_used_ip`, `created_by`, `revoked_at` |
| `enrollment_tokens` | One-time tokens for connecting servers | `token_hash` (hidden), `hint`, `name`, `roles[]`, `expires_at`, `used_at`, `created_by`, `host` |
| `audit_log` | Append-only change and security log | `seq`, `at`, `category` (change / security / system), `action` (create / update / delete / revert / sign_in / checkpoint / …), `actor_kind` (user / token / superuser / system / anonymous), `actor`, `actor_label`, `source` (ui / api / pb_admin / migration / scheduler / agent / panel), `ip`, `user_agent`, `collection`, `record_id`, `record_label`, `host`, `changes` (json: field → from / to, secrets only as “changed”), `snapshot` (json: the last state on delete, details of security events), `command`, `revision`, `note`, `prev_hash`, `hash` |

`audit_log` has no create, update or delete API rules; entries are written only by the audit writer, and hooks reject every other write, for superusers too (retention excepted, see [Change audit](architecture.md#change-audit)). Admins read everything; operators only see the category `change`. The head of the chain, sealed with a key derived from the master key, is kept in the plain table `_pc_audit_head` (one row: `seq`, `hash`, `mac`, the last sealed `sealed_seq`, `sealed_hash`, `sealed_mac`, and `keyless`, the sealed ranges of entries written without the master key), created with the first entry, which the API and the dashboard do not see.

API rules in short: viewers read, operators manage workloads, admins do everything. Collections the panel itself maintains (`containers`, `events`, `stack_revisions` and others) cannot be written through the API at all.

## Hosts and agents

| Collection | Purpose | Key fields |
|---|---|---|
| `hosts` | A managed server and its agent | `name`, `roles[]` (docker / edge / backup), `status` (online / degraded / offline), `hostname`, `os`, `kernel`, `arch`, `cpu_cores`, `mem_bytes`, `ip4`, `ip6`, `labels`, `agent_version`, `agent_image_digest`, `agent_container_id`, `capabilities`, `docker_version`, `compose_version`, `last_seen`, `rtt_ms`, `clock_skew_ms`, `connected_since`, `stream_open`, `uptime_s`, `load1`, `load5`, `load15`, `cpu_percent`, `mem_used_bytes`, `disk_used_bytes`, `disk_total_bytes`, `runtime` (json), `self_checks` (json), `inventory_at`, `cert_serial`, `cert_prev_serial`, `cert_expires_at`, `host_shell_enabled`; the edge proxy's state: `edge_digest` and `edge_revision` (the config the panel wants), `edge_applied_digest`, `edge_applied_revision`, `edge_running`, `edge_error`, `edge_requests_per_s`, `edge_caddy_version` (what the agent reports); `public_ip4` and `public_ip6`, what the A and AAAA records of its domains point at (#22) |
| `revoked_certs` | Agent certificate denylist | `serial`, `host`, `host_name`, `reason` |
| `commands` | Audit trail and delivery queue. A payload holds no secrets: they are read when the command is delivered | `host`, `host_name`, `type`, `label`, `payload` (json), `status` (queued / dispatched / running / succeeded / failed / timed_out / cancelled), `requested_by`, `requested_label`, `resource` (ordering key), `parent`, `correlation_id`, `attempts`, `last_sequence`, `timeout_s`, `dispatched_at`, `started_at`, `finished_at`, `expires_at`, `progress` (json), `result` (json), `error`, `output` (file) |
| `events` | Timeline per host and resource; removed after 30 days | `host`, `kind`, `severity`, `message`, `data` |
| `alerts` | Open problems; those about a certificate (`certificate_failed`, `certificate_expiring`) or a DNS provider (`dns_provider_failed`) have its id as `target` | `rule`, `target`, `host`, `severity`, `status` (open / acknowledged / resolved), `first_seen`, `last_seen`, `resolved_at`, `message`, `acknowledged_by` |
| `alert_rules` | Thresholds and channels | `kind`, `params`, `channels[]`, `enabled` |
| `notification_channels` | E-mail, ntfy, Gotify, webhook | `kind`, `config` 🔒 |
| `diagnostic_bundles` | Debug bundles from agents | `host`, `requested_by`, `command`, `file`, `size_bytes`, `expires_at` |

Live diagnostics (connection stats, message trace, queue latencies) are kept in memory by the diagnostics plugin and are not stored in collections.

## Containers and stacks

| Collection | Purpose | Key fields |
|---|---|---|
| `stacks` | Compose project on a host | `host`, `name`, `compose_yaml`, `env`, `secrets` 🔒, `secret_keys`, `current_revision` (the latest saved), `deployed_revision` and `deployed_at` (what runs), `source` (empty when made in the panel, `import` for a project imported from the host, `db_server` for the own stack of a shared database server), `status` (draft / deploying / deployed / failed / stopped / down / imported), `label_problems` (json: what was wrong with the `pocketcloud.*` labels at the last deploy, with lines), `labels_at` |
| `stack_revisions` | History of compose files | `stack`, `number`, `compose_yaml`, `env`, `env_keys[]`, `author`, `deployed_at`, `note` |
| `containers` | Inventory snapshot from the agent | `host`, `stack`, `project`, `service`, `docker_id`, `name`, `image`, `state`, `status_text`, `health`, `system_role`, `restart_count`, `ports`, `labels` (the value of `pocketcloud.auth` hidden), `started_at` |
| `volumes` | Inventory | `host`, `name`, `driver`, `size_bytes`, `labels` |
| `networks` | Inventory | `host`, `name`, `docker_id`, `driver`, `labels` |

Metrics are not a collection: they live in their own SQLite file with rollups, see [ADR 0003](adr/0003-metrics-storage.md).

## Sites, domains and TLS

| Collection | Purpose | Key fields |
|---|---|---|
| `sites` | Static website or PHP application (#21) | `name` (unique per server; names its directory), `host` (the edge that builds and serves it), `type` (static / php, fixed once the site exists), `source` (git / upload / empty), `repo_url` (HTTPS), `branch`, `git_token` 🔒 (with `git_token_set`), `generator` (auto / none / hugo / mkdocs / astro / npm / custom), `build_image`, `build_cmd`, `output_dir`, `build_cpus`, `build_memory_mb`, `keep_releases`; how the edge serves it: `https_redirect`, `hsts`, `compress`, `spa`, `not_found`, `headers` (json: path, name, value), `cache_rules` (json: path, max_age), `basic_auth` 🔒 (user:bcrypt-hash lines, with `basic_auth_users`, which the audit keeps out as well); `webhook_secret` 🔒, `active_deployment` (set by the panel only, with the deployment's state). The token and password are set with the site's create or update, or through `/api/pc/sites/{id}/credentials`; the webhook secret through `/webhook` |
| `site_php` | PHP settings of a site (1:1, #50), which only the panel writes: every change through `/api/pc/sites/{id}/php` is applied to the site's PHP-FPM | `site`, `php_version`, `template` (custom / wordpress / laravel / nette), `docroot` (in the release), `mode` (releases / classic), `fpm` (json: pm, max_children, max_requests, request_terminate_timeout), `php_ini` (lines), `env` (NAME=value lines, which may refer to `${secret:name}`), `secrets` 🔒 (a JSON object of the site's own secrets, set through `/php/secrets`), `secret_names`, `cpu_limit`, `mem_limit_mb`, `shared_paths` (json), `release_hook`, `database`, `queue_worker` (the template's worker, #52), `health_path` (asked after each release, such as `/up`), `backup_exclude` (json: paths of the site its backups leave out), `install` 🔒 (a fresh install's title, administrator and language until it is done; its password is a secret of the site meanwhile), `fresh_build` (the build of a fresh install's own release, such as `wp core download --locale=cs_CZ`; an upload or Git source of the site builds as any other); as the last apply left the container: `state` (pending / running / failed), `image` (pinned by its digest), `applied_at`, `last_error` |
| `deployments` | Build and release history | `site`, `number` (its release's directory), `status` (queued / building / succeeded / failed), `trigger` (create / manual / push / upload / update, #52), `source`, `branch`, `commit_sha`, `message`, `requested_by`, `command` (whose output is the build log), `generator`, `image`, `build_cmd`, `started_at`, `finished_at`, `build_ms`, `size_bytes`, `files`, `error`, `on_server` (its release is still there to go back to), `upload` (file, up to 256 MiB), `upload_sha256`, `php_version` (a PHP site's release was built for it, #50), `base_release` (the release a copy of which it built, as an update's or a fresh install's redeploy, #52), `pre_hook` (what ran before the release hook, as an update's), `health` (json: how the site answered at its health path once the release was live: `ok`, `status`, `url`, `error`, `checked_at`) |
| `scheduled_tasks` | Commands on a cron schedule in a PHP site's container or a stack's service (#51) | `target_kind` (site / stack), `site` (a PHP site), `stack`, `service`, `host` (set by the panel), `schedule` (UTC), `command`, `user` (stacks only; a PHP site runs it as www-data), `timeout_s` (default 300, at most 86400), `enabled`; the last run, kept by the panel: `last_run_at`, `last_status` (running / succeeded / failed / timed_out / skipped / cancelled), `last_exit_code`, `last_duration_ms`, `last_output` (its end), `last_command`, `failures` (in a row) |
| `domains` | Hostname → target, served by one edge | `hostname` (unique, lowercase), `host` (the edge), `target_kind` (container / redirect / site), `container`, `port`, `redirect_to`, `redirect_permanent`, `dns_alias` (another domain of the same server, not itself an alias: the zone gets a CNAME to it instead of addresses, which go first, as a CNAME stands alone; the aliased domain keeps its name and server, and its aliases go with it), `tls` (auto / off / internal: a certificate of the internal CA, #26), `https_redirect`, `hsts`, `compress`, `zone` (the zone that holds the name), `dns_status` (pending / ok / drift / manual / error), `dns_error`, `basic_auth` 🔒 (user:bcrypt-hash lines); from a Compose label (#25): `origin` (label, or empty when added by hand), `stack`, `service`, `label_removed` (the label is gone; the domain waits to be removed or kept); `site` (the site it serves: the site's options say how; removing the site removes it) |
| `dns_providers` | Connected DNS accounts | `kind` (cloudflare / hetzner / wedos / manual / adguard / pihole), `name`, `credentials` 🔒, `host` (for AdGuard Home and Pi-hole: the server whose agent reaches them, #26), `status`, `last_check`, `last_error`. Connected, changed and removed through `/api/pc/dns-providers` only |
| `dns_zones` | Zones discovered at providers, or named | `provider`, `name` (unique), `managed`, `status`, `last_check`, `last_error` |
| `dns_records` | Records created by PocketCloud | `zone`, `domain`, `name` (relative to the zone), `type`, `value`, `ttl`, `status`, `last_error`, `last_check`, `drift` (what the zone holds instead); unique by zone, name, type and value |
| `acme_accounts` | ACME registrations: a primary one, and a fallback one tried when it fails | `role` (primary / fallback, unique), `directory_url`, `email`, `key` 🔒, `eab_kid`, `eab_hmac` 🔒, `uri` (the account at the CA), `status`, `last_error`, `registered_at`. Set and removed through `/api/pc/acme-accounts/{role}` only, which registers them at the CA |
| `certificates` | Certificates the edges serve | `names[]`, `issuer` (manual / acme / internal), `not_before`, `not_after`, `status` (active / expired; pending / failed before an ACME certificate was first issued), `chain_pem`, `key_pem` 🔒, `fingerprint` (SHA-256 of the leaf, unique), `subject`, `issuer_name`, `installed` (json: host → file on that edge), `note`, `last_error`; from ACME (#23): `challenge` (auto / http-01 / dns-01), `auto` (ordered by the panel for a domain), `renew_after`, `attempts` (failures in a row), `next_attempt`, `attempt_started`, `history` (json: the last 20 attempts), `account` (primary / fallback: an admin asked that ACME CA alone, #27). Uploaded, requested from ACME and removed through `/api/pc/certificates` only |
| `internal_ca` | Internal CA material (single record, no API access at all) | `root_cert`, `root_key` 🔒, `agent_intermediate_cert`, `agent_intermediate_key` 🔒, `server_cert`, `server_key` 🔒, `server_names`, `server_expires_at`, `lan_intermediate_cert`, `lan_intermediate_key` 🔒 (signs the certificates of LAN names, #26) |

## Databases

| Collection | Purpose | Key fields |
|---|---|---|
| `db_servers` | A MariaDB or PostgreSQL server on a host: `shared`, which PocketCloud runs as a stack of its own, or `dedicated`, a database container inside an application's stack that someone linked | `host`, `name`, `engine` (mariadb / postgres), `version` (as the server reports it), `kind` (shared / dedicated), `image`, `container_ref` (container name), `service` (Compose service), `stack` (the server's own stack, or the application's for a dedicated server), `volume`, `port`, `admin_user`, `admin_password` 🔒, `max_connections`, `connections`, `extensions[]`, `status` (creating / ready / failed), `size_bytes`, `stats_at`, `last_error` |
| `databases` | One database on a server | `server`, `name`, `owner_kind` (none / stack / site), `stack`, `site` (a PHP site's, which keeps it when the site is removed), `size_bytes`, `connections`, `temporary_until` (reserved for restore tests, M4), `status` (creating / ready / failed / dropping), `last_error` |
| `db_users` | Database users created by PocketCloud, one per database, and the users of databases that moved from an application's own server | `server`, `name`, `password` 🔒, `pending_password` 🔒 (a new password until the server has taken it), `databases[]`, `secret_name` (`db/<database>`), `rotated_at`, `app_password` (the application keeps this password in its own settings, so the panel does not rotate it) |
| `db_networks` | A server on the internal network of an application | `server`, `name` (`pcdb-<stack>`: one network per application, which each of its servers joins), `stack`, `site` (`pcdb-site.<site>` for a PHP site's, removed with the site), `aliases[]` (the names of the application's own database service whose data moved to this server, which the server answers to on this network) |
| `db_dumps` | SQL exports, the final dumps of dropped databases and the copies of moved ones (admins only) | `server`, `database_name`, `kind` (export / final / move), `command`, `requested_by`, `file` (gzip, at most 1 GiB), `size_bytes`, `expires_at` (exports 7 days, final dumps and moves 30) |
| `db_moves` | A move of databases into a shared server: all of a stack's own server, or one database of a shared server for a newer version | `host`, `stack`, `target`, `target_name`, the source as it was (`source_name`, `source_kind`, `source_container`, `source_service`, `source_volume`, `source_version`), `databases[]` (name and user), `secrets` 🔒 (the passwords the databases keep, until the command is delivered), `status` (moving / moved / failed), `command`, `requested_by`, `error`, `moved_at`, `remove_after` (when the old container goes, 7 days on), `removed_at` |

The REST API only reads these collections (a move without its secrets), except that admins may delete a dump: the panel's own routes under `/api/pc/` write them, generate the passwords and queue the commands. A dedicated server's databases are listed from its statistics, without users, because they belong to their stack. Sizes and connection counts come from `db.stats`: once an hour, and when an operator opens a server on the Databases page or refreshes it, at most once a minute per server. There is no history of them.

## Backups

| Collection | Purpose | Key fields |
|---|---|---|
| `backup_targets` | Where backups go | `kind` (s3 / sftp / rest / local), `name`, `config`, `credentials` 🔒, `repo_password` 🔒, `capacity_bytes`, `used_bytes` |
| `backup_jobs` | What, when and how long | `name`, `host`, `source_kind` (volume / path / db_dump / database / site_shared / stack), `source`, `database`, `site`, `pre_hook`, `targets[]`, `schedule` (cron), `retention` (json), `enabled`, `origin` (manual / label / database / site) |
| `backup_runs` | One execution per target | `job`, `target`, `status` (succeeded / failed / skipped), `started_at`, `finished_at`, `bytes_added`, `snapshot_id`, `error`, `log` (file) |
| `backup_checks` | Repository integrity checks | `target`, `status`, `started_at`, `finished_at`, `output` |

Snapshots are not stored; they are listed on demand from the repository through the agent.

## Settings

Panel-wide settings live in a single `settings` record next to PocketBase's own settings (SMTP, S3 for files): `public_url`, `grpc_address`, `audit_retention_days`, `command_queue_ttl_s`, and the SMTP relay of PHP sites' mail (#51): `smtp_host` (empty: mail is off), `smtp_port`, `smtp_tls` (starttls / tls / off), `smtp_user`, `smtp_password` 🔒 (hidden; set through `/api/pc/settings/smtp`), `smtp_password_set`, `smtp_from` (the sender, required with a host). Defaults for renewal join them with their features.
