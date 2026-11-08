package panel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
	"github.com/pocketbase/pocketbase/tools/router"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/edge"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/php"
	"github.com/oldbear24/PocketServerManager/internal/phpapps"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/sites"
)

// A new site in one go (#24). The wizard sends the site, its name on the web,
// whether www goes with it, and the challenge of its certificate; the panel
// creates the site and runs the rest as one parent command, site.provision,
// whose steps are its children, one after the other:
//
//	site.create       the site, on its server
//	site.dns          its domains, and their records at the zone's provider,
//	                  or the records to make by hand
//	site.propagation  until the public resolvers and the zone's name servers
//	                  answer them
//	site.deploy       the first deployment, built and released by the agent
//	site.certificate  one certificate for its names, from ACME
//	site.check        the site answers over HTTPS, as a visitor sees it
//
// Every step is a command, so the command history shows each. A step that
// fails pauses the flow: Retry runs it again as a new command, and the
// history keeps the failed one; Cancel ends the flow. A flow that is
// cancelled, or not retried within a day, removes the domains it created, and
// with them their DNS records. The site stays, to be deployed or removed.

// provisionSteps are the steps of a new site, in order.
var provisionSteps = []string{"site.create", "site.dns", "site.propagation", "site.deploy", "site.certificate", "site.check"}

// phpProvisionSteps are those of a new PHP site (#50): its database, when it
// has one, then its PHP-FPM, before its first release. They are the agent's
// own commands, as a deployment is.
var phpProvisionSteps = []string{"site.create", "site.dns", "site.propagation", "db.database.create", "site.php.apply", "site.deploy", "site.certificate", "site.check"}

// steps are a flow's steps, in order.
func (payload provisionPayload) steps() []string {
	if payload.PHP == nil {
		return provisionSteps
	}
	if payload.PHP.DatabaseServer == "" {
		return slices.DeleteFunc(slices.Clone(phpProvisionSteps), func(step string) bool { return step == "db.database.create" })
	}
	return phpProvisionSteps
}

const (
	// provisionPause is how long a failed step waits for Retry before the
	// flow gives up.
	provisionPause = 24 * time.Hour
	// provisionFor is how long a flow may take in all.
	provisionFor = 7 * 24 * time.Hour
	// heldBack is the next attempt of a certificate a flow orders with its
	// domains: the ACME manager leaves it to the flow's certificate step.
	heldBack = provisionFor
)

// provisionState is the work of the flows under way.
type provisionState struct {
	// tick is how soon a waiting step looks again.
	tick time.Duration
	// checkFor is how long the availability check keeps asking, every
	// checkEvery, before it fails: a new route and certificate can take a
	// moment.
	checkFor   time.Duration
	checkEvery time.Duration
	// locks keeps one flow from being advanced twice at once.
	locks keyedLocks
	// pending holds the flows with a look scheduled: one at a time each.
	pending sync.Map
	// noFirstDeploy holds the sites a flow creates: their first deployment is
	// one of its steps, not something the new site starts by itself.
	noFirstDeploy sync.Map
	// checked is when each step last asked: the resolvers, or the site.
	checked sync.Map
	// siteCheck asks a site's server for it; tests replace it.
	// roots, when set, are the only CAs trusted, as the internal CA's root
	// for a name of the LAN
	siteCheck func(ctx context.Context, url, address string, roots *x509.CertPool) (siteAnswer, error)
}

// siteAnswer is what a server answered for a site.
type siteAnswer struct {
	Status   int    `json:"status"`
	Location string `json:"location,omitempty"`
	TLS      string `json:"tls,omitempty"`
	Protocol string `json:"protocol,omitempty"`
}

// keyedLocks are mutexes by key.
type keyedLocks struct{ m sync.Map }

func (l *keyedLocks) of(key string) *sync.Mutex {
	lock, _ := l.m.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// SetProvisionTiming sets how soon a waiting step of a new site looks again,
// and how long and how often the availability check asks.
func (p *Panel) SetProvisionTiming(tick, checkFor, checkEvery time.Duration) {
	p.provision.tick, p.provision.checkFor, p.provision.checkEvery = tick, checkFor, checkEvery
}

// scheduleAdvance looks at a flow again after the tick, unless a look is
// scheduled already, so that a flow has one chain of looks.
func (p *Panel) scheduleAdvance(parentID string) {
	if _, scheduled := p.provision.pending.LoadOrStore(parentID, true); scheduled {
		return
	}
	p.later.after(p.provision.tick, func() {
		p.provision.pending.Delete(parentID)
		p.advanceProvision(parentID)
	})
}

// SetSiteCheck replaces how the availability check asks a site's server.
func (p *Panel) SetSiteCheck(check func(ctx context.Context, url, address string) (status int, location string, err error)) {
	p.provision.siteCheck = func(ctx context.Context, url, address string, _ *x509.CertPool) (siteAnswer, error) {
		status, location, err := check(ctx, url, address)
		return siteAnswer{Status: status, Location: location}, err
	}
}

func (p *Panel) registerProvision() {
	if p.provision.tick == 0 {
		p.provision.tick = 3 * time.Second
	}
	if p.provision.checkFor == 0 {
		p.provision.checkFor, p.provision.checkEvery = time.Minute, 3*time.Second
	}
	if p.provision.siteCheck == nil {
		p.provision.siteCheck = askSite
	}

	// a step the agent ran, or a step the panel settled, moves the flow on
	advanceParent := func(command *core.Record) {
		if parent := command.GetString("parent"); parent != "" {
			p.later.after(0, func() { p.advanceProvision(parent) })
		}
	}
	p.Commands.OnFinish("site.deploy", advanceParent)
	// a new PHP site's database and PHP-FPM; the database's own hook has
	// connected the site by the time this one runs
	p.Commands.OnFinish("db.database.create", advanceParent)
	p.Commands.OnFinish("site.php.apply", advanceParent)
	for _, step := range provisionSteps {
		if commands.PanelRun(step) {
			p.Commands.OnFinish(step, advanceParent)
		}
	}
	// a flow that ends without its site removes the domains it created
	p.Commands.OnFinish("site.provision", func(command *core.Record) {
		if command.GetString("status") != schema.CommandSucceeded {
			p.later.after(0, func() { p.abandonProvision(command.Id) })
		}
	})
	// the domains of a flow change state: it looks again
	p.App.OnRecordAfterUpdateSuccess(schema.Domains).BindFunc(func(e *core.RecordEvent) error {
		p.later.after(0, p.advanceProvisions)
		return e.Next()
	})
	p.App.OnRecordAfterUpdateSuccess(schema.Certificates).BindFunc(func(e *core.RecordEvent) error {
		p.later.after(0, p.advanceProvisions)
		return e.Next()
	})
}

func (p *Panel) bindProvisionRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	operators := requireRole(schema.RoleOperator, schema.RoleAdmin)
	// a site from an upload sends its ZIP with it
	group.POST("/sites/provision", p.handleProvision).BindFunc(operators).Bind(apis.BodyLimit(maxSiteUpload + 1<<20))
	group.POST("/provisions/{id}/retry", p.handleProvisionRetry).BindFunc(operators)
	group.GET("/dns/preview", p.handleDNSPreview).BindFunc(operators)
}

