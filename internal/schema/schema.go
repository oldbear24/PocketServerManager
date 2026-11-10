// Package schema is the single source of truth for collection names, API rules
// and which fields hold secrets. See docs/data-model.md.
package schema

import "slices"

// Collections created by migrations.
const (
	Users             = "users"
	Superusers        = "_superusers"
	Hosts             = "hosts"
	RevokedCerts      = "revoked_certs"
	EnrollmentTokens  = "enrollment_tokens"
	Commands          = "commands"
	Events            = "events"
	Alerts            = "alerts"
	Stacks            = "stacks"
	StackRevisions    = "stack_revisions"
	Containers        = "containers"
	Volumes           = "volumes"
	Networks          = "networks"
	InternalCA        = "internal_ca"
	Settings          = "settings"
	AuditLog          = "audit_log"
	DiagnosticBundles = "diagnostic_bundles"
	DBServers         = "db_servers"
	Databases         = "databases"
	DBUsers           = "db_users"
	DBNetworks        = "db_networks"
	DBDumps           = "db_dumps"
	DBMoves           = "db_moves"
	Domains           = "domains"
	Certificates      = "certificates"
	DNSProviders      = "dns_providers"
	DNSZones          = "dns_zones"
	// BackupTargets are where backups go (#28)
	BackupTargets = "backup_targets"
	// BackupJobs are what is backed up, when and how long it is kept (#29)
	BackupJobs = "backup_jobs"
	// BackupRuns are the runs of a job, one per target (#29)
	BackupRuns = "backup_runs"
	// BackupRestores are restores and downloads of a job's snapshots: the
	// plan, its preview, what lets the panel give the target's secrets to
	// the restore's server, and the history (#30)
	BackupRestores = "backup_restores"
	// BackupChecks are the integrity checks of a target's repository: when,
	// what restic found, and the retries (#31). Restore tests (#53) are a
	// kind of check.
	BackupChecks   = "backup_checks"
	DNSRecords     = "dns_records"
	ACMEAccounts   = "acme_accounts"
	Sites          = "sites"
	Deployments    = "deployments"
	SitePHP        = "site_php"
	ScheduledTasks = "scheduled_tasks"
)

// What a scheduled task runs in (#51).
const (
	TaskTargetSite  = "site"
	TaskTargetStack = "stack"
)

// How a scheduled task's last run went.
const (
	TaskRunning   = "running"
	TaskSucceeded = "succeeded"
	TaskFailed    = "failed"
	TaskTimedOut  = "timed_out"
	TaskSkipped   = "skipped"
	// TaskCancelled is a run stopped by hand or never delivered: no failure of the task.
	TaskCancelled = "cancelled"
)

// States of a backup target (#28).
const (
	TargetPending = "pending"
	TargetOK      = "ok"
	TargetFailed  = "failed"
)

// What a backup job backs up (#29).
const (
	// SourceVolume is a Docker volume of the job's server.
	SourceVolume = "volume"
	// SourcePath is a directory of the job's server.
	SourcePath = "path"
	// SourceStack is every named volume of a stack.
	SourceStack = "stack"
	// SourceDump is a database dump made in a stack's service container.
	SourceDump = "db_dump"
)

// Database engines whose dumps a backup job makes (#29).
const (
	DumpPostgres = "postgres"
	DumpMySQL    = "mysql"
	DumpSQLite   = "sqlite"
)

// How a backup job came to be.
const (
	JobManual = "manual"
	// JobLabel is a job a stack's pocketcloud.backup label makes (OriginLabel).
	JobLabel = OriginLabel
)

// How a backup run, or a check of a target, started.
const (
	RunSchedule = "schedule"
	RunManual   = "manual"
	// RunRetry is a run or a check the panel started again after one that
	// failed (#31): its retry_of names the one before it.
	RunRetry = "retry"
)

