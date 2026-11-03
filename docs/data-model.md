# Data model (proposal)

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

`audit_log` has no create, update or delete API rules; entries are written only by the audit writer, and hooks reject every other write, for superusers too (retention excepted, see [Change audit](architecture.md#change-audit)). Admins read everything; operators only see the category `change`.

API rules in short: viewers read, operators manage workloads, admins do everything. Collections the panel itself maintains (`containers`, `events`, `stack_revisions` and others) cannot be written through the API at all.

## Hosts and agents

| Collection | Purpose | Key fields |
|---|---|---|
| `hosts` | A managed server and its agent | `name`, `roles[]` (docker / edge / backup), `status` (online / degraded / offline), `hostname`, `os`, `kernel`, `arch`, `cpu_cores`, `mem_bytes`, `ip4`, `ip6`, `labels`, `agent_version`, `agent_image_digest`, `agent_container_id`, `capabilities`, `docker_version`, `last_seen`, `rtt_ms`, `clock_skew_ms`, `connected_since`, `stream_open`, `uptime_s`, `load1`, `cpu_percent`, `mem_used_bytes`, `disk_used_bytes`, `disk_total_bytes`, `runtime` (json), `self_checks` (json), `inventory_at`, `cert_serial`, `cert_prev_serial`, `cert_expires_at`, `host_shell_enabled` |
| `revoked_certs` | Agent certificate denylist | `serial`, `host`, `host_name`, `reason` |
| `commands` | Audit trail and delivery queue. A payload holds no secrets: they are read when the command is delivered | `host`, `host_name`, `type`, `label`, `payload` (json), `status` (queued / dispatched / running / succeeded / failed / timed_out / cancelled), `requested_by`, `requested_label`, `resource` (ordering key), `parent`, `correlation_id`, `attempts`, `last_sequence`, `timeout_s`, `dispatched_at`, `started_at`, `finished_at`, `expires_at`, `progress` (json), `result` (json), `error`, `output` (file) |
| `events` | Timeline per host and resource; removed after 30 days | `host`, `kind`, `severity`, `message`, `data` |
| `alerts` | Open problems | `rule`, `target`, `host`, `severity`, `status` (open / acknowledged / resolved), `first_seen`, `last_seen`, `resolved_at`, `message`, `acknowledged_by` |
| `alert_rules` | Thresholds and channels | `kind`, `params`, `channels[]`, `enabled` |
| `notification_channels` | E-mail, ntfy, Gotify, webhook | `kind`, `config` 🔒 |
| `diagnostic_bundles` | Debug bundles from agents | `host`, `requested_by`, `command`, `file`, `size_bytes`, `expires_at` |

Live diagnostics (connection stats, message trace, queue latencies) are kept in memory by the diagnostics plugin and are not stored in collections.

## Containers and stacks

| Collection | Purpose | Key fields |
|---|---|---|
| `stacks` | Compose project on a host | `host`, `name`, `compose_yaml`, `env`, `secrets` 🔒, `secret_keys`, `current_revision`, `status` |
| `stack_revisions` | History of compose files | `stack`, `number`, `compose_yaml`, `env`, `env_keys[]`, `author`, `deployed_at`, `note` |
| `containers` | Inventory snapshot from the agent | `host`, `stack`, `project`, `service`, `docker_id`, `name`, `image`, `state`, `status_text`, `health`, `system_role`, `restart_count`, `ports`, `labels`, `started_at` |
| `volumes` | Inventory | `host`, `name`, `driver`, `size_bytes`, `labels` |
| `networks` | Inventory | `host`, `name`, `docker_id`, `driver`, `labels` |

Metrics are not a collection: they live in their own SQLite file with rollups, see [ADR 0003](adr/0003-metrics-storage.md).

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
| `internal_ca` | Internal CA material (single record, no API access at all) | `root_cert`, `root_key` 🔒, `agent_intermediate_cert`, `agent_intermediate_key` 🔒, `server_cert`, `server_key` 🔒, `server_names`, `server_expires_at` |

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

Panel-wide settings live in a single `settings` record next to PocketBase's own settings (SMTP, S3 for files): `public_url`, `grpc_address`, `audit_retention_days`, `command_queue_ttl_s`. Defaults for renewal, the SMTP relay used by PHP sites and supported PHP versions with their end-of-life dates join them with their features.