// provisionPayload is what a flow was asked for, in its parent command.
type provisionPayload struct {
	Site      string `json:"site"`
	SiteName  string `json:"site_name"`
	Hostname  string `json:"hostname"`
	WWW       string `json:"www,omitempty"`
	Challenge string `json:"challenge"`
	// PHP is what a new PHP site was asked for (#50)
	PHP *provisionPHP `json:"php,omitempty"`
}

// provisionPHP is a new PHP site's version, web root, template, and the
// database made for it on a server of its host, if any.
type provisionPHP struct {
	Version        string `json:"version"`
	Docroot        string `json:"docroot"`
	Template       string `json:"template"`
	DatabaseServer string `json:"database_server,omitempty"`
	Database       string `json:"database,omitempty"`
	// Install is a fresh install of the template's application, as
	// WordPress's with WP-CLI, for a site without a source of its own (#52)
	Install *freshInstall `json:"install,omitempty"`
	// QueueWorker gives the site its template's worker, as Laravel's
	QueueWorker bool `json:"queue_worker,omitempty"`
	// Health is the path the availability check asks for, the template's
	Health string `json:"health,omitempty"`
}

func payloadOf(command *core.Record) provisionPayload {
	var payload provisionPayload
	raw, _ := json.Marshal(command.Get("payload"))
	_ = json.Unmarshal(raw, &payload)
	return payload
}

// siteFields are what the wizard may set of a new site.
var siteFields = []string{
	"name", "host", "type", "source", "repo_url", "branch", "generator", "build_image", "build_cmd", "output_dir",
	"build_cpus", "build_memory_mb", "keep_releases", "git_token", "basic_auth",
	"https_redirect", "hsts", "compress", "spa", "not_found", "headers", "cache_rules",
}

// handleProvision creates a site with its name on the web, and starts the
// flow that does the rest. A site from an upload sends its ZIP as file, next
// to the request as data.
func (p *Panel) handleProvision(e *core.RequestEvent) error {
	var body struct {
		Site      map[string]any `json:"site"`
		Hostname  string         `json:"hostname"`
		WWW       bool           `json:"www"`
		Challenge string         `json:"challenge"`
		PHP       *provisionPHP  `json:"php"`
	}
	if data := e.Request.FormValue("data"); data != "" {
		if err := json.Unmarshal([]byte(data), &body); err != nil {
			return e.BadRequestError("Invalid request.", err)
		}
	} else if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid request.", err)
	}

	hostname := strings.ToLower(strings.TrimSpace(body.Hostname))
	if err := edge.ValidHostname(hostname); err != nil || strings.HasPrefix(hostname, "*.") {
		return e.BadRequestError("Enter the site's name on the web, such as blog.example.com.", nil)
	}
	www := ""
	if body.WWW {
		if strings.HasPrefix(hostname, "www.") {
			return e.BadRequestError("The name starts with www already.", nil)
		}
		www = "www." + hostname
	}
	challenge := body.Challenge
	if challenge == "" {
		challenge = schema.ChallengeAuto
	}
	if !slices.Contains([]string{schema.ChallengeAuto, schema.ChallengeHTTP01, schema.ChallengeDNS01}, challenge) {
		return e.BadRequestError("The challenge is auto, http-01 or dns-01.", nil)
	}
	for _, name := range []string{hostname, www} {
		if name == "" {
			continue
		}
		if taken, _ := e.App.CountRecords(schema.Domains, dbx.HashExp{"hostname": name}); taken > 0 {
			return e.BadRequestError(fmt.Sprintf("%s is a domain already.", name), nil)
		}
	}

	collection, err := e.App.FindCachedCollectionByNameOrId(schema.Sites)
	if err != nil {
		return e.InternalServerError("", err)
	}
	site := core.NewRecord(collection)
	for _, field := range siteFields {
		if value, ok := body.Site[field]; ok {
			site.Set(field, value)
		}
	}
	host, err := e.App.FindRecordById(schema.Hosts, site.GetString("host"))
	if err != nil {
		return e.BadRequestError("Choose the server that builds and serves the site.", nil)
	}
	// the records need the server's public address, and a problem is better
	// said now than as a failed step
	if preview := p.previewDNS(hostname, host.Id, ""); preview.Problem != "" {
		return e.BadRequestError(preview.Problem, nil)
	}
	var phpRequest *provisionPHP
	freshBuild := ""
	if site.GetString("type") == schema.SitePHPApp {
		if phpRequest, err = p.checkProvisionPHP(body.PHP, site, host); err != nil {
			return e.BadRequestError(capitalize(err.Error())+".", nil)
		}
		// a fresh install's build, as WP-CLI's download (#52)
		if install := phpRequest.Install; install != nil {
			t, _ := phpapps.Get(phpRequest.Template)
			if freshBuild, err = t.FreshBuild(phpapps.Vars{Locale: install.Locale}); err != nil {
				return e.BadRequestError(capitalize(err.Error())+".", nil)
			}
		}
	}

	var upload *uploadedZip
	if site.GetString("source") == sites.SourceUpload {
		files, err := e.FindUploadedFiles("file")
		if err != nil || len(files) != 1 {
			return e.BadRequestError("Send the site's ZIP file with it.", err)
		}
		if files[0].Size > maxSiteUpload {
			return e.BadRequestError(fmt.Sprintf("The upload is larger than %d MiB.", maxSiteUpload>>20), nil)
		}
		digest, err := zipDigest(files[0])
		if err != nil {
			return e.BadRequestError("The upload is not a ZIP file.", err)
		}
		upload = &uploadedZip{file: files[0], digest: digest}
	}

	ctx := audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e))
	site.Id = core.GenerateDefaultRandomId()
	p.provision.noFirstDeploy.Store(site.Id, true)
	if err := e.App.SaveWithContext(ctx, site); err != nil {
		p.provision.noFirstDeploy.Delete(site.Id)
		return e.BadRequestError("The site cannot be created.", err)
	}

	if phpRequest != nil {
		// the site was made with the defaults: the wizard's choices go in
		settings, err := p.phpSettings(e.App, site)
		if err == nil {
			settings.Set("php_version", phpRequest.Version)
			settings.Set("docroot", phpRequest.Docroot)
			settings.Set("template", phpRequest.Template)
			settings.Set("fresh_build", freshBuild)
			// and the template's defaults (#52)
			err = p.applyTemplate(settings, phpRequest, hostname)
		}
		if err == nil {
			err = e.App.SaveWithContext(ctx, settings)
		}
		if err != nil {
			_ = e.App.Delete(site)
			return e.InternalServerError("Failed to start the new site.", err)
		}
	}

	userID, label := requester(e)
	payload := map[string]any{
		"site": site.Id, "site_name": site.GetString("name"), "hostname": hostname, "www": www, "challenge": challenge,
	}
	if phpRequest != nil {
		// the flow's record keeps no password
		recorded := *phpRequest
		if recorded.Install != nil {
			install := *recorded.Install
			install.AdminPassword = ""
			recorded.Install = &install
		}
		payload["php"] = recorded
	}
	parent, err := p.Commands.Begin(ctx, commands.Options{
		Host:        host.Id,
		Type:        "site.provision",
		Label:       truncate(fmt.Sprintf("%s at %s", site.GetString("name"), hostname), 255),
		RequestedBy: userID,
		Requester:   label,
	}, payload)
	if err != nil {
		_ = e.App.Delete(site)
		return e.InternalServerError("Failed to start the new site.", err)
	}

	// the first step is done already: the site is there
	step, err := p.beginStep(parent, "site.create")
	if err == nil && upload != nil {
		_, err = p.startDeployment(ctx, site, deploymentRequest{
			Trigger: schema.TriggerUpload, Upload: upload.file, UploadSHA: upload.digest,
			RequestedBy: userID, Requester: label, Parent: parent.Id, Hold: true,
		})
	}
	if err != nil {
		_ = p.Commands.Settle(parent.Id, schema.CommandFailed, "The new site could not be set up: "+err.Error(), nil)
		return e.InternalServerError("Failed to start the new site.", err)
	}
	_ = p.Commands.Settle(step.Id, schema.CommandSucceeded, "", map[string]any{
		"site": site.Id, "name": site.GetString("name"), "server": host.GetString("name"),
	})

	return e.JSON(http.StatusCreated, map[string]any{"provision": parent.Id, "site": site.Id})
}