// States of a backup run (#29).
const (
	RunQueued    = "queued"
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	// RunSkipped is a run that did not start: its server was offline at its
	// minute, the run before it was still going, or it waited its whole time,
	// in the panel's queue or for a free worker of the agent, and never
	// started.
	RunSkipped = "skipped"
	// RunCancelled is a run someone cancelled, or one not delivered because
	// its job no longer backs up to its target (the target left the job, or
	// the job's label went), or whose command is gone: no failure of the job.
	RunCancelled = "cancelled"
)

// Kinds of check of a target's repository (#31).
const (
	// CheckIntegrity is restic check: the structure, and one group of the
	// data packs.
	CheckIntegrity = "integrity"
	// CheckRestoreTest is reserved for #53's restore tests.
	CheckRestoreTest = "restore_test"
)

// States of a check of a target's repository (#31). A target's
// last_check_status takes them too; its integrity_status only ok and failed,
// from the newest check that completed.
const (
	// CheckQueued is a check that waits for its server's agent.
	CheckQueued = "queued"
	// CheckRunning is a check the agent runs.
	CheckRunning = "running"
	// CheckOK is a check that found no error, or whose errors the second
	// pass, under the exclusive lock, did not confirm.
	CheckOK = "ok"
	// CheckFailed is a check whose errors the second pass confirmed.
	CheckFailed = "failed"
	// CheckSuspect is a check whose first pass found errors that the
	// second could not confirm or refute: it could not lock the repository,
	// or it failed.
	CheckSuspect = "suspect"
	// CheckError is a check that did not complete: no repository, a wrong
	// password, an agent that restarted, a check that ran out of time.
	CheckError = "error"
	// CheckSkipped is a check that did not start: its server was offline,
	// its agent cannot check repositories, or the check before it was still
	// going.
	CheckSkipped = "skipped"
	// CheckCancelled is a check someone stopped.
	CheckCancelled = "cancelled"
)

// CheckStates lists the states of a check.
var CheckStates = []string{
	CheckQueued, CheckRunning, CheckOK, CheckFailed, CheckSuspect, CheckError, CheckSkipped, CheckCancelled,
}

// The default schedule and groups of the integrity checks (#31), which admins
// change in the settings: every Sunday at 05:00 UTC, one of 20 groups of the
// data packs each time.
const (
	DefaultCheckSchedule = "0 5 * * 0"
	DefaultCheckGroups   = 20
)

// States of a restore or a download of a snapshot (#30). A restore is a
// draft while it is previewed, until someone starts it; a download starts
// queued.
const (
	// RestorePreviewing is a draft whose preview the agent is making.
	RestorePreviewing = "previewing"
	// RestorePreviewed is a draft whose preview is ready, for 30 minutes.
	RestorePreviewed = "previewed"
	// RestoreQueued is a restore someone started, or a download, that waits
	// for its server.
	RestoreQueued = "queued"
	// RestoreRunning is a restore or a download the agent runs.
	RestoreRunning = "running"
	// RestoreSucceeded is a restore or a download that ended well.
	RestoreSucceeded = "succeeded"
	// RestoreFailed is a restore, a download or a preview that failed, or
	// one that never started.
	RestoreFailed = "failed"
	// RestoreCancelled is a restore or a download someone stopped, or one
	// stopped before it started.
	RestoreCancelled = "cancelled"
	// RestoreExpired is a draft whose preview is older than 30 minutes.
	RestoreExpired = "expired"
)

// RestoreStates lists the states of a restore, in the order of its life.
var RestoreStates = []string{
	RestorePreviewing, RestorePreviewed, RestoreQueued, RestoreRunning,
	RestoreSucceeded, RestoreFailed, RestoreCancelled, RestoreExpired,
}

