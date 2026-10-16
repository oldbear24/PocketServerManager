# Data model (proposal)

PocketBase collections, created by Go migrations (see the schema issue in milestone M0). Names and fields are a starting point; `docs/data-model.md` is updated together with the migrations.

Conventions:

- Every collection has PocketBase's `id`, `created` and `updated`.
- Fields marked 🔒 are encrypted with the master key before they are stored.
- Agents never use the REST API; everything agents report arrives over gRPC and is written by the panel.

## Identity and access

| Collection | Purpose | Key fields |
|---|---|---|
| `users` (auth, built-in) | People using the panel | `role` (admin / operator / viewer), MFA settings |
| `api_tokens` | Scoped tokens for automation (CI) | `name`, `token_hash`, `scopes[]`, `expires_at`, `last_used_at`, `last_used_ip`, `created_by`, `revoked_at` |
| `enrollment_tokens` | One-time tokens for connecting servers | `token_hash`, `name`, `roles[]`, `expires_at`, `used_at`, `created_by`, `host` |
| `audit_log` | Append-only change and security log | `seq`, `category` (change / security), `action` (create / update / delete / revert / sign_in / …), `actor_kind` (user / token / superuser / system), `actor`, `actor_label`, `source` (ui / api / pb_admin / scheduler / agent / acme), `ip`, `user_agent`, `collection`, `record_id`, `record_label`, `host`, `changes` (json: field → from / to, secrets only as “changed”), `snapshot` (json, on delete), `command`, `revision`, `note`, `prev_hash`, `hash` |

