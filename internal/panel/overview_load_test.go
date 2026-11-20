package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// The Overview (#33) loads in under a second with 20 servers (AC2). This test
// times each read the page and its Layout make on a cold load, through the
// panel's routes as a viewer, on a panel with 20 servers and the history of a
// busy one: what the panel spends answering, without the network or the
// browser, which the real check on a VM measures.

// The reads, as the browser sends them. They must stay equal to what
// web/src/lib/overview.ts's overviewQueries builds for the page and what
// web/src/components/Layout.tsx's lists ask for (taken from the spec of #33,
// §4.2 and §4.3): a change of one is a change of the other.
const (
	// hostSummaryFields is web/src/lib/hosts.tsx's: Layout's one list of the
	// servers, which the Overview shares.
	hostSummaryFields = "id,collectionId,collectionName,updated,name,roles,status,stream_open,ip4,public_ip4,os,cpu_percent," +
		"mem_used_bytes,mem_bytes,disk_used_bytes,disk_total_bytes,rtt_ms,last_seen,agent_version"
	// openAlertsFields are web/src/lib/alerts.tsx's openAlertsQuery's:
	// Layout's one list of the open alerts, which the Overview shares.
	openAlertsFields = "id,collectionId,collectionName,rule,target,host,severity,status,first_seen,last_seen,message,updated"
	// sectionAlertsFields are web/src/lib/alerts.tsx's sectionAlertsQuery's,
	// and the filters below its filters for web/src/lib/events.ts's
	// domainAlertRules and backupAlertRules: the open alerts of the
	// sidebar's Domains & TLS and Backups, for their dots.
	sectionAlertsFields = "id,collectionId,collectionName,severity"
	domainAlertsFilter  = "status != 'resolved' && (rule = 'certificate_failed' || rule = 'certificate_expiring' || rule = 'dns_provider_failed')"
	backupAlertsFilter  = "status != 'resolved' && (rule = 'backup_failed' || rule = 'backup_skipped' || rule = 'backup_retention'" +
		" || rule = 'backup_check_failed' || rule = 'backup_check_unverified' || rule = 'panel_backup'" +
		" || rule = 'backup_restore_test_failed' || rule = 'backup_restore_test_unverified' || rule = 'db_temporary_left')"
	// overviewRunFields is web/src/lib/backupJobs.ts's.
	overviewRunFields = "id,created,updated,job,target,host,status,trigger,scheduled_at,started_at,finished_at,total_bytes,error,command,retry_of"
	// overviewQueueFilter is web/src/lib/overview.ts's queueFilter: the
	// command queue without reads, measurements and the steps of other
	// commands.
	overviewQueueFilter = "parent = '' && resource !~ 'backup-usage:%' && type !~ 'logs.%' && type !~ 'exec.%'" +
		" && type !~ 'acme.challenge.%' && type !~ 'db.adminer.%' && type != 'site.php.status'" +
		" && type != 'site.php.mail' && type != 'site.detect' && type != 'stack.validate'" +
		" && type != 'db.stats' && type != 'backup.snapshots' && type != 'backup.browse'"
)

// pbFilterTime is a time as the SDK's pb.filter writes a Date into a filter.
func pbFilterTime(at time.Time) string {
	return "'" + at.UTC().Format("2006-01-02 15:04:05.000Z") + "'"
}

// overviewRead is one read of a cold load: a page of a list, every page of
// one as the SDK's getFullList reads them for useLiveRecords, or a route.
type overviewRead struct {
	name string
	// the list's collection and its query, without the page
	collection string
	query      url.Values
	perPage    int
	// every page, as getFullList reads them
	all bool
	// a route that is no list
	route string
	// it has a filter: PocketBase holds up a list of a collection whose
	// filter finds nothing, after 3 of them in 3 seconds from one address
	filtered bool
}

// listRead is a read of the first page of a list, with the options the page
// gives useLiveList; empty ones are not sent, as the SDK sends none.
func listRead(name, collection string, perPage int, options map[string]string) overviewRead {
	query := url.Values{}
	for key, value := range options {
		if value != "" {
			query.Set(key, value)
		}
	}
	return overviewRead{name: name, collection: collection, query: query, perPage: perPage, filtered: options["filter"] != ""}
}

// fullListRead is a read of every record a filter matches, as useLiveRecords
// makes it: getFullList, in pages of 1000 without their count.
func fullListRead(name, collection string, options map[string]string) overviewRead {
	read := listRead(name, collection, 1000, options)
	read.query.Set("skipTotal", "1")
	read.all = true
	return read
}