// Where a restore of a snapshot goes (#30).
const (
	// RestoreOriginal puts the job's data back where it was backed up from,
	// on the job's server: the containers that use it stop for a while.
	RestoreOriginal = "original"
	// RestorePath writes a folder of the snapshot into a new folder of any
	// server.
	RestorePath = "path"
	// RestoreDownload streams a folder of the snapshot as a tar, or one file
	// as it is, to a browser.
	RestoreDownload = "download"
)

// What a download of a snapshot sends (#30).
const (
	// DownloadDir is a folder of the snapshot, as a tar.
	DownloadDir = "dir"
	// DownloadFile is one file of the snapshot, as it is.
	DownloadFile = "file"
)

// Reserved names of collections that land with later features. They are listed
// here so that nothing else takes them; see docs/data-model.md for their fields.
var Reserved = []string{
	"api_tokens", "alert_rules", "notification_channels",
}

// User roles.
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// API rules. Superusers bypass them, and agents never use the REST API.
const (
	RuleViewer   = `@request.auth.role = "viewer" || @request.auth.role = "operator" || @request.auth.role = "admin"`
	RuleOperator = `@request.auth.role = "operator" || @request.auth.role = "admin"`
	RuleAdmin    = `@request.auth.role = "admin"`
)

// Host roles.
const (
	HostRoleDocker = "docker"
	HostRoleEdge   = "edge"
	HostRoleBackup = "backup"
)

// Host statuses.
const (
	HostOnline   = "online"
	HostDegraded = "degraded"
	HostOffline  = "offline"
)

// Command statuses.
const (
	CommandQueued     = "queued"
	CommandDispatched = "dispatched"
	CommandRunning    = "running"
	CommandSucceeded  = "succeeded"
	CommandFailed     = "failed"
	CommandTimedOut   = "timed_out"
	CommandCancelled  = "cancelled"
)

// Database engines and kinds of database servers.
const (
	EngineMariaDB  = "mariadb"
	EnginePostgres = "postgres"

	// DBServerShared is a server PocketCloud runs for the applications of a host.
	DBServerShared = "shared"
	// DBServerDedicated is a database container found inside an application's stack.
	DBServerDedicated = "dedicated"
)

// Kinds of database dumps kept by the panel.
const (
	DumpExport = "export"
	DumpFinal  = "final"
	// DumpMove is the copy of a database as it was when it moved to another server.
	DumpMove = "move"
)

// States of a move of databases to another server.
const (
	MoveRunning = "moving"
	MoveDone    = "moved"
	MoveFailed  = "failed"
)

// Targets of a domain on an edge.
const (
	TargetContainer = "container"
	TargetRedirect  = "redirect"
	TargetSite      = "site"
)

// Types of a site: static files (#21), or a PHP application served by a
// PHP-FPM container of its own (#50).
const (
	SiteStatic = "static"
	SitePHPApp = "php"
)

// Modes of a PHP site: releases keep its code read-only in releases, with
// rollback; classic keeps one writable copy, for applications that update
// themselves, without release history.
const (
	PHPReleases = "releases"
	PHPClassic  = "classic"
)

// Templates of a PHP site. custom is code of one's own; the frameworks'
// templates are internal/phpapps' (#52).
const (
	PHPCustom    = "custom"
	PHPWordPress = "wordpress"
	PHPLaravel   = "laravel"
	PHPNette     = "nette"
)

// States of a PHP site's PHP-FPM container.
const (
	PHPPending = "pending"
	PHPRunning = "running"
	PHPFailed  = "failed"
)

// States of a site's deployment.
const (
	DeploymentQueued    = "queued"
	DeploymentBuilding  = "building"
	DeploymentSucceeded = "succeeded"
	DeploymentFailed    = "failed"
)

// What started a deployment.
const (
	TriggerCreate = "create"
	TriggerManual = "manual"
	TriggerPush   = "push"
	TriggerUpload = "upload"
	// TriggerUpdate is an update of a PHP application, such as WordPress's (#52)
	TriggerUpdate = "update"
)