// uploadedZip is a ZIP sent with a new site.
type uploadedZip struct {
	file   *filesystem.File
	digest string
}

// beginStep starts a step of a flow, as a child command of it.
func (p *Panel) beginStep(parent *core.Record, step string) (*core.Record, error) {
	payload := payloadOf(parent)
	return p.Commands.Begin(context.Background(), commands.Options{
		Host:        parent.GetString("host"),
		Type:        step,
		Label:       payload.Hostname,
		RequestedBy: parent.GetString("requested_by"),
		Requester:   parent.GetString("requested_label"),
		Parent:      parent.Id,
		Correlation: parent.Id,
	}, map[string]any{"site": payload.Site, "hostname": payload.Hostname})
}

// stepsOf are a flow's steps so far, oldest first.
func (p *Panel) stepsOf(parentID string) []*core.Record {
	steps, err := p.App.FindRecordsByFilter(schema.Commands, "parent={:p}", "created,id", 0, 0, dbx.Params{"p": parentID})
	if err != nil {
		return nil
	}
	return steps
}

// advanceProvisions looks at every flow under way.
func (p *Panel) advanceProvisions() {
	running, err := p.App.FindRecordsByFilter(schema.Commands, "type='site.provision' && status={:running}", "created", 0, 0,
		dbx.Params{"running": schema.CommandRunning})
	if err != nil {
		return
	}
	for _, parent := range running {
		p.advanceProvision(parent.Id)
	}
}

// advanceProvision moves a flow on as far as it can: it settles the step
// under way when it is done, starts the next one, and looks again soon while
// a step waits.
func (p *Panel) advanceProvision(parentID string) {
	lock := p.provision.locks.of(parentID)
	if !lock.TryLock() {
		// another call is at it, which may not look again, as a Retry does
		// not: one more look comes after the tick
		p.scheduleAdvance(parentID)
		return
	}
	defer lock.Unlock()

	for range len(phpProvisionSteps) + 1 {
		parent, err := p.App.FindRecordById(schema.Commands, parentID)
		if err != nil || parent.GetString("status") != schema.CommandRunning {
			return
		}
		flowSteps := payloadOf(parent).steps()
		steps := p.stepsOf(parentID)
		if len(steps) == 0 {
			return
		}
		last := steps[len(steps)-1]
		status := last.GetString("status")

		switch status {
		case schema.CommandSucceeded:
			if strings.HasPrefix(pausedOf(parent), "start:") {
				return // the next step could not start: it waits for Retry
			}
			index := slices.Index(flowSteps, last.GetString("type"))
			if index == len(flowSteps)-1 {
				payload := payloadOf(parent)
				_ = p.Commands.Settle(parentID, schema.CommandSucceeded, "", map[string]any{"url": "https://" + payload.Hostname})
				events.Log(p.App, parent.GetString("host"), "site.provisioned", schema.SeverityInfo,
					fmt.Sprintf("%s is live at https://%s.", payload.SiteName, payload.Hostname), map[string]any{"site": payload.Site, "command": parentID})
				return
			}
			if err := p.startProvisionStep(parent, flowSteps[index+1]); err != nil {
				p.Log.Warn("a step of a new site could not start", "command", parentID, "step", flowSteps[index+1], "error", err)
				p.pauseStart(parent, flowSteps[index+1], err)
				return
			}
			continue
		case schema.CommandRunning, schema.CommandDispatched, schema.CommandQueued:
			if commands.PanelRun(last.GetString("type")) {
				p.workOnStep(parent, last)
			}
			// look again, unless the step ended meanwhile: then go on now
			if fresh, err := p.App.FindRecordById(schema.Commands, last.Id); err == nil && fresh.GetString("status") == schema.CommandSucceeded {
				continue
			}
			p.scheduleAdvance(parentID)
			return
		default:
			p.pauseProvision(parent, last)
			return
		}
	}
}

// startProvisionStep starts a step: the panel's own as a running command it
// works on, the deployment as the agent's command.
func (p *Panel) startProvisionStep(parent *core.Record, step string) error {
	switch step {
	case "site.deploy":
		return p.startProvisionDeploy(parent)
	case "db.database.create", "site.php.apply":
		return p.startProvisionPHP(parent, step)
	}
	command, err := p.beginStep(parent, step)
	if err != nil {
		return err
	}
	switch step {
	case "site.dns":
		return p.createProvisionDomains(parent, command)
	case "site.certificate":
		return p.startProvisionCertificate(parent, command)
	}
	return nil
}

// workOnStep does what a step of the panel's own needs now, and settles it
// when it is done.
func (p *Panel) workOnStep(parent, step *core.Record) {
	payload := payloadOf(parent)
	domains := p.provisionDomains(payload, parent.GetString("host"))
	switch step.GetString("type") {
	case "site.dns":
		p.workOnDNS(step, domains)
	case "site.propagation":
		p.workOnPropagation(parent, step, domains)
	case "site.certificate":
		p.workOnCertificate(parent, step, payload)
	case "site.check":
		p.workOnCheck(parent, step, payload)
	}
}

// provisionDomains are the domains of a flow's names that are still there.
func (p *Panel) provisionDomains(payload provisionPayload, hostID string) []*core.Record {
	var domains []*core.Record
	for _, name := range []string{payload.Hostname, payload.WWW} {
		if name == "" {
			continue
		}
		if domain, err := p.App.FindFirstRecordByData(schema.Domains, "hostname", name); err == nil && domain.GetString("host") == hostID {
			domains = append(domains, domain)
		}
	}
	return domains
}

