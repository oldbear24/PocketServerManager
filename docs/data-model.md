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
| `enrollment_tokens` | One-time tokens for connecting servers | `token_hash`, `name`, `roles[]`, `expires_at`, `used_at`, `created_by`, `host` |
| `audit_log` | Sensitive actions | `actor`, `action`, `target`, `data`, `ip` |

## Hosts and agents

| Collection | Purpose | Key fields |
|---|---|---|
| `hosts` | A managed server and its agent | `name`, `roles[]` (docker / edge / backup), `status` (online / degraded / offline), `os`, `arch`, `cpu_cores`, `mem_bytes`, `ip4`, `ip6`, `labels`, `agent_version`, `docker_version`, `last_seen`, `rtt_ms`, `connected_since`, `cert_serial`, `cert_expires_at` |
| `revoked_certs` | Agent certificate denylist | `serial`, `host`, `reason` |
| `commands` | Audit trail and delivery queue | `host`, `type`, `payload` (json, 🔒 parts), `status`, `requested_by`, `parent`, `correlation_id`, `attempts`, `dispatched_at`, `started_at`, `finished_at`, `expires_at`, `result` (json), `error`, `output` (file) |
| `events` | Timeline per host and resource | `host`, `kind`, `severity`, `message`, `data` |
| `alerts` | Open problems | `rule`, `target`, `severity`, `status` (open / acknowledged / resolved), `first_seen`, `last_seen`, `message`, `acknowledged_by` |
| `alert_rules` | Thresholds and channels | `kind`, `params`, `channels[]`, `enabled` |
| `notification_channels` | E-mail, ntfy, Gotify, webhook | `kind`, `config` 🔒 |

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
| `sites` | Static website | `name`, `host`, `source_type` (git / upload / empty), `repo_url`, `branch`, `build_image`, `build_cmd`, `output_dir`, `options` (https redirect, HSTS, SPA fallback, basic auth 🔒, headers), `active_deployment`, `webhook_secret` 🔒 |
| `deployments` | Build and release history | `site`, `number`, `status`, `commit_sha`, `message`, `trigger`, `started_at`, `finished_at`, `size_bytes`, `log` (file), `upload` (file) |
| `domains` | Hostname → target | `hostname`, `target_kind` (site / container / redirect), `site`, `container_ref`, `redirect_to`, `zone`, `dns_status`, `certificate` |
| `dns_providers` | Connected DNS accounts | `kind` (cloudflare / hetzner / wedos / adguard / pihole / manual), `name`, `credentials` 🔒, `status`, `last_check`, `last_error` |
| `dns_zones` | Zones discovered at providers | `provider`, `name`, `managed` |
| `dns_records` | Records created by PocketCloud | `zone`, `type`, `name`, `value`, `ttl`, `provider_record_id`, `domain`, `status` |
| `acme_accounts` | ACME registrations | `directory_url`, `email`, `key` 🔒, `eab_kid`, `eab_hmac` 🔒 |
| `certificates` | Issued certificates | `names[]`, `issuer`, `challenge`, `not_before`, `not_after`, `status`, `renew_after`, `attempts`, `last_error`, `chain_pem`, `key_pem` 🔒, `hosts[]` |
| `internal_ca` | Internal CA material (single record) | `root_cert`, `root_key` 🔒, `agent_intermediate_cert`, `agent_intermediate_key` 🔒 |

## Backups

| Collection | Purpose | Key fields |
|---|---|---|
| `backup_targets` | Where backups go | `kind` (s3 / sftp / rest / local), `name`, `config`, `credentials` 🔒, `repo_password` 🔒, `capacity_bytes`, `used_bytes` |
| `backup_jobs` | What, when and how long | `name`, `host`, `source_kind` (volume / path / db_dump / stack), `source`, `pre_hook`, `targets[]`, `schedule` (cron), `retention` (json), `enabled`, `origin` (manual / label) |
| `backup_runs` | One execution per target | `job`, `target`, `status` (succeeded / failed / skipped), `started_at`, `finished_at`, `bytes_added`, `snapshot_id`, `error`, `log` (file) |
| `backup_checks` | Repository integrity checks | `target`, `status`, `started_at`, `finished_at`, `output` |

Snapshots are not stored; they are listed on demand from the repository through the agent.

## Settings

Panel-wide settings (public URL, gRPC address, defaults for retention and renewal) live in a single `settings` record next to PocketBase's own settings (SMTP, S3 for files).