// TLS of a domain: auto serves it over HTTPS with a certificate that is not
// from the internal CA, from ACME unless one was uploaded; internal, with one
// from the internal CA, for a LAN name (#26).
const (
	TLSAuto     = "auto"
	TLSOff      = "off"
	TLSInternal = "internal"
)

// OriginLabel marks a domain that the pocketcloud.domain label of a stack's
// service asks for (#25); a domain without an origin was added by hand.
const OriginLabel = "label"

// DNS states. A provider and a zone are ok once their API answered; a
// domain's records are pending until the provider took them, ok once every
// resolver answers them, drift when someone changed them outside PocketCloud,
// manual when no connected zone holds the name, and none when it needs none.
const (
	DNSUnknown = "unknown"
	DNSOK      = "ok"
	DNSError   = "error"
	DNSPending = "pending"
	DNSDrift   = "drift"
	DNSManual  = "manual"
	DNSNone    = "none"
)

// Issuers and states of certificates.
const (
	IssuerManual   = "manual"
	IssuerACME     = "acme"
	IssuerInternal = "internal"

	CertActive  = "active"
	CertExpired = "expired"
	// an ACME certificate before its first issuance, and after that failed
	CertPending = "pending"
	CertFailed  = "failed"
)

// ACME accounts: the primary one issues, the fallback one when the primary
// one fails; and the challenges an ACME certificate can be checked with.
const (
	ACMEPrimary  = "primary"
	ACMEFallback = "fallback"

	ACMEOK    = "ok"
	ACMEError = "error"

	ChallengeAuto   = "auto"
	ChallengeHTTP01 = "http-01"
	ChallengeDNS01  = "dns-01"
)

// Severity of events and alerts.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Alert statuses.
const (
	AlertOpen         = "open"
	AlertAcknowledged = "acknowledged"
	AlertResolved     = "resolved"
)

// Encrypted lists the fields marked 🔒 in docs/data-model.md. They are text
// fields hidden from the API, sealed with the master key before they are stored
// and never written to the audit log. Collections of later milestones are listed
// too, so a new collection cannot be added without its secrets being covered.
var Encrypted = map[string][]string{
	Stacks:                  {"secrets"},
	InternalCA:              {"root_key", "agent_intermediate_key", "server_key", "lan_intermediate_key"},
	"notification_channels": {"config"},
	Sites:                   {"webhook_secret", "basic_auth", "git_token"},
	SitePHP:                 {"secrets"},
	Settings:                {"smtp_password"},
	DNSProviders:            {"credentials"},
	Domains:                 {"basic_auth"},
	ACMEAccounts:            {"key", "eab_hmac"},
	Certificates:            {"key_pem"},
	DBServers:               {"admin_password"},
	DBUsers:                 {"password", "pending_password"},
	DBMoves:                 {"secrets"},
	BackupTargets:           {"credentials", "repo_password"},
}

// AuditedCollection describes how changes of one collection are audited.
type AuditedCollection struct {
	// Name is the collection name.
	Name string
	// Label is the field used as the record's human-readable label.
	Label string
	// Host is the relation field that links a record to a host, if any.
	Host string
	// Ignore lists fields that are not configuration, such as live status.
	// A save that changes nothing else produces no audit entry.
	Ignore []string
	// Revision is the number field that counts the revisions of a record, if any.
	// An entry of a save that moves it says from which revision to which.
	Revision string
	// Redact lists fields that may be read but are made from a secret: the
	// audit only says that they changed, as it does for the secret.
	Redact []string
}