// createProvisionDomains creates the site's domain, and the www alias that
// redirects to it, unless they are there from an earlier attempt.
func (p *Panel) createProvisionDomains(parent, step *core.Record) error {
	payload := payloadOf(parent)
	site, err := p.App.FindRecordById(schema.Sites, payload.Site)
	if err != nil {
		return p.Commands.Settle(step.Id, schema.CommandFailed, "The site is gone.", nil)
	}
	collection, err := p.App.FindCachedCollectionByNameOrId(schema.Domains)
	if err != nil {
		return err
	}
	actor := audit.System("new site setup", audit.SourcePanel)
	actor.Command = parent.Id
	ctx := audit.WithActor(context.Background(), actor)

	// the ACME manager waits meanwhile: it would order a certificate for
	// each domain, or drop the order before a domain needs it
	p.acme.planning.Lock()
	defer p.acme.planning.Unlock()
	// one certificate covers both names, issued in the certificate step
	p.orderProvisionCertificate(parent)

	created := []string{}
	failed := func(message string) error {
		// what was created so far goes when the setup ends
		_ = p.Commands.Report(step.Id, map[string]any{"step": "dns", "created": created}, time.Time{})
		return p.Commands.Settle(step.Id, schema.CommandFailed, message, nil)
	}
	tls := schema.TLSAuto
	if lan, _ := p.provisionLAN(parent); lan {
		tls = schema.TLSInternal
	}
	for _, name := range []string{payload.Hostname, payload.WWW} {
		if name == "" {
			continue
		}
		if existing, err := p.App.FindFirstRecordByData(schema.Domains, "hostname", name); err == nil {
			if existing.GetString("host") != site.GetString("host") {
				return failed(fmt.Sprintf("%s is a domain of another server.", name))
			}
			// from an earlier attempt: its records are set again, as what
			// failed may be fixed by now
			if err := p.syncDomainDNS(ctx, existing.Id, true); err != nil {
				p.Log.Warn("failed to set a new site's records again", "domain", name, "error", err)
			}
			continue
		}
		domain := core.NewRecord(collection)
		domain.Set("hostname", name)
		domain.Set("host", site.GetString("host"))
		domain.Set("tls", tls)
		if name == payload.Hostname {
			domain.Set("target_kind", schema.TargetSite)
			domain.Set("site", site.Id)
		} else {
			domain.Set("target_kind", schema.TargetRedirect)
			domain.Set("redirect_to", "https://"+payload.Hostname)
			domain.Set("redirect_permanent", true)
			domain.Set("dns_alias", payload.Hostname)
		}
		if err := p.App.SaveWithContext(ctx, domain); err != nil {
			return failed(fmt.Sprintf("%s cannot be added: %v", name, err))
		}
		created = append(created, domain.Id)
	}
	return p.Commands.Report(step.Id, map[string]any{"step": "dns", "message": "Setting the records", "created": created}, time.Time{})
}

// workOnDNS settles the DNS step once the provider holds the records, or at
// once in manual mode, with the records to make by hand.
func (p *Panel) workOnDNS(step *core.Record, domains []*core.Record) {
	if len(domains) == 0 {
		_ = p.Commands.Settle(step.Id, schema.CommandFailed, "The site's domains are gone.", nil)
		return
	}
	var records []map[string]any
	mode, zone, provider, lan := "automatic", "", "", false
	for _, domain := range domains {
		want := p.desiredDNS(domain)
		if want.Problem != "" {
			_ = p.Commands.Settle(step.Id, schema.CommandFailed, want.Problem, nil)
			return
		}
		if want.Manual {
			mode = "manual"
		}
		lan = lan || want.LAN
		zone = want.ZoneName
		if want.Provider != nil {
			provider = want.Provider.GetString("name")
		}
		records = append(records, recordsOf(want, domain.GetString("hostname"))...)
		switch domain.GetString("dns_status") {
		case schema.DNSError:
			// an error from before the step is what the step sets right
			if domain.GetDateTime("updated").Time().Before(step.GetDateTime("started_at").Time()) {
				return
			}
			_ = p.Commands.Settle(step.Id, schema.CommandFailed, domain.GetString("dns_error"), nil)
			return
		case schema.DNSPending, schema.DNSOK, schema.DNSManual:
		default:
			return // not synced yet
		}
	}
	result := map[string]any{"mode": mode, "zone": zone, "provider": provider, "records": records, "created": createdOf(step), "lan": lan}
	_ = p.Commands.Settle(step.Id, schema.CommandSucceeded, "", result)
}

// createdOf are the domains a DNS step created.
func createdOf(step *core.Record) []string {
	var progress struct {
		Created []string `json:"created"`
	}
	raw, _ := json.Marshal(step.Get("progress"))
	_ = json.Unmarshal(raw, &progress)
	return progress.Created
}

// recordsOf are a domain's records as the wizard shows them: full names.
func recordsOf(want domainDNS, hostname string) []map[string]any {
	zone := want.ZoneName
	if zone == "" {
		zone = hostname
	}
	out := make([]map[string]any, 0, len(want.Records))
	for _, r := range want.Records {
		name := zone
		if r.Name != "@" {
			name = r.Name + "." + zone
		}
		out = append(out, map[string]any{"type": r.Type, "name": name, "value": strings.TrimSuffix(r.Value, "."), "ttl": r.TTL})
	}
	return out
}

// workOnPropagation asks the resolvers until every record of the flow's
// domains resolves: often at first, then once a minute, as records made by
// hand can take a while.
func (p *Panel) workOnPropagation(parent, step *core.Record, domains []*core.Record) {
	if len(domains) == 0 {
		_ = p.Commands.Settle(step.Id, schema.CommandFailed, "The site's domains are gone.", nil)
		return
	}
	waited := time.Since(step.GetDateTime("started_at").Time())
	if last, ok := p.provision.checked.Load(step.Id); ok && waited > 5*time.Minute && time.Since(last.(time.Time)) < time.Minute {
		return
	}
	p.provision.checked.Store(step.Id, time.Now())

	waiting := []string{}
	for _, domain := range domains {
		if domain.GetString("dns_status") == schema.DNSOK {
			continue
		}
		p.checkDomainPropagation(context.Background(), domain)
		if fresh, err := p.App.FindRecordById(schema.Domains, domain.Id); err == nil && fresh.GetString("dns_status") == schema.DNSOK {
			continue
		}
		waiting = append(waiting, domain.GetString("hostname"))
	}
	if len(waiting) == 0 {
		p.provision.checked.Delete(step.Id)
		result := map[string]any{"resolvers": p.checker().Resolvers}
		if lan, server := p.provisionLAN(parent); lan {
			// the DNS server of the LAN answers what it took
			result = map[string]any{"resolvers": []string{}, "lan": server}
		}
		_ = p.Commands.Settle(step.Id, schema.CommandSucceeded, "", result)
		return
	}
	_ = p.Commands.Report(step.Id, map[string]any{
		"step": "propagation", "message": "Waiting for " + strings.Join(waiting, " and "), "waiting": waiting,
	}, time.Time{})
}

// startProvisionDeploy sends the site's first deployment, or a new one when
// the step runs again.
func (p *Panel) startProvisionDeploy(parent *core.Record) error {
	payload := payloadOf(parent)
	site, err := p.App.FindRecordById(schema.Sites, payload.Site)
	if err != nil {
		// nothing is left to set up: the flow ends here
		_ = p.Commands.Settle(parent.Id, schema.CommandFailed, "The site is gone.", nil)
		return nil //nolint:nilerr // the flow is settled, so there is no step to start
	}
	request := deploymentRequest{
		Trigger: schema.TriggerCreate, RequestedBy: parent.GetString("requested_by"),
		Requester: parent.GetString("requested_label"), Parent: parent.Id,
	}
	// an upload sent with the new site waits, recorded, for this step,
	// unless a newer deployment came first: then that one's source is used
	held, err := p.App.FindRecordsByFilter(schema.Deployments, "site={:s} && command='' && status={:q}", "number", 1, 0,
		dbx.Params{"s": site.Id, "q": schema.DeploymentQueued})
	if err == nil && len(held) == 1 {
		newer, _ := p.App.CountRecords(schema.Deployments, dbx.And(dbx.HashExp{"site": site.Id}, dbx.NewExp("number > {:n}", dbx.Params{"n": held[0].GetInt("number")})))
		if newer == 0 && held[0].GetString("upload") != "" {
			return p.sendDeployment(context.Background(), site, held[0], request)
		}
		p.setFields(held[0], map[string]any{"status": schema.DeploymentFailed, "error": "A newer deployment came first.", "finished_at": time.Now().UTC()})
	}
	_, err = p.startDeployment(context.Background(), site, request)
	return err
}