// overviewReads are the reads of a cold load of the Overview at now, with
// the servers that are offline, whose last disconnect the page looks up, and
// with an open alert of a DNS provider, whose providers it then reads.
func overviewReads(now time.Time, offline []string) []overviewRead {
	reads := []overviewRead{
		// Layout: its lists and the panel's facts, on every page
		listRead("layout hosts", schema.Hosts, 200, map[string]string{"sort": "name", "fields": hostSummaryFields}),
		listRead("layout containers", schema.Containers, 1, nil),
		listRead("layout databases", schema.Databases, 1, map[string]string{"filter": "temporary_until = ''"}),
		listRead("layout sites", schema.Sites, 1, nil),
		// the open alerts once, for the sidebar's count and the Overview's
		// O3, and for each of the sidebar's two dots the most severe of its
		// rules with their count, which the 200 newest may not hold
		listRead("layout alerts", schema.Alerts, 200, map[string]string{
			"filter": "status != 'resolved'", "sort": "-last_seen", "fields": openAlertsFields,
		}),
		listRead("layout domain alerts", schema.Alerts, 1, map[string]string{
			"filter": domainAlertsFilter, "sort": "severity", "fields": sectionAlertsFields,
		}),
		listRead("layout backup alerts", schema.Alerts, 1, map[string]string{
			"filter": backupAlertsFilter, "sort": "severity", "fields": sectionAlertsFields,
		}),
		{name: "info", route: "/api/pc/info"},

		// the Overview, O1 to O12 but O3, which is Layout's
		fullListRead("O1 containers", schema.Containers, map[string]string{"fields": "id,host,state,updated"}),
		listRead("O2 stacks", schema.Stacks, 500, map[string]string{"sort": "name", "fields": "id,collectionId,collectionName,name,host,updated"}),
		listRead("O4 commands", schema.Commands, 5, map[string]string{
			"filter": overviewQueueFilter, "sort": "-created", "skipTotal": "true",
			"fields": "id,collectionId,collectionName,created,updated,host,host_name,type,label,status,resource",
		}),
		listRead("O5 sites", schema.Sites, 500, map[string]string{
			"sort": "name", "expand": "active_deployment",
			"fields": "id,collectionId,collectionName,updated,name,host,type,source,generator,active_deployment," +
				"expand.active_deployment.id,expand.active_deployment.status,expand.active_deployment.generator,expand.active_deployment.finished_at",
		}),
		listRead("O6 site_php", schema.SitePHP, 500, map[string]string{"fields": "id,collectionId,collectionName,updated,site,php_version,template,state"}),
		listRead("O7 domains", schema.Domains, 1000, map[string]string{
			"filter": "site != ''", "sort": "hostname", "fields": "id,collectionId,collectionName,updated,hostname,host,site,tls",
		}),
		listRead("O8 certificates", schema.Certificates, 500, map[string]string{
			"sort": "-not_after",
			"fields": "id,collectionId,collectionName,updated,names,issuer,issuer_name,not_before,not_after,status,fingerprint,installed," +
				"attempts,attempt_started,renew_after,last_error,challenge,panel",
		}),
		listRead("O9 deployments", schema.Deployments, 100, map[string]string{
			// midnight UTC 7 days ago
			"filter": "(status = 'queued' || status = 'building' || status = 'failed') && created >= " +
				pbFilterTime(now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -7)),
			"sort": "-created", "skipTotal": "true", "fields": "id,collectionId,collectionName,created,updated,site,status,finished_at",
		}),
		listRead("O10 backup_jobs", schema.BackupJobs, 1000, map[string]string{
			"sort":   "name",
			"fields": "id,collectionId,collectionName,updated,name,host,source_kind,targets,origin,database,site,label_removed,enabled",
		}),
		listRead("O11 backup_targets", schema.BackupTargets, 500, map[string]string{"fields": "id,collectionId,collectionName,updated,name"}),
		// the start of the hour UTC 49 hours ago: the day, the 12 hours a
		// run may wait in the queue, the 12 it may then run, and an hour to
		// spare; sorted by created, which only the runs' index of created
		// serves
		fullListRead("O12 backup_runs", schema.BackupRuns, map[string]string{
			"filter": "created >= " + pbFilterTime(now.UTC().Truncate(time.Hour).Add(-49*time.Hour)),
			"sort":   "created", "fields": overviewRunFields,
		}),
	}
	// R1, one read for every server offline, and R2, which do not hold up
	// "filled"
	if len(offline) > 0 {
		each := []string{}
		for _, host := range offline {
			each = append(each, "host = '"+host+"'")
		}
		reads = append(reads, listRead("R1 events", schema.Events, 200, map[string]string{
			"filter": "(" + strings.Join(each, " || ") + ") && kind = 'connection.disconnected'", "sort": "-created",
			"fields": "id,host,data,created", "skipTotal": "true",
		}))
	}
	return append(reads, listRead("R2 dns_providers", schema.DNSProviders, 100, map[string]string{
		"fields": "id,collectionId,collectionName,updated,name,last_error",
	}))
}

// overviewLoadScale is how much history the seed writes: a busy panel's,
// or under the race detector a tenth of it and less, whose times say nothing
// but whose answers are checked all the same. The live records are the same
// in both.
type overviewLoadScale struct {
	commands, runsPerJob, resolvedAlerts, events int
}

var (
	overviewFullScale = overviewLoadScale{commands: 100_000, runsPerJob: 300, resolvedAlerts: 2000, events: 5000}
	overviewRaceScale = overviewLoadScale{commands: 5000, runsPerJob: 8, resolvedAlerts: 200, events: 500}
)

// overviewSeed is what the seed wrote that the answers are checked against.
type overviewSeed struct {
	hosts      []*core.Record
	offline    []string
	containers int
	// the newest commands the queue shows, newest first
	queue []string
	// the deployments and runs the page's windows hold
	recentDeployments, windowRuns int
}

// seeder saves records in a transaction, with a created time of the past
// where history needs one.
type seeder struct {
	t           *testing.T
	tx          core.App
	collections map[string]*core.Collection
	// the collections whose history has had a record checked
	checked map[string]bool
}

// save saves a record, checked as the API would check it.
func (s *seeder) save(name string, fields map[string]any, created time.Time) *core.Record {
	s.t.Helper()
	return s.write(name, fields, created, true)
}

// history saves a record of a long history: the first of its collection is
// checked, the others are made alike, which halves the seed's time.
func (s *seeder) history(name string, fields map[string]any, created time.Time) *core.Record {
	s.t.Helper()
	record := s.write(name, fields, created, !s.checked[name])
	s.checked[name] = true
	return record
}