`audit_log` has no create, update or delete API rules; entries are written only by the audit writer, and hooks reject updates and deletions (retention excepted, see [Change audit](architecture.md#change-audit)).

## Hosts and agents

| Collection | Purpose | Key fields |
|---|---|---|
| `hosts` | A managed server and its agent | `name`, `roles[]` (docker / edge / backup), `status` (online / degraded / offline), `os`, `arch`, `cpu_cores`, `mem_bytes`, `ip4`, `ip6`, `labels`, `agent_version`, `agent_image_digest`, `agent_container_id`, `docker_version`, `last_seen`, `rtt_ms`, `connected_since`, `cert_serial`, `cert_expires_at` |
| `revoked_certs` | Agent certificate denylist | `serial`, `host`, `reason` |
| `commands` | Audit trail and delivery queue | `host`, `type`, `payload` (json, 🔒 parts), `status`, `requested_by`, `parent`, `correlation_id`, `attempts`, `dispatched_at`, `started_at`, `finished_at`, `expires_at`, `result` (json), `error`, `output` (file) |
| `events` | Timeline per host and resource | `host`, `kind`, `severity`, `message`, `data` |
| `alerts` | Open problems | `rule`, `target`, `severity`, `status` (open / acknowledged / resolved), `first_seen`, `last_seen`, `message`, `acknowledged_by` |
| `alert_rules` | Thresholds and channels | `kind`, `params`, `channels[]`, `enabled` |
| `notification_channels` | E-mail, ntfy, Gotify, webhook | `kind`, `config` 🔒 |
| `diagnostic_bundles` | Debug bundles from agents or the panel | `host` (empty = panel), `requested_by`, `file`, `size_bytes`, `expires_at` |

Live diagnostics (connection stats, message trace, queue latencies) are kept in memory by the diagnostics plugin and are not stored in collections.

## Containers and stacks

| Collection | Purpose | Key fields |
|---|---|---|
| `stacks` | Compose project on a host | `host`, `name`, `compose_yaml`, `env`, `secrets` 🔒, `current_revision`, `status` |
| `stack_revisions` | History of compose files | `stack`, `number`, `compose_yaml`, `env_keys[]`, `author`, `deployed_at`, `note` |
| `containers` | Inventory snapshot from the agent | `host`, `stack`, `docker_id`, `name`, `image`, `state`, `health`, `restart_count`, `ports`, `labels`, `started_at` |
| `volumes` | Inventory | `host`, `name`, `driver`, `size_bytes`, `labels` |
| `networks` | Inventory | `host`, `name`, `driver`, `labels` |
| `metrics` | Time-series samples **(spike: storage)** | `host`, `container`, `resolution` (10s / 1m / 1h), `ts`, `cpu`, `mem`, `disk`, `net_in`, `net_out` |

## Sites, domains and TLS

| Collection | Purpose | Key fields |
|---|---|---|
| `sites` | Static website or PHP application | `name`, `host`, `type` (static / php), `source_type` (git / upload / empty / template), `repo_url`, `branch`, `build_image`, `build_cmd`, `output_dir` (static) or `doc_root` (PHP), `options` (https redirect, HSTS, SPA fallback, basic auth 🔒, headers), `active_deployment`, `webhook_secret` 🔒 |
| `site_php` | PHP settings of a site (1:1) | `site`, `php_version`, `template` (wordpress / laravel / nette / custom), `image`, `mode` (releases / classic), `fpm` (json: pm, max_children, max_requests), `php_ini`, `env` (json, may reference secrets), `secrets` 🔒, `cpu_limit`, `mem_limit`, `shared_paths[]`, `release_hook`, `database` |
| `deployments` | Build and release history | `site`, `number`, `status`, `commit_sha`, `message`, `trigger`, `php_version`, `started_at`, `finished_at`, `size_bytes`, `log` (file), `upload` (file) |
| `scheduled_tasks` | Cron commands in a site or stack container | `target_kind` (site / stack), `site`, `stack`, `service`, `schedule`, `command`, `timeout_s`, `enabled`, `last_run_at`, `last_status`, `last_duration_ms`, `last_output` |
| `domains` | Hostname → target | `hostname`, `target_kind` (site / container / redirect), `site`, `container_ref`, `redirect_to`, `zone`, `dns_status`, `certificate` |
| `dns_providers` | Connected DNS accounts | `kind` (cloudflare / hetzner / wedos / adguard / pihole / manual), `name`, `credentials` 🔒, `status`, `last_check`, `last_error` |
| `dns_zones` | Zones discovered at providers | `provider`, `name`, `managed` |
| `dns_records` | Records created by PocketCloud | `zone`, `type`, `name`, `value`, `ttl`, `provider_record_id`, `domain`, `status` |
| `acme_accounts` | ACME registrations | `directory_url`, `email`, `key` 🔒, `eab_kid`, `eab_hmac` 🔒 |
| `certificates` | Issued certificates | `names[]`, `issuer`, `challenge`, `not_before`, `not_after`, `status`, `renew_after`, `attempts`, `last_error`, `chain_pem`, `key_pem` 🔒, `hosts[]` |
| `internal_ca` | Internal CA material (single record) | `root_cert`, `root_key` 🔒, `agent_intermediate_cert`, `agent_intermediate_key` 🔒 |

## Databases

| Collection | Purpose | Key fields |
|---|---|---|
| `db_servers` | MariaDB / PostgreSQL server on a host | `name`, `host`, `engine` (mariadb / postgres), `version`, `kind` (shared / dedicated), `image`, `container_ref`, `stack` (dedicated), `volume`, `admin_user`, `admin_password` 🔒, `max_connections`, `extensions[]`, `status`, `size_bytes` |
| `databases` | One database on a server | `server`, `name`, `owner_kind` (site / stack / none), `site`, `stack`, `size_bytes`, `connections`, `backup_job`, `temporary_until` (restore tests, clones), `status` |
| `db_users` | Database users created by PocketCloud | `server`, `name`, `password` 🔒, `databases[]`, `secret_name` (e.g. `db/shop`), `rotated_at` |
| `db_networks` | Private networks between apps and servers | `server`, `name` (`pcdb-<app>`), `site`, `stack` |

Sizes and connection counts are refreshed by the agent; history lives in `metrics`.

## Backups

| Collection | Purpose | Key fields |
|---|---|---|
| `backup_targets` | Where backups go | `kind` (s3 / sftp / rest / local), `name`, `config`, `credentials` 🔒, `repo_password` 🔒, `capacity_bytes`, `used_bytes` |
| `backup_jobs` | What, when and how long | `name`, `host`, `source_kind` (volume / path / db_dump / database / site_shared / stack), `source`, `database`, `site`, `pre_hook`, `targets[]`, `schedule` (cron), `retention` (json), `enabled`, `origin` (manual / label / database / site) |
| `backup_runs` | One execution per target | `job`, `target`, `status` (succeeded / failed / skipped), `started_at`, `finished_at`, `bytes_added`, `snapshot_id`, `error`, `log` (file) |
| `backup_checks` | Repository integrity checks | `target`, `status`, `started_at`, `finished_at`, `output` |

Snapshots are not stored; they are listed on demand from the repository through the agent.

## Settings

Panel-wide settings (public URL, gRPC address, defaults for retention and renewal, `audit.retention_days`, the SMTP relay used by PHP sites, supported PHP versions with their end-of-life dates) live in a single `settings` record next to PocketBase's own settings (SMTP, S3 for files).