// provisionNames are the names of a flow's certificate.
// provisionLAN tells whether a flow's name is in a zone of a DNS server of a
// LAN (#26), and which: its domains get a certificate of the internal CA,
// nothing propagates, and the site is checked at its server's address on its
// network.
func (p *Panel) provisionLAN(parent *core.Record) (bool, string) {
	collection, err := p.App.FindCachedCollectionByNameOrId(schema.Domains)
	if err != nil {
		return false, ""
	}
	domain := core.NewRecord(collection)
	domain.Set("hostname", payloadOf(parent).Hostname)
	domain.Set("host", parent.GetString("host"))
	want := p.desiredDNS(domain)
	if want.Manual || !lanDriver(want.Provider) {
		return false, ""
	}
	return true, want.Provider.GetString("name")
}

func provisionNames(payload provisionPayload) []string {
	names := []string{payload.Hostname}
	if payload.WWW != "" {
		names = append(names, payload.WWW)
	}
	return names
}

// orderProvisionCertificate orders one certificate for the flow's names,
// unless one covers them, held back until the certificate step. It is called
// under the ACME manager's planning lock.
func (p *Panel) orderProvisionCertificate(parent *core.Record) {
	payload := payloadOf(parent)
	names := provisionNames(payload)
	internal, _ := p.provisionLAN(parent)
	if p.provisionCertificate(names, internal) != nil {
		return
	}
	collection, err := p.App.FindCachedCollectionByNameOrId(schema.Certificates)
	if err != nil {
		return
	}
	cert := core.NewRecord(collection)
	cert.Set("names", names)
	cert.Set("status", schema.CertPending)
	if internal {
		cert.Set("issuer", schema.IssuerInternal)
	} else {
		cert.Set("issuer", schema.IssuerACME)
		cert.Set("challenge", payload.Challenge)
	}
	// renewed while a domain needs it, and dropped when none does
	cert.Set("auto", true)
	cert.Set("next_attempt", time.Now().Add(heldBack).UTC())
	if err := p.App.Save(cert); err != nil {
		p.Log.Error("failed to order a new site's certificate", "names", names, "error", err)
	}
}

// startProvisionCertificate starts an attempt at the flow's certificate, as
// the step's own: what an earlier attempt said is not this step's outcome.
func (p *Panel) startProvisionCertificate(parent, step *core.Record) error {
	payload := payloadOf(parent)
	names := provisionNames(payload)
	internal, _ := p.provisionLAN(parent)
	if p.installedCovering(names, parent.GetString("host"), internal) != nil {
		return nil // the step sees it is done
	}
	cert := p.provisionCertificate(names, internal)
	if cert == nil {
		p.acme.planning.Lock()
		p.orderProvisionCertificate(parent)
		p.acme.planning.Unlock()
		if cert = p.provisionCertificate(names, internal); cert == nil {
			return p.Commands.Settle(step.Id, schema.CommandFailed, "No certificate could be ordered for "+strings.Join(names, " and ")+".", nil)
		}
	}
	if cert.GetString("key_pem") != "" {
		return nil // issued: it waits for the edge
	}
	if until, limited := dailyLimit(cert, time.Now()); limited {
		return p.Commands.Settle(step.Id, schema.CommandFailed,
			fmt.Sprintf("The CA's daily limit for these names is reached: retry after %s.", until.UTC().Format(time.RFC1123)), map[string]any{"certificate": cert.Id})
	}
	p.setFields(cert, map[string]any{"status": schema.CertPending, "last_error": "", "next_attempt": nil})
	message := "Asking the CA"
	if internal {
		message = "Signing with the internal CA"
	}
	_ = p.Commands.Report(step.Id, map[string]any{"step": "certificate", "message": message, "certificate": cert.Id}, time.Time{})
	p.startIssue(cert.Id, parent.GetString("requested_label"), false)
	return nil
}

// provisionCertificate is a certificate of the kind the names' domains take
// that covers all of them: one that is valid now, or one of ACME or the
// internal CA on its way.
func (p *Panel) provisionCertificate(names []string, internal bool) *core.Record {
	records, err := p.App.FindAllRecords(schema.Certificates)
	if err != nil {
		return nil
	}
	var found *core.Record
	for _, record := range records {
		covered := certificateNames(record)
		if (record.GetString("issuer") == schema.IssuerInternal) != internal {
			continue
		}
		if !slices.ContainsFunc(names, func(name string) bool { return !edge.Covers(covered, name) }) {
			if record.GetString("key_pem") != "" && time.Now().Before(record.GetDateTime("not_after").Time()) {
				return record
			}
			if record.GetString("issuer") != schema.IssuerManual && found == nil {
				found = record
			}
		}
	}
	return found
}

// installedCovering is a valid certificate for all the names that the
// server's edge has, of the kind their domains take: the edge picks one per
// name itself.
func (p *Panel) installedCovering(names []string, hostID string, internal bool) *core.Record {
	records, err := p.App.FindAllRecords(schema.Certificates)
	if err != nil {
		return nil
	}
	for _, record := range records {
		covered := certificateNames(record)
		if (record.GetString("issuer") == schema.IssuerInternal) != internal {
			continue
		}
		if record.GetString("key_pem") != "" && time.Now().Before(record.GetDateTime("not_after").Time()) &&
			installedOf(record)[hostID] != "" &&
			!slices.ContainsFunc(names, func(name string) bool { return !edge.Covers(covered, name) }) {
			return record
		}
	}
	return nil
}

// workOnCertificate settles the certificate step once a certificate for the
// names is on the site's edge, or fails it with what the CA said of the
// step's own attempt.
func (p *Panel) workOnCertificate(parent, step *core.Record, payload provisionPayload) {
	names := provisionNames(payload)
	internal, _ := p.provisionLAN(parent)
	if cert := p.installedCovering(names, parent.GetString("host"), internal); cert != nil {
		_ = p.Commands.Settle(step.Id, schema.CommandSucceeded, "", map[string]any{
			"certificate": cert.Id, "names": certificateNames(cert), "issuer": cert.GetString("issuer_name"),
			"not_after": cert.GetDateTime("not_after").String(),
		})
		return
	}
	cert := p.provisionCertificate(names, internal)
	if cert == nil {
		_ = p.Commands.Settle(step.Id, schema.CommandFailed, "The certificate for these names is gone.", nil)
		return
	}
	issued := cert.GetString("key_pem") != "" && time.Now().Before(cert.GetDateTime("not_after").Time())
	// the step started its attempt by setting the order pending: a failure
	// now is that attempt's
	if !issued && cert.GetString("status") == schema.CertFailed &&
		!cert.GetDateTime("updated").Time().Before(step.GetDateTime("started_at").Time()) {
		message := cert.GetString("last_error")
		if message == "" {
			message = "The certificate could not be issued."
		}
		_ = p.Commands.Settle(step.Id, schema.CommandFailed, message, map[string]any{"certificate": cert.Id})
		return
	}
	text := "Waiting for the certificate"
	if issued {
		text = "Installing the certificate on the server"
	} else if reason := cert.GetString("last_error"); reason != "" && cert.GetString("status") != schema.CertFailed {
		text = reason
	}
	_ = p.Commands.Report(step.Id, map[string]any{"step": "certificate", "message": text, "certificate": cert.Id}, time.Time{})
}