// Audited lists the configuration collections covered by the change audit.
// High-volume or derived data (inventory, metrics, events, diagnostics) is not
// audited, and commands are shown from the commands collection.
var Audited = []AuditedCollection{
	{Name: Users, Label: "email", Ignore: []string{"lastLogin"}},
	{Name: Hosts, Label: "name", Ignore: []string{
		"status", "os", "kernel", "arch", "cpu_cores", "mem_bytes", "ip4", "ip6",
		"agent_version", "agent_image_digest", "agent_container_id", "docker_version",
		"last_seen", "rtt_ms", "clock_skew_ms", "connected_since", "stream_open",
		"uptime_s", "load1", "load5", "load15", "cpu_percent", "mem_used_bytes", "disk_used_bytes", "disk_total_bytes",
		"runtime", "self_checks", "inventory_at", "capabilities", "compose_version",
		"cert_serial", "cert_prev_serial", "cert_expires_at", "hostname",
		"edge_digest", "edge_revision", "edge_applied_digest", "edge_applied_revision",
		"edge_running", "edge_error", "edge_requests_per_s", "edge_caddy_version",
	}},
	{Name: Stacks, Label: "name", Host: "host", Revision: "current_revision", Ignore: []string{"status", "current_revision", "deployed_revision", "deployed_at", "label_problems", "labels_at"}},
	{Name: Settings, Label: "public_url"},
	{Name: "api_tokens", Label: "name", Ignore: []string{"last_used_at", "last_used_ip"}},
	{Name: "alert_rules", Label: "kind"},
	{Name: "notification_channels", Label: "kind"},
	{Name: Sites, Label: "name", Host: "host", Ignore: []string{"active_deployment"}, Redact: []string{"basic_auth_users"}},
	{Name: SitePHP, Label: "site", Ignore: []string{"state", "image", "applied_at", "last_error"}},
	{Name: ScheduledTasks, Label: "command", Ignore: []string{
		"host", "last_run_at", "last_status", "last_exit_code", "last_duration_ms", "last_output", "last_command", "failures",
	}},
	{Name: Domains, Label: "hostname", Host: "host", Ignore: []string{"zone", "dns_status", "dns_error", "label_removed"}},
	{Name: DNSProviders, Label: "name", Ignore: []string{"status", "last_check", "last_error"}},
	{Name: DNSZones, Label: "name", Ignore: []string{"status", "last_check", "last_error"}},
	{Name: Certificates, Label: "names", Ignore: []string{
		"status", "attempts", "last_error", "installed", "renew_after", "next_attempt", "attempt_started", "history",
	}},
	{Name: ACMEAccounts, Label: "directory_url", Ignore: []string{"status", "last_error", "uri", "registered_at"}},
	{Name: DBServers, Label: "name", Host: "host", Ignore: []string{
		"status", "size_bytes", "connections", "max_connections", "version", "stats_at", "last_error", "container_ref",
	}},
	{Name: Databases, Label: "name", Ignore: []string{"status", "size_bytes", "connections", "last_error"}},
	{Name: DBUsers, Label: "name"},
	{Name: DBNetworks, Label: "name"},
	{Name: DBMoves, Label: "source_name", Host: "host", Ignore: []string{"status", "error", "moved_at", "removed_at", "command"}},
	{Name: BackupTargets, Label: "name", Ignore: []string{
		"status", "last_error", "checked_at", "repo_id", "capacity_bytes", "used_bytes", "usage_at",
		// how its integrity checks go (#31)
		"integrity_status", "integrity_at", "integrity_error", "last_check", "last_check_status",
		"last_check_error", "next_check_at", "integrity_group",
	}},
	{Name: BackupJobs, Label: "name", Host: "host", Ignore: []string{"next_run_at", "last_run_at", "last_status", "problem", "label_removed"}},
}

// IsEncrypted reports whether a field holds a secret.
func IsEncrypted(collection, field string) bool {
	return slices.Contains(Encrypted[collection], field)
}

// IsRedacted reports whether the audit keeps a field's values out, as those
// of a secret.
func IsRedacted(collection, field string) bool {
	for _, audited := range Audited {
		if audited.Name == collection {
			return slices.Contains(audited.Redact, field)
		}
	}
	return false
}