func (s *seeder) write(name string, fields map[string]any, created time.Time, check bool) *core.Record {
	s.t.Helper()
	collection, ok := s.collections[name]
	if !ok {
		found, err := s.tx.FindCollectionByNameOrId(name)
		if err != nil {
			s.t.Fatal(err)
		}
		collection = found
		s.collections[name] = found
	}
	record := core.NewRecord(collection)
	record.Load(fields)
	if !created.IsZero() {
		at, err := types.ParseDateTime(created)
		if err != nil {
			s.t.Fatal(err)
		}
		record.SetRaw("created", at)
		record.SetRaw("updated", at)
	}
	save := s.tx.Save
	if !check {
		save = s.tx.SaveNoValidate
	}
	if err := save(record); err != nil {
		s.t.Fatalf("%s: %v", name, err)
	}
	return record
}

// hexID is a value of a size made from i, one of its own for each i once
// it is 16 characters or longer.
func hexID(prefix string, i, size int) string {
	base := fmt.Sprintf("%s%015x", prefix, i*2654435761)
	id := base
	for len(id) < size {
		id += base
	}
	return id[:size]
}

// seedOverviewLoad writes 20 servers with their vitals, 1000 containers, 100
// stacks, 40 sites with 60 domains, 50 certificates and 800 deployments, 120
// backup jobs to 3 targets with 36,000 runs and logs of 2 to 8 KB, 100,000
// commands over 100 days, 2000 resolved alerts and 8 open ones, in one
// transaction, before the panel registers: its hooks see none of it, as on a
// panel that starts with this database. The runs go back 240 days: as many
// as a panel with four times the jobs keeps in its 60, with no more of them
// in the Overview's window than these 120 jobs make.
func seedOverviewLoad(t *testing.T, app core.App, scale overviewLoadScale, now time.Time) overviewSeed {
	t.Helper()
	seed := overviewSeed{}
	day := 24 * time.Hour
	err := app.RunInTransaction(func(tx core.App) error {
		s := &seeder{t: t, tx: tx, collections: map[string]*core.Collection{}, checked: map[string]bool{}}

		capabilities := []string{}
		for i := range 30 {
			capabilities = append(capabilities, fmt.Sprintf("command.kind-%02d", i))
		}
		runtime := map[string]any{}
		for i := range 40 {
			runtime[fmt.Sprintf("setting_%02d", i)] = strings.Repeat("v", 40)
		}
		for i := range 20 {
			roles := []string{schema.HostRoleDocker}
			switch i {
			case 0:
				roles = append(roles, schema.HostRoleEdge)
			case 1:
				roles = append(roles, schema.HostRoleBackup)
			}
			status, stream, seen := schema.HostOnline, true, now.Add(-5*time.Second)
			if i == 19 {
				status, stream, seen = schema.HostOffline, false, now.Add(-14*time.Minute)
			}
			host := s.save(schema.Hosts, map[string]any{
				"name": fmt.Sprintf("load-%02d", i+1), "roles": roles, "status": status, "stream_open": stream,
				"hostname": fmt.Sprintf("load-%02d.lan", i+1), "os": "Debian GNU/Linux 12 (bookworm)", "kernel": "6.1.0-27-amd64",
				"arch": "x86_64", "cpu_cores": 4, "mem_bytes": int64(8) << 30, "ip4": fmt.Sprintf("10.0.0.%d", i+10),
				"public_ip4": "203.0.113.7", "agent_version": "0.33.0", "capabilities": capabilities, "docker_version": "27.3.1",
				"last_seen": seen, "rtt_ms": 1.2 + float64(i)/10, "clock_skew_ms": 3, "connected_since": now.Add(-48 * time.Hour),
				"uptime_s": 86400 * 12, "load1": 0.42, "cpu_percent": float64(5 + i*5), "mem_used_bytes": int64(i+1) << 28,
				"disk_used_bytes": int64(i+10) << 30, "disk_total_bytes": int64(64) << 30, "runtime": runtime,
				"self_checks":  map[string]any{"docker": "ok", "disk": "ok", "clock": "ok", "notes": strings.Repeat("n", 300)},
				"inventory_at": now.Add(-time.Minute), "cert_serial": hexID("c", i, 32), "cert_expires_at": now.Add(300 * day),
			}, now.Add(-90*day))
			seed.hosts = append(seed.hosts, host)
			if status == schema.HostOffline {
				seed.offline = append(seed.offline, host.Id)
			}
		}
		edge, carrier := seed.hosts[0], seed.hosts[1]

		// 5 stacks and 50 containers per server, 2 of each stack's
		stacks := []*core.Record{}
		for i := range 100 {
			stacks = append(stacks, s.save(schema.Stacks, map[string]any{
				"host": seed.hosts[i%20].Id, "name": fmt.Sprintf("stack-%03d", i),
				"compose_yaml": strings.Repeat("services:\n  web:\n    image: nginx:1.27\n", 100), "env": strings.Repeat("KEY=value\n", 50),
				"current_revision": 3, "status": "running",
			}, now.Add(-60*day)))
		}
		for i := range 1000 {
			host := i % 20
			state := "running"
			switch {
			case i%31 == 0:
				state = "restarting"
			case i%17 == 0:
				state = "exited"
			}
			fields := map[string]any{
				"host": seed.hosts[host].Id, "docker_id": hexID("d", i, 64), "name": fmt.Sprintf("app-%04d", i),
				"image": "ghcr.io/example/app:1.4.2", "state": state, "status_text": "Up 3 days", "health": "healthy",
				"restart_count": i % 3, "ports": []map[string]any{{"private": 80, "public": 8000 + i, "type": "tcp"}},
				"labels":     map[string]string{"com.docker.compose.project": "app", "org.opencontainers.image.source": strings.Repeat("s", 200)},
				"started_at": now.Add(-72 * time.Hour),
			}
			if slot := i / 20; slot < 10 {
				stack := stacks[host+20*(slot/2)]
				fields["stack"], fields["project"], fields["service"] = stack.Id, stack.GetString("name"), fmt.Sprintf("svc-%d", slot%2)
			}
			s.save(schema.Containers, fields, now.Add(-72*time.Hour))
			seed.containers++
		}

		server := s.save(schema.DBServers, map[string]any{"host": seed.hosts[2].Id, "name": "shared", "engine": schema.EnginePostgres,
			"kind": schema.DBServerShared, "container_ref": "pcdb-shared-db-1", "admin_user": "postgres", "status": "ready"}, now.Add(-30*day))
		databases := []*core.Record{}
		for i := range 6 {
			databases = append(databases, s.save(schema.Databases, map[string]any{"server": server.Id, "name": fmt.Sprintf("app_%d", i),
				"owner_kind": "none", "status": "ready"}, now.Add(-30*day)))
		}

		// 40 sites on the edge, 30 static and 10 PHP, each with 20
		// deployments over 100 days, the newest of some failed or building
		sevenDays := now.UTC().Truncate(day).AddDate(0, 0, -7)
		sites := []*core.Record{}
		for i := range 40 {
			kind, generator := schema.SiteStatic, []string{"hugo", "mkdocs", "astro", "npm", "none"}[i%5]
			if i >= 30 {
				kind, generator = schema.SitePHPApp, ""
			}
			site := s.save(schema.Sites, map[string]any{"name": fmt.Sprintf("site-%02d", i), "host": edge.Id, "type": kind,
				"source": []string{"git", "upload", "empty"}[i%3], "generator": generator, "repo_url": "https://git.example.org/site.git",
				"branch": "main", "keep_releases": 5, "headers": map[string]string{"X-Frame-Options": "DENY"}}, now.Add(-100*day))
			active := ""
			for number := 1; number <= 20; number++ {
				created := now.Add(-time.Duration(20-number)*5*day - time.Duration(i)*time.Hour)
				status := schema.DeploymentSucceeded
				switch {
				case number == 20 && i%7 == 0:
					status = schema.DeploymentFailed
				case number == 20 && i%11 == 0:
					status = schema.DeploymentBuilding
				case number%9 == 0:
					status = schema.DeploymentFailed
				}
				fields := map[string]any{"site": site.Id, "number": number, "status": status, "trigger": "push", "source": site.GetString("source"),
					"branch": "main", "commit_sha": hexID("a", i*100+number, 40), "message": "Update the content", "generator": generator,
					"build_cmd": "hugo --minify", "started_at": created, "build_ms": 42000, "size_bytes": 12 << 20, "files": 812}
				if status != schema.DeploymentBuilding {
					fields["finished_at"] = created.Add(time.Minute)
				}
				if status == schema.DeploymentFailed {
					fields["error"] = strings.Repeat("Error: build failed\n", 20)
				}
				deployment := s.history(schema.Deployments, fields, created)
				if status == schema.DeploymentSucceeded {
					active = deployment.Id
				}
				if status != schema.DeploymentSucceeded && !created.Before(sevenDays) {
					seed.recentDeployments++
				}
			}
			if kind == schema.SiteStatic {
				site.Set("active_deployment", active)
				if err := tx.Save(site); err != nil {
					return err
				}
			} else {
				s.save(schema.SitePHP, map[string]any{"site": site.Id, "php_version": "8.3",
					"template": []string{schema.PHPWordPress, schema.PHPLaravel, schema.PHPNette, schema.PHPCustom}[i%4],
					"docroot":  "public", "mode": schema.PHPReleases, "state": schema.PHPRunning, "image": "php:8.3-fpm"}, now.Add(-100*day))
			}
			sites = append(sites, site)
		}
		// a domain per site and a second for 20 of them; certificates for 50
		certificates := []*core.Record{}
		for i := range 60 {
			site := sites[i%40]
			hostname := site.GetString("name") + ".example.org"
			if i >= 40 {
				hostname = "www." + hostname
			}
			tls := schema.TLSAuto
			switch {
			case i >= 40 && i < 45:
				tls = schema.TLSInternal
			case i >= 58:
				tls = schema.TLSOff
			}
			s.save(schema.Domains, map[string]any{"hostname": hostname, "host": edge.Id, "target_kind": schema.TargetSite, "site": site.Id,
				"tls": tls, "https_redirect": true, "compress": true}, now.Add(-100*day))
			if i >= 50 {
				continue
			}
			issuer, issuerName := schema.IssuerACME, "Let's Encrypt R11"
			switch {
			case i >= 40 && i < 45:
				issuer, issuerName = schema.IssuerInternal, "PocketCloud Internal CA"
			case i >= 45:
				issuer, issuerName = schema.IssuerManual, "Example CA"
			}
			certificates = append(certificates, s.save(schema.Certificates, map[string]any{"names": []string{hostname}, "issuer": issuer,
				"not_before": now.Add(-time.Duration(i) * day), "not_after": now.Add(time.Duration(12+i*2) * day), "status": schema.CertActive,
				"chain_pem": strings.Repeat("MIIFazCCBFOgAwIBAgISA", 150), "fingerprint": hexID("f", i, 64), "subject": "CN=" + hostname,
				"issuer_name": issuerName, "installed": map[string]string{edge.Id: hexID("i", i, 15)}, "challenge": schema.ChallengeHTTP01,
				"history": []map[string]string{{"at": now.Format(time.RFC3339), "event": strings.Repeat("renewed ", 30)}}}, now.Add(-time.Duration(i)*day)))
		}
		providers := []*core.Record{
			s.save(schema.DNSProviders, map[string]any{"name": "cloudflare", "kind": "cloudflare", "credentials": "sealed", "status": schema.DNSOK}, now.Add(-60*day)),
			s.save(schema.DNSProviders, map[string]any{"name": "desec", "kind": "desec", "credentials": "sealed", "status": schema.DNSError,
				"last_error": "401 Unauthorized: the token was revoked"}, now.Add(-60*day)),
		}

		// 3 targets on the backup server; 120 jobs, each with a run every
		// 19.2 hours, 300 of them over 240 days
		targets := []*core.Record{}
		for i, kind := range []string{"local", "s3", "sftp"} {
			targets = append(targets, s.save(schema.BackupTargets, map[string]any{"name": "target-" + kind, "kind": kind, "host": carrier.Id,
				"config": map[string]string{"path": "/srv/restic/" + kind}, "location": "/srv/restic/" + kind, "credentials": "sealed",
				"repo_password": "sealed", "repo_id": hexID("r", i, 64), "status": schema.TargetOK, "used_bytes": int64(i+1) << 34}, now.Add(-90*day)))
		}
		jobs := []*core.Record{}
		for j := range 120 {
			jobTargets := []string{targets[j%3].Id}
			if j%4 == 0 {
				jobTargets = append(jobTargets, targets[(j+1)%3].Id)
			}
			// disabled: the panel that serves this database starts none of them
			jobs = append(jobs, s.save(schema.BackupJobs, map[string]any{"name": fmt.Sprintf("job-%03d", j), "host": seed.hosts[j%19].Id,
				"source_kind": schema.SourcePath, "source": fmt.Sprintf("/srv/data-%03d", j), "targets": jobTargets, "schedule": "0 3 * * *",
				"retention": map[string]int{"daily": 7, "weekly": 4}, "enabled": false, "origin": schema.JobManual,
				"last_status": schema.RunSucceeded, "last_run_at": now.Add(-time.Hour)}, now.Add(-90*day)))
		}
		runsSince := now.UTC().Truncate(time.Hour).Add(-49 * time.Hour)
		interval := 1152 * time.Minute // 19.2 hours
		for j, job := range jobs {
			jobTargets := job.GetStringSlice("targets")
			for k := range scale.runsPerJob {
				created := now.Add(-time.Duration(k)*interval - time.Duration(j)*interval/120)
				status := schema.RunSucceeded
				switch {
				case (j*7+k)%23 == 0:
					status = schema.RunFailed
				case (j*11+k)%61 == 0:
					status = schema.RunSkipped
				}
				fields := map[string]any{"job": job.Id, "target": jobTargets[k%len(jobTargets)], "host": job.GetString("host"),
					"trigger": schema.RunSchedule, "status": status, "scheduled_at": created, "started_at": created,
					"finished_at": created.Add(4 * time.Minute), "duration_ms": 240000, "snapshot_id": hexID("s", j*1000+k, 64),
					"bytes_added": 12 << 20, "total_bytes": int64(j+1) << 26, "files_new": 12, "files_changed": 40,
					"log": strings.Repeat("restic: processed 1234 files, 2.1 GiB in 4:01\n", (2048+(j*131+k*17)%6145)/46+1)}
				if status != schema.RunSucceeded {
					fields["error"] = "Fatal: unable to open repository: connection refused"
				}
				s.history(schema.BackupRuns, fields, created)
				if !created.Before(runsSince) {
					seed.windowRuns++
				}
			}
		}

		// commands, one every 86.4 seconds: 1000 a day; 4 in 10 are of
		// the queue, the rest reads, measurements and steps it hides
		shown := map[int]bool{}
		parent := ""
		for i := scale.commands - 1; i >= 0; i-- {
			created := now.Add(-time.Duration(i)*86400*time.Millisecond - time.Second)
			host := seed.hosts[i%19]
			fields := map[string]any{"host": host.Id, "host_name": host.GetString("name"), "status": schema.CommandSucceeded,
				"requested_label": "admin@example.com", "correlation_id": hexID("k", i, 15), "attempts": 1, "timeout_s": 120,
				"dispatched_at": created, "started_at": created, "finished_at": created.Add(2 * time.Second),
				"payload": map[string]any{"id": i, "note": strings.Repeat("p", 120)}, "result": map[string]any{"ok": true, "note": strings.Repeat("r", 200)}}
			target := targets[i%3].Id
			switch i % 20 {
			case 0, 1, 2, 3, 4, 5:
				fields["type"] = "db.stats"
			case 6, 7:
				fields["type"], fields["resource"] = "backup.init", "backup-usage:"+target
			case 8:
				fields["type"] = "logs.follow"
			case 9:
				fields["type"] = "exec.start"
			case 10:
				fields["type"], fields["parent"] = "container.restart", parent
			case 11:
				fields["type"] = "site.php.status"
			case 12, 13:
				fields["type"], fields["resource"], fields["label"] = "backup.run", "backup:"+target, "Back up job-001 to target-s3"
			case 14:
				fields["type"], fields["resource"], fields["label"] = "container.restart", "container:"+hexID("d", i%1000, 64), "app-0001"
			case 15:
				fields["type"], fields["resource"], fields["label"] = "stack.deploy", "stack:stack-001", "stack-001"
			case 16:
				fields["type"], fields["resource"], fields["label"] = "site.deploy", "site:"+sites[i%40].Id, sites[i%40].GetString("name")
			case 17:
				fields["type"], fields["resource"] = "edge.apply", "edge"
			case 18:
				fields["type"], fields["resource"] = "agent.update", "agent"
			case 19:
				fields["type"], fields["label"] = "db.database.create", "app_9"
			}
			command := s.history(schema.Commands, fields, created)
			parent = command.Id
			if i%20 >= 12 {
				shown[i] = true
				if i < 100 {
					seed.queue = append(seed.queue, command.Id)
				}
			}
		}
		// newest first
		slices.Reverse(seed.queue)
		seed.queue = seed.queue[:5]

		// alerts: those of 100 days, resolved, and 8 open
		rules := []string{"agent_offline", "backup_failed", "certificate_expiring", "backup_check_unverified", "dns_provider_failed"}
		for i := range scale.resolvedAlerts {
			created := now.Add(-time.Duration(i) * 72 * time.Minute)
			host := seed.hosts[i%20]
			s.history(schema.Alerts, map[string]any{"rule": rules[i%len(rules)], "target": host.Id, "host": host.Id,
				"severity": schema.SeverityWarning, "status": schema.AlertResolved, "first_seen": created, "last_seen": created.Add(time.Hour),
				"resolved_at": created.Add(time.Hour), "message": "load-01: the agent stopped responding"}, created)
		}
		offlineHost := seed.hosts[19]
		open := []map[string]any{
			{"rule": "agent_offline", "target": offlineHost.Id, "host": offlineHost.Id, "severity": schema.SeverityCritical,
				"message": "load-20: the agent stopped responding"},
			{"rule": "backup_failed", "target": jobs[3].Id + ":" + targets[0].Id, "host": jobs[3].GetString("host"), "severity": schema.SeverityCritical,
				"message": "job-003: the backup to target-local failed"},
			{"rule": "backup_failed", "target": jobs[8].Id + ":" + targets[2].Id, "host": jobs[8].GetString("host"), "severity": schema.SeverityCritical,
				"message": "job-008: the backup to target-sftp failed"},
			{"rule": "backup_check_unverified", "target": targets[1].Id, "host": carrier.Id, "severity": schema.SeverityWarning,
				"message": "target-s3 is not verified"},
			{"rule": "certificate_expiring", "target": certificates[0].Id, "host": edge.Id, "severity": schema.SeverityWarning,
				"message": "site-00.example.org expires in 12 days"},
			{"rule": "dns_provider_failed", "target": providers[1].Id, "severity": schema.SeverityWarning, "message": "desec: 401 Unauthorized"},
			{"rule": "db_temporary_left", "target": databases[0].Id, "host": server.GetString("host"), "severity": schema.SeverityWarning,
				"message": "a temporary database was left"},
			{"rule": "backup_restore", "target": seed.hosts[4].Id, "host": seed.hosts[4].Id, "severity": schema.SeverityInfo,
				"message": "load-05: a restore needs attention"},
		}
		for i, alert := range open {
			created := now.Add(-time.Duration(i+1) * time.Hour)
			alert["status"], alert["first_seen"], alert["last_seen"] = schema.AlertOpen, created, now.Add(-time.Duration(i)*time.Minute)
			s.save(schema.Alerts, alert, created)
		}

		// the events of 30 days, and the offline server's last disconnect
		for i := range scale.events {
			host := seed.hosts[i%20]
			s.history(schema.Events, map[string]any{"host": host.Id, "kind": []string{"agent.online", "agent.offline", "container.die"}[i%3],
				"severity": schema.SeverityInfo, "message": "load: the server is back online", "data": map[string]any{"i": i}},
				now.Add(-time.Duration(i)*8*time.Minute))
		}
		s.save(schema.Events, map[string]any{"host": offlineHost.Id, "kind": "connection.disconnected", "severity": schema.SeverityWarning,
			"message": "load-20 disconnected", "data": map[string]any{"reason": "eof"}}, now.Add(-14*time.Minute))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return seed
}

// overviewAnswer is one read's answer: each page's body.
type overviewAnswer struct {
	pages []string
}

func (a overviewAnswer) bytes() int {
	total := 0
	for _, page := range a.pages {
		total += len(page)
	}
	return total
}

// items are the records of every page, and the first page's count.
func (a overviewAnswer) items(t *testing.T) ([]map[string]any, int) {
	t.Helper()
	all := []map[string]any{}
	total := -1
	for i, page := range a.pages {
		var list struct {
			Items      []map[string]any `json:"items"`
			TotalItems int              `json:"totalItems"`
		}
		if err := json.Unmarshal([]byte(page), &list); err != nil {
			t.Fatalf("%v: %s", err, page)
		}
		if i == 0 {
			total = list.TotalItems
		}
		all = append(all, list.Items...)
	}
	return all, total
}

// fetch makes a read as the viewer: a list's pages until one is short, as
// getFullList does, or its first page.
func fetch(t *testing.T, handler http.Handler, token string, read overviewRead) overviewAnswer {
	t.Helper()
	if read.route != "" {
		status, body := testutil.Do(t, handler, testutil.Request{Method: http.MethodGet, URL: read.route, Token: token})
		if status != http.StatusOK {
			t.Fatalf("%s: %d %s", read.name, status, body)
		}
		return overviewAnswer{pages: []string{body}}
	}
	answer := overviewAnswer{}
	for page := 1; ; page++ {
		query := url.Values{}
		for key, values := range read.query {
			query[key] = values
		}
		query.Set("page", strconv.Itoa(page))
		query.Set("perPage", strconv.Itoa(read.perPage))
		status, body := testutil.Do(t, handler, testutil.Request{Method: http.MethodGet,
			URL: "/api/collections/" + read.collection + "/records?" + query.Encode(), Token: token})
		if status != http.StatusOK {
			t.Fatalf("%s: %d %s", read.name, status, body)
		}
		answer.pages = append(answer.pages, body)
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal([]byte(body), &list); err != nil {
			t.Fatal(err)
		}
		if !read.all || len(list.Items) < read.perPage {
			return answer
		}
	}
}

// overviewTimes are a read's times over the rounds, and its last answer.
type overviewTimes struct {
	read   overviewRead
	times  []time.Duration
	answer overviewAnswer
}

func (m overviewTimes) median() time.Duration {
	sorted := slices.Clone(m.times)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}

// TestTheOverviewsReadsAnswerQuicklyWith20Servers (#33, AC2): every read of a
// cold load of the Overview and its Layout answers a viewer in under 100 ms,
// all of them in under 300 ms, with under 512 KB, on a panel with 20 servers
// and the history of a busy one; each read answers what the page needs and
// only the fields it asks for. Under the race detector it seeds less history
// and checks the answers, not the times.
func TestTheOverviewsReadsAnswerQuicklyWith20Servers(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a large history")
	}
	scale := overviewFullScale
	if raceEnabled {
		scale = overviewRaceScale
	}

	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)
	// the internal CA, as the panel's first start makes it before any server
	// enrolls: a panel refuses a database with servers but no CA
	p := New(app, &Config{MasterKey: testutil.MasterKey, GRPCAddr: "127.0.0.1:0"}, testUI)
	if err := p.loadMasterKey(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.openAuthority(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	began := time.Now()
	seed := seedOverviewLoad(t, app, scale, now)
	t.Logf("seeded %+v in %s", scale, time.Since(began).Round(time.Millisecond))

	p.Register()
	t.Cleanup(p.later.stop)
	handler := testutil.Handler(t, app)
	_, viewer := testutil.NewUser(t, app, "viewer@example.com", schema.RoleViewer)

	// the open alerts; the panel may raise some of its own as it starts,
	// such as about its recovery
	open, err := app.FindAllRecords(schema.Alerts, dbx.Not(dbx.HashExp{"status": schema.AlertResolved}))
	if err != nil {
		t.Fatal(err)
	}
	openAlerts := len(open)
	if openAlerts < 8 {
		t.Fatalf("%d open alerts", openAlerts)
	}
	// the panel makes a job for each database once it serves (#53): the
	// reads are timed once it has, with nothing else written meanwhile
	var jobs int64
	for deadline := time.Now().Add(15 * time.Second); jobs != 120+6; time.Sleep(10 * time.Millisecond) {
		if jobs, err = app.CountRecords(schema.BackupJobs); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the panel made %d jobs", jobs)
		}
	}

	reads := overviewReads(now, seed.offline)
	measured := make([]overviewTimes, len(reads))
	// the median of 5 rounds; once under the race detector, whose times are
	// not checked
	rounds := 5
	if raceEnabled {
		rounds = 1
	}
	for range rounds {
		for i, read := range reads {
			start := time.Now()
			answer := fetch(t, handler, viewer, read)
			measured[i].read, measured[i].answer = read, answer
			measured[i].times = append(measured[i].times, time.Since(start))
		}
	}

	var sum time.Duration
	totalBytes, requests := 0, 0
	table := []string{fmt.Sprintf("%-26s %9s %9s %5s %9s", "read", "median", "max", "reqs", "bytes")}
	for _, m := range measured {
		median := m.median()
		sum += median
		totalBytes += m.answer.bytes()
		requests += len(m.answer.pages)
		table = append(table, fmt.Sprintf("%-26s %9s %9s %5d %9d", m.read.name, median.Round(10*time.Microsecond),
			slices.Max(m.times).Round(10*time.Microsecond), len(m.answer.pages), m.answer.bytes()))
	}
	table = append(table, fmt.Sprintf("%-26s %9s %9s %5d %9d", "sum", sum.Round(10*time.Microsecond), "", requests, totalBytes))
	t.Logf("a cold load's reads, %d rounds:\n%s", rounds, strings.Join(table, "\n"))

	// A read costs PocketBase about 50 µs a record, and its fields picker as
	// much again for a long record: it writes each whole record as JSON and
	// reads it back before it picks the fields (tools/picker). The thousand
	// containers and the runs of the window, whose logs it reads too, take
	// the longest; the rest under 15 ms each. The runs' read goes through
	// their index of created to the window only because it is sorted by
	// created: PocketBase reads a list whose rule names @request.auth with a
	// join of the users and DISTINCT, and SQLite would otherwise go by id
	// through all 36,000 runs and their logs, which takes 150 ms or more.
	if !raceEnabled {
		for _, m := range measured {
			if median := m.median(); median >= 100*time.Millisecond {
				t.Errorf("%s answers in %s", m.read.name, median)
			}
		}
		if sum >= 300*time.Millisecond {
			t.Errorf("the reads answer in %s together", sum)
		}
	}
	if totalBytes >= 512<<10 {
		t.Errorf("the reads send %d bytes", totalBytes)
	}

	byName := map[string]overviewTimes{}
	for _, m := range measured {
		byName[m.read.name] = m
	}
	items := func(name string) ([]map[string]any, int) {
		t.Helper()
		m, ok := byName[name]
		if !ok {
			t.Fatalf("no read %s", name)
		}
		return m.answer.items(t)
	}
	keys := func(item map[string]any) string {
		names := []string{}
		for key := range item {
			names = append(names, key)
		}
		sort.Strings(names)
		return strings.Join(names, ",")
	}
	fieldsOf := func(fields string) string {
		names := strings.Split(fields, ",")
		sort.Strings(names)
		return strings.Join(names, ",")
	}

	// PocketBase sleeps up to half a second before it answers a fourth
	// filtered list of a collection that finds nothing within 3 seconds
	// from one address (apis/record_crud.go, listTimingRateLimitRule): a
	// load reads no collection with a filter more than 3 times, as
	// web/src/pages/OverviewLoad.test.tsx checks of the page itself, and no
	// filtered read finds nothing here, so that none of these times holds
	// such a sleep
	filteredOf := map[string]int{}
	for _, m := range measured {
		if !m.read.filtered {
			continue
		}
		filteredOf[m.read.collection] += len(m.answer.pages)
		if found, _ := m.answer.items(t); len(found) == 0 {
			t.Errorf("%s finds nothing", m.read.name)
		}
	}
	for collection, count := range filteredOf {
		if count > 3 {
			t.Errorf("a load reads %s with a filter %d times", collection, count)
		}
	}
	if filteredOf[schema.Alerts] != 3 || filteredOf[schema.Events] != 1 {
		t.Errorf("the reads with a filter: %v", filteredOf)
	}

	// Layout
	hosts, total := items("layout hosts")
	if len(hosts) != 20 || total != 20 || keys(hosts[0]) != fieldsOf(hostSummaryFields) {
		t.Errorf("the servers: %d of %d with %s", len(hosts), total, keys(hosts[0]))
	}
	if hosts[0]["name"] != "load-01" || hosts[19]["name"] != "load-20" || hosts[19]["status"] != schema.HostOffline {
		t.Errorf("the servers are not in order of name: %v, %v", hosts[0]["name"], hosts[19])
	}
	for name, want := range map[string]int{
		"layout containers": seed.containers, "layout databases": 6, "layout sites": 40, "layout alerts": openAlerts,
	} {
		if _, total := items(name); total != want {
			t.Errorf("%s counts %d, not %d", name, total, want)
		}
	}
	var info struct {
		Agents struct{ Connected, Total int } `json:"agents"`
	}
	if err := json.Unmarshal([]byte(byName["info"].answer.pages[0]), &info); err != nil || info.Agents.Total != 20 || info.Agents.Connected != 19 {
		t.Errorf("the panel's facts: %+v, %v", info, err)
	}

	// O1: every container, in two pages of getFullList, with 4 fields
	containers, _ := items("O1 containers")
	if len(containers) != seed.containers || len(byName["O1 containers"].answer.pages) != 2 || keys(containers[0]) != "host,id,state,updated" {
		t.Errorf("the containers: %d in %d pages, with %s", len(containers), len(byName["O1 containers"].answer.pages), keys(containers[0]))
	}
	stacks, total := items("O2 stacks")
	if len(stacks) != 100 || total != 100 || keys(stacks[0]) != "collectionId,collectionName,host,id,name,updated" {
		t.Errorf("the stacks: %d of %d with %s", len(stacks), total, keys(stacks[0]))
	}
	// the open alerts, newest first, with their fields only
	alerts, _ := items("layout alerts")
	if len(alerts) != openAlerts || keys(alerts[0]) != fieldsOf(openAlertsFields) {
		t.Errorf("%d open alerts, not %d, with %s", len(alerts), openAlerts, keys(alerts[0]))
	}
	for i := 1; i < len(alerts); i++ {
		if alerts[i-1]["last_seen"].(string) < alerts[i]["last_seen"].(string) {
			t.Errorf("the alerts are not newest first: %v", alerts)
		}
	}
	// the sidebar's dots: of each section's open alerts the most severe,
	// critical sorting before info and warning, and the count of all
	for name, rules := range map[string][]string{
		"layout domain alerts": {"certificate_failed", "certificate_expiring", "dns_provider_failed"},
		"layout backup alerts": {"backup_failed", "backup_skipped", "backup_retention", "backup_check_failed",
			"backup_check_unverified", "panel_backup", "backup_restore_test_failed", "backup_restore_test_unverified", "db_temporary_left"},
	} {
		want, critical := 0, false
		for _, record := range open {
			if slices.Contains(rules, record.GetString("rule")) {
				want++
				critical = critical || record.GetString("severity") == schema.SeverityCritical
			}
		}
		found, total := items(name)
		if want == 0 || total != want || len(found) != 1 || keys(found[0]) != fieldsOf(sectionAlertsFields) ||
			(found[0]["severity"] == schema.SeverityCritical) != critical {
			t.Errorf("%s: %v of %d, not of %d (critical: %v)", name, found, total, want, critical)
		}
	}

	// O4: the 5 newest commands of the queue, without the reads,
	// measurements and steps that came after them
	commands, total := items("O4 commands")
	got := []string{}
	for _, command := range commands {
		got = append(got, command["id"].(string))
	}
	if !slices.Equal(got, seed.queue) || total != -1 {
		t.Errorf("the queue: %v (%d counted), not %v", got, total, seed.queue)
	}

	sites, _ := items("O5 sites")
	if len(sites) != 40 {
		t.Fatalf("%d sites", len(sites))
	}
	for _, site := range sites {
		if site["type"] != schema.SiteStatic {
			continue
		}
		expand, _ := site["expand"].(map[string]any)
		deployment, _ := expand["active_deployment"].(map[string]any)
		if keys(deployment) != "finished_at,generator,id,status" || deployment["status"] != schema.DeploymentSucceeded {
			t.Errorf("a site's active deployment: %v", site)
		}
	}
	for name, want := range map[string]int{
		"O6 site_php": 10, "O7 domains": 60, "O8 certificates": 50, "O9 deployments": seed.recentDeployments,
		"O10 backup_jobs": int(jobs), "O11 backup_targets": 3, "O12 backup_runs": seed.windowRuns, "R2 dns_providers": 2,
	} {
		if found, _ := items(name); len(found) != want {
			t.Errorf("%s: %d, not %d", name, len(found), want)
		}
	}
	certificates, _ := items("O8 certificates")
	if _, ok := certificates[0]["chain_pem"]; ok {
		t.Error("the certificates send their chains")
	}
	runs, _ := items("O12 backup_runs")
	if keys(runs[0]) != fieldsOf(overviewRunFields) {
		t.Errorf("a run sends %s", keys(runs[0]))
	}
	events, _ := items("R1 events")
	if len(events) != 1 || keys(events[0]) != "created,data,host,id" || events[0]["host"] != seed.offline[0] {
		t.Errorf("the offline server's disconnect: %v", events)
	}
}