// workOnCheck asks the site's server for the site over HTTPS, and for the www
// name, which must redirect to it.
func (p *Panel) workOnCheck(parent, step *core.Record, payload provisionPayload) {
	host, err := p.App.FindRecordById(schema.Hosts, parent.GetString("host"))
	if err != nil {
		_ = p.Commands.Settle(step.Id, schema.CommandFailed, "The server is gone.", nil)
		return
	}
	address := host.GetString("public_ip4")
	if address == "" {
		address = host.GetString("public_ip6")
	}
	var roots *x509.CertPool
	if lan, _ := p.provisionLAN(parent); lan {
		// a name of the LAN is reached at the server's address there, with a
		// certificate of the internal CA
		address = host.GetString("ip4")
		if address == "" {
			address = host.GetString("ip6")
		}
		if authority := p.authority.Load(); authority != nil {
			roots = x509.NewCertPool()
			roots.AppendCertsFromPEM(authority.RootPEM())
		}
	}
	// asked every checkEvery, whatever woke the flow
	if last, ok := p.provision.checked.Load(step.Id); ok && time.Since(last.(time.Time)) < p.provision.checkEvery {
		return
	}
	p.provision.checked.Store(step.Id, time.Now())
	var progress struct {
		Attempts int `json:"attempts"`
	}
	raw, _ := json.Marshal(step.Get("progress"))
	_ = json.Unmarshal(raw, &progress)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// the template's health check, such as Laravel's /up (#52)
	path := "/"
	if payload.PHP != nil && payload.PHP.Health != "" {
		path = payload.PHP.Health
	}
	url := "https://" + payload.Hostname + path
	answer, err := p.provision.siteCheck(ctx, url, address, roots)
	problem := ""
	switch {
	case err != nil:
		problem = fmt.Sprintf("%s: %v", url, err)
	case answer.Status >= 400 && answer.Status != http.StatusUnauthorized:
		problem = fmt.Sprintf("%s answered %d.", url, answer.Status)
	}
	var wwwAnswer siteAnswer
	if problem == "" && payload.WWW != "" {
		wwwURL := "https://" + payload.WWW + "/"
		wwwAnswer, err = p.provision.siteCheck(ctx, wwwURL, address, roots)
		switch {
		case err != nil:
			problem = fmt.Sprintf("%s: %v", wwwURL, err)
		case wwwAnswer.Status != http.StatusMovedPermanently && wwwAnswer.Status != http.StatusPermanentRedirect:
			problem = fmt.Sprintf("%s answered %d, not a redirect.", wwwURL, wwwAnswer.Status)
		case !strings.HasPrefix(wwwAnswer.Location, "https://"+payload.Hostname):
			problem = fmt.Sprintf("%s redirects to %s.", wwwURL, wwwAnswer.Location)
		}
	}
	if problem == "" {
		result := map[string]any{"url": url, "status": answer.Status, "tls": answer.TLS, "protocol": answer.Protocol}
		if payload.WWW != "" {
			result["www"] = map[string]any{"status": wwwAnswer.Status, "location": wwwAnswer.Location}
		}
		p.provision.checked.Delete(step.Id)
		_ = p.Commands.Settle(step.Id, schema.CommandSucceeded, "", result)
		return
	}
	progress.Attempts++
	if time.Since(step.GetDateTime("started_at").Time()) >= p.provision.checkFor {
		p.provision.checked.Delete(step.Id)
		_ = p.Commands.Settle(step.Id, schema.CommandFailed, problem, nil)
		return
	}
	_ = p.Commands.Report(step.Id, map[string]any{"step": "check", "message": problem, "attempts": progress.Attempts}, time.Time{})
}

// pauseProvision holds a flow whose step failed: Retry runs the step again,
// and without one the flow gives up after a day.
func (p *Panel) pauseProvision(parent, step *core.Record) {
	p.pause(parent, step.Id, step.GetString("type"), step.GetString("error"))
}

// pauseStart holds a flow whose next step could not even start, such as a
// deployment whose upload is gone: Retry starts it.
func (p *Panel) pauseStart(parent *core.Record, step string, err error) {
	p.pause(parent, "start:"+step, step, capitalize(err.Error()))
}

// pause records what a flow waits for: the step to retry, by its command or
// as start:<type> for one that never started.
func (p *Panel) pause(parent *core.Record, paused, step, reason string) {
	if pausedOf(parent) == paused {
		return
	}
	payload := payloadOf(parent)
	_ = p.Commands.Report(parent.Id, map[string]any{
		"step": step, "message": fmt.Sprintf("%s: %s", step, reason), "paused": paused,
	}, time.Now().Add(provisionPause))
	events.Log(p.App, parent.GetString("host"), "site.provision_failed", schema.SeverityWarning,
		fmt.Sprintf("Setting up %s stopped at %s: retry the step or cancel.", payload.Hostname, step),
		map[string]any{"site": payload.Site, "command": parent.Id})
}

// pausedOf is what a paused flow waits to retry, or nothing.
func pausedOf(parent *core.Record) string {
	var progress struct {
		Paused string `json:"paused"`
	}
	raw, _ := json.Marshal(parent.Get("progress"))
	_ = json.Unmarshal(raw, &progress)
	return progress.Paused
}

// errNoRetry says why a flow has no step to retry.
type errNoRetry struct{ reason string }

func (e errNoRetry) Error() string { return e.reason }

// retryProvision runs a paused flow's step again, under the flow's lock: what
// was read before may have changed, by another Retry or by the end of the flow.
func (p *Panel) retryProvision(parentID string) error {
	lock := p.provision.locks.of(parentID)
	lock.Lock()
	defer lock.Unlock()

	parent, err := p.App.FindRecordById(schema.Commands, parentID)
	if err != nil {
		return err
	}
	if parent.GetString("status") != schema.CommandRunning {
		return errNoRetry{"This setup has ended."}
	}
	paused := pausedOf(parent)
	step := ""
	if after, ok := strings.CutPrefix(paused, "start:"); ok {
		step = after
	} else {
		steps := p.stepsOf(parentID)
		if len(steps) == 0 {
			return errNoRetry{"This setup has no step to retry."}
		}
		last := steps[len(steps)-1]
		if paused == "" || paused != last.Id {
			return errNoRetry{"No step of this setup has failed."}
		}
		step = last.GetString("type")
	}
	if err := p.Commands.Report(parentID, map[string]any{"step": step, "message": "Retrying", "paused": ""}, time.Now().Add(provisionFor)); err != nil {
		return err
	}
	if err := p.startProvisionStep(parent, step); err != nil {
		fresh, _ := p.App.FindRecordById(schema.Commands, parentID)
		if fresh != nil {
			p.pauseStart(fresh, step, err)
		}
		return errNoRetry{capitalize(err.Error()) + "."}
	}
	return nil
}

// handleProvisionRetry runs a flow's failed step again.
func (p *Panel) handleProvisionRetry(e *core.RequestEvent) error {
	parent, err := e.App.FindRecordById(schema.Commands, e.Request.PathValue("id"))
	if err != nil || parent.GetString("type") != "site.provision" {
		return e.NotFoundError("The new site's setup does not exist.", err)
	}
	var refused errNoRetry
	switch err := p.retryProvision(parent.Id); {
	case errors.As(err, &refused):
		return e.BadRequestError(refused.reason, nil)
	case err != nil:
		return e.InternalServerError("Failed to retry the step.", err)
	}
	p.later.after(0, func() { p.advanceProvision(parent.Id) })
	return e.NoContent(http.StatusNoContent)
}

// abandonProvision undoes what a flow that ended without its site set up
// made public: the domains it created go, and with them their records. A step
// still under way is cancelled, and a deployment that never left waits no
// more.
func (p *Panel) abandonProvision(parentID string) {
	// a Retry under way finishes first, and none starts a step after
	lock := p.provision.locks.of(parentID)
	lock.Lock()
	defer lock.Unlock()
	parent, err := p.App.FindRecordById(schema.Commands, parentID)
	if err != nil {
		return
	}
	payload := payloadOf(parent)
	created := map[string]bool{}
	for _, step := range p.stepsOf(parentID) {
		if !isFinalStatus(step.GetString("status")) {
			if err := p.Commands.Cancel(step.Id, "The new site's setup ended."); err != nil {
				p.Log.Warn("failed to cancel a step", "command", step.Id, "error", err)
			}
		}
		if step.GetString("type") == "site.dns" {
			for _, id := range createdOf(step) {
				created[id] = true
			}
		}
	}
	removed := []string{}
	// the www alias goes with its site's domain; the name is checked, as the
	// id could be another domain's by now only if it was removed meanwhile
	for _, domain := range p.provisionDomains(payload, parent.GetString("host")) {
		if !created[domain.Id] {
			continue
		}
		actor := audit.System("new site setup", audit.SourcePanel)
		actor.Command = parentID
		if err := p.App.DeleteWithContext(audit.WithActor(context.Background(), actor), domain); err != nil {
			p.Log.Warn("failed to remove a new site's domain", "domain", domain.GetString("hostname"), "error", err)
			continue
		}
		removed = append(removed, domain.GetString("hostname"))
	}
	if held, err := p.App.FindRecordsByFilter(schema.Deployments, "site={:s} && command='' && status={:q}", "", 0, 0,
		dbx.Params{"s": payload.Site, "q": schema.DeploymentQueued}); err == nil {
		for _, deployment := range held {
			p.setFields(deployment, map[string]any{"status": schema.DeploymentFailed, "error": "The new site's setup ended before it was deployed."})
		}
	}
	message := fmt.Sprintf("Setting up %s ended: %s", payload.Hostname, strings.TrimSuffix(parent.GetString("error"), ".")+".")
	if len(removed) > 0 {
		message += fmt.Sprintf(" Removed %s, with its DNS records; the site %s stays.", strings.Join(removed, " and "), payload.SiteName)
	}
	events.Log(p.App, parent.GetString("host"), "site.provision_ended", schema.SeverityWarning, message,
		map[string]any{"site": payload.Site, "command": parentID})
}

func isFinalStatus(status string) bool {
	switch status {
	case schema.CommandSucceeded, schema.CommandFailed, schema.CommandTimedOut, schema.CommandCancelled:
		return true
	}
	return false
}

// resumeProvisions picks up the flows a restart interrupted.
func (p *Panel) resumeProvisions() { p.advanceProvisions() }

// dnsPreview is what the wizard shows while the name is typed.
type dnsPreview struct {
	Hostname string           `json:"hostname"`
	Zone     string           `json:"zone"`
	Provider string           `json:"provider"`
	Kind     string           `json:"kind"`
	Mode     string           `json:"mode"`
	Records  []map[string]any `json:"records"`
	Taken    []string         `json:"taken"`
	Problem  string           `json:"problem,omitempty"`
	// LAN is true for a zone of a DNS server of a LAN: the certificate comes
	// from the internal CA
	LAN bool `json:"lan"`
}

// previewDNS is what a new site's names would need, without saving anything.
func (p *Panel) previewDNS(hostname, hostID, www string) dnsPreview {
	preview := dnsPreview{Hostname: hostname, Mode: "automatic", Records: []map[string]any{}, Taken: []string{}}
	collection, err := p.App.FindCachedCollectionByNameOrId(schema.Domains)
	if err != nil {
		preview.Problem = err.Error()
		return preview
	}
	for _, name := range []string{hostname, www} {
		if name == "" {
			continue
		}
		domain := core.NewRecord(collection)
		domain.Set("hostname", name)
		domain.Set("host", hostID)
		if name == www {
			domain.Set("dns_alias", hostname)
		}
		want := p.desiredDNS(domain)
		if want.Problem != "" {
			preview.Problem = want.Problem
			return preview
		}
		if want.Manual {
			preview.Mode = "manual"
		}
		preview.Zone = want.ZoneName
		preview.LAN = !want.Manual && lanDriver(want.Provider)
		if want.Provider != nil {
			preview.Provider = want.Provider.GetString("name")
			preview.Kind = want.Provider.GetString("kind")
		}
		preview.Records = append(preview.Records, recordsOf(want, name)...)
		if taken, _ := p.App.CountRecords(schema.Domains, dbx.HashExp{"hostname": name}); taken > 0 {
			preview.Taken = append(preview.Taken, name)
		}
	}
	return preview
}

// handleDNSPreview says, while a new site's name is typed, which zone holds it
// and which records it needs.
func (p *Panel) handleDNSPreview(e *core.RequestEvent) error {
	query := e.Request.URL.Query()
	hostname := strings.ToLower(strings.TrimSpace(query.Get("hostname")))
	if err := edge.ValidHostname(hostname); err != nil || strings.HasPrefix(hostname, "*.") {
		return e.BadRequestError("Enter a name such as blog.example.com.", nil)
	}
	host, err := e.App.FindRecordById(schema.Hosts, query.Get("host"))
	if err != nil {
		return e.BadRequestError("Choose the server.", nil)
	}
	www := ""
	if query.Get("www") == "true" && !strings.HasPrefix(hostname, "www.") {
		www = "www." + hostname
	}
	return e.JSON(http.StatusOK, p.previewDNS(hostname, host.Id, www))
}

// askSite asks a server for a site over HTTPS, at its public address with the
// site's name, as a visitor would once the name resolves there. Redirects are
// not followed: the www name's redirect is what is checked of it.
func askSite(ctx context.Context, url, address string, roots *x509.CertPool) (siteAnswer, error) {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(address, port))
		},
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return siteAnswer{}, err
	}
	request.Header.Set("User-Agent", "PocketCloud availability check")
	response, err := client.Do(request)
	if err != nil {
		return siteAnswer{}, err
	}
	defer response.Body.Close()
	answer := siteAnswer{Status: response.StatusCode, Location: response.Header.Get("Location"), Protocol: response.Proto}
	if response.TLS != nil {
		answer.TLS = tls.VersionName(response.TLS.Version)
	}
	return answer, nil
}

// checkProvisionPHP checks what a new PHP site was asked for, with the
// defaults for what was left out (#50).
func (p *Panel) checkProvisionPHP(request *provisionPHP, site, host *core.Record) (*provisionPHP, error) {
	checked := provisionPHP{Version: defaultPHPVersion(time.Now()), Template: schema.PHPCustom}
	if request != nil {
		checked = *request
		if checked.Version == "" {
			checked.Version = defaultPHPVersion(time.Now())
		}
		if checked.Template == "" {
			checked.Template = schema.PHPCustom
		}
	}
	checked.Docroot = strings.Trim(strings.TrimSpace(checked.Docroot), "/")
	if _, found := php.Find(checked.Version); !found {
		return nil, fmt.Errorf("PocketCloud has no runtime image of PHP %s", checked.Version)
	}
	if checked.Docroot != "" && (!docrootPath.MatchString(checked.Docroot) || slices.Contains(strings.Split(checked.Docroot, "/"), "..")) {
		return nil, errors.New("the web root is a directory of the release, such as public, or empty for the release itself")
	}
	t, templated := phpapps.Get(checked.Template)
	if !templated && checked.Template != schema.PHPCustom {
		return nil, fmt.Errorf("the application is custom or one of %s", strings.Join(phpapps.Names(), ", "))
	}
	checked.Health = ""
	if templated {
		checked.Health = t.Health
		if checked.Docroot == "" {
			checked.Docroot = t.Docroot
		}
		if t.Database == phpapps.DatabaseRequired && checked.DatabaseServer == "" {
			return nil, fmt.Errorf("%s keeps its data in a database: choose a database server for it", t.Title)
		}
	}
	checked.QueueWorker = checked.QueueWorker && templated && t.Worker != nil
	switch {
	case checked.Install != nil && (!templated || t.Fresh == nil):
		return nil, errors.New("only an application with a fresh install, such as WordPress, is installed fresh")
	case checked.Install != nil && site.GetString("source") != sites.SourceEmpty:
		return nil, errors.New("a fresh install has no source of its own: its source is empty")
	case checked.Install == nil && templated && t.Fresh != nil && site.GetString("source") == sites.SourceEmpty:
		return nil, fmt.Errorf("a new %s without a source of its own is installed fresh: give its title and administrator", t.Title)
	case templated && t.Fresh == nil && site.GetString("source") == sites.SourceEmpty:
		return nil, fmt.Errorf("a %s site comes from Git or an upload", t.Title)
	case checked.Install != nil:
		install := *checked.Install
		if err := checkInstall(&install); err != nil {
			return nil, err
		}
		checked.Install = &install
		// the release is WordPress, whatever the wizard sent
		checked.Docroot = t.Docroot
	}
	if checked.DatabaseServer == "" {
		checked.Database = ""
		return &checked, nil
	}
	server, err := p.App.FindRecordById(schema.DBServers, checked.DatabaseServer)
	if err != nil || server.GetString("host") != host.Id {
		return nil, errors.New("the database server is not one of the site's server")
	}
	engine, err := engineOfServer(server)
	if err != nil {
		return nil, err
	}
	if checked.Database == "" {
		checked.Database = strings.ReplaceAll(site.GetString("name"), "-", "_")
	}
	if err := databases.ValidDatabaseName(engine, checked.Database); err != nil {
		return nil, err
	}
	if used, _ := p.App.FindRecordsByFilter(schema.DBUsers, "secret_name={:secret} && server.host={:host}", "", 1, 0,
		dbx.Params{"secret": databases.SecretName(checked.Database), "host": host.Id}); len(used) > 0 {
		return nil, fmt.Errorf("this server already has a database named %s: choose another name", checked.Database)
	}
	return &checked, nil
}

// startProvisionPHP starts a new PHP site's database or PHP-FPM, as commands
// of the agent with the flow as their parent (#50).
func (p *Panel) startProvisionPHP(parent *core.Record, step string) error {
	payload := payloadOf(parent)
	site, err := p.App.FindRecordById(schema.Sites, payload.Site)
	if err != nil {
		_ = p.Commands.Settle(parent.Id, schema.CommandFailed, "The site is gone.", nil)
		return nil //nolint:nilerr // the flow is settled, so there is no step to start
	}
	if payload.PHP == nil {
		return errors.New("the setup is not one of a PHP site")
	}
	ctx := context.Background()
	if step == "site.php.apply" {
		// the database made for the site is connected to it as it is made; a
		// connection that failed is tried again here, and says why it fails
		if err := p.connectProvisionDatabase(ctx, parent, site, payload.PHP); err != nil {
			return err
		}
		_, err := p.applyPHP(ctx, site, commands.Options{
			RequestedBy: parent.GetString("requested_by"), Requester: parent.GetString("requested_label"),
			Parent: parent.Id, Correlation: parent.Id,
		})
		return err
	}
	server, err := p.App.FindRecordById(schema.DBServers, payload.PHP.DatabaseServer)
	if err != nil {
		return errors.New("the database server is gone")
	}
	create := func(user string) *agentv1.Command {
		return &agentv1.Command{Payload: &agentv1.Command_DbDatabaseCreate{DbDatabaseCreate: &agentv1.DbDatabaseCreate{
			Connection: connectionOf(server),
			Database:   payload.PHP.Database,
			User:       user,
		}}}
	}
	// a Retry sends the create again for the records the first try made: the
	// server makes what is missing, and the command's end connects the site
	if database, err := p.App.FindFirstRecordByFilter(schema.Databases, "server={:server} && name={:name} && site={:site}",
		dbx.Params{"server": server.Id, "name": payload.PHP.Database, "site": site.Id}); err == nil {
		_, err = p.Commands.Create(ctx, commands.Options{
			Host: server.GetString("host"), Label: server.GetString("name") + "/" + database.GetString("name"),
			RequestedBy: parent.GetString("requested_by"), Requester: parent.GetString("requested_label"),
			Parent: parent.Id, Correlation: parent.Id, Command: create(database.GetString("name")),
		})
		return err
	}
	_, _, err = p.newDatabase(ctx, databaseRequest{
		Actor: dbActor, UserID: parent.GetString("requested_by"), Label: parent.GetString("requested_label"),
		Site: site.Id, Parent: parent.Id,
	}, server, payload.PHP.Database, create)
	return err
}

// connectProvisionDatabase connects a new PHP site to the database its setup
// made, unless it is connected already.
func (p *Panel) connectProvisionDatabase(ctx context.Context, parent, site *core.Record, request *provisionPHP) error {
	if request.DatabaseServer == "" {
		return nil
	}
	settings, err := p.phpSettings(p.App, site)
	if err != nil {
		return err
	}
	if settings.GetString("database") != "" {
		return nil
	}
	server, err := p.App.FindRecordById(schema.DBServers, request.DatabaseServer)
	if err != nil {
		return errors.New("the database server is gone")
	}
	database, err := p.App.FindFirstRecordByFilter(schema.Databases, "server={:server} && name={:name} && site={:site}",
		dbx.Params{"server": server.Id, "name": request.Database, "site": site.Id})
	if err != nil {
		return errors.New("the site's database is gone")
	}
	connect := databaseRequest{Actor: dbActor, UserID: parent.GetString("requested_by"), Label: parent.GetString("requested_label")}
	if err := p.connectSite(ctx, connect, server, database, site, false); err != nil {
		return fmt.Errorf("the site could not be connected to its database: %w", err)
	}
	return nil
}
