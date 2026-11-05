package panel

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
	"github.com/pocketbase/pocketbase/tools/router"

	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/edge"
	"github.com/oldbear24/PocketServerManager/internal/events"
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

const (
	// provisionPause is how long a failed step waits for Retry before the
	// flow gives up.
	provisionPause = 24 * time.Hour
	// provisionFor is how long a flow may take in all.
	provisionFor = 7 * 24 * time.Hour
	// checkAttempts is how often the availability check asks, a few seconds
	// apart, before it fails: a new route and certificate can take a moment.
	checkAttempts = 20
)

// provisionState is the work of the flows under way.
type provisionState struct {
	// tick is how soon a waiting step looks again.
	tick time.Duration
	// locks keeps one flow from being advanced twice at once.
	locks keyedLocks
	// noFirstDeploy holds the sites a flow creates: their first deployment is
	// one of its steps, not something the new site starts by itself.
	noFirstDeploy sync.Map
	// checked is when each flow last asked about its propagation.
	checked sync.Map
	// siteCheck asks a site's server for it; tests replace it.
	siteCheck func(ctx context.Context, url, address string) (siteAnswer, error)
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

// SetProvisionTick sets how soon a waiting step of a new site looks again.
func (p *Panel) SetProvisionTick(tick time.Duration) { p.provision.tick = tick }

// SetSiteCheck replaces how the availability check asks a site's server.
func (p *Panel) SetSiteCheck(check func(ctx context.Context, url, address string) (status int, location string, err error)) {
	p.provision.siteCheck = func(ctx context.Context, url, address string) (siteAnswer, error) {
		status, location, err := check(ctx, url, address)
		return siteAnswer{Status: status, Location: location}, err
	}
}

func (p *Panel) registerProvision() {
	if p.provision.tick == 0 {
		p.provision.tick = 3 * time.Second
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
	group.POST("/sites/provision", p.handleProvision).BindFunc(operators)
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
}

func payloadOf(command *core.Record) provisionPayload {
	var payload provisionPayload
	raw, _ := json.Marshal(command.Get("payload"))
	_ = json.Unmarshal(raw, &payload)
	return payload
}

// siteFields are what the wizard may set of a new site.
var siteFields = []string{
	"name", "host", "source", "repo_url", "branch", "generator", "build_image", "build_cmd", "output_dir",
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

	userID, label := requester(e)
	parent, err := p.Commands.Begin(ctx, commands.Options{
		Host:        host.Id,
		Type:        "site.provision",
		Label:       fmt.Sprintf("%s at %s", site.GetString("name"), hostname),
		RequestedBy: userID,
		Requester:   label,
	}, map[string]any{
		"site": site.Id, "site_name": site.GetString("name"), "hostname": hostname, "www": www, "challenge": challenge,
	})
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
		// another call is at it; it looks again on its own
		return
	}
	defer lock.Unlock()

	for range len(provisionSteps) + 1 {
		parent, err := p.App.FindRecordById(schema.Commands, parentID)
		if err != nil || parent.GetString("status") != schema.CommandRunning {
			return
		}
		steps := p.stepsOf(parentID)
		if len(steps) == 0 {
			return
		}
		last := steps[len(steps)-1]
		status := last.GetString("status")

		switch status {
		case schema.CommandSucceeded:
			index := slices.Index(provisionSteps, last.GetString("type"))
			if index == len(provisionSteps)-1 {
				payload := payloadOf(parent)
				_ = p.Commands.Settle(parentID, schema.CommandSucceeded, "", map[string]any{"url": "https://" + payload.Hostname})
				events.Log(p.App, parent.GetString("host"), "site.provisioned", schema.SeverityInfo,
					fmt.Sprintf("%s is live at https://%s.", payload.SiteName, payload.Hostname), map[string]any{"site": payload.Site, "command": parentID})
				return
			}
			if err := p.startProvisionStep(parent, provisionSteps[index+1]); err != nil {
				p.Log.Error("failed to start a step of a new site", "command", parentID, "step", provisionSteps[index+1], "error", err)
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
			p.later.after(p.provision.tick, func() { p.advanceProvision(parentID) })
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
	if step == "site.deploy" {
		return p.startProvisionDeploy(parent)
	}
	command, err := p.beginStep(parent, step)
	if err != nil {
		return err
	}
	switch step {
	case "site.dns":
		return p.createProvisionDomains(parent, command)
	case "site.certificate":
		p.orderProvisionCertificate(parent, true)
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
		p.workOnPropagation(step, domains)
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
	created := []string{}
	for _, name := range []string{payload.Hostname, payload.WWW} {
		if name == "" {
			continue
		}
		if existing, err := p.App.FindFirstRecordByData(schema.Domains, "hostname", name); err == nil {
			if existing.GetString("host") != site.GetString("host") {
				return p.Commands.Settle(step.Id, schema.CommandFailed, fmt.Sprintf("%s is a domain of another server.", name), nil)
			}
			continue
		}
		domain := core.NewRecord(collection)
		domain.Set("hostname", name)
		domain.Set("host", site.GetString("host"))
		domain.Set("tls", schema.TLSAuto)
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
			return p.Commands.Settle(step.Id, schema.CommandFailed, fmt.Sprintf("%s cannot be added: %v", name, err), nil)
		}
		created = append(created, domain.Id)
	}
	// one certificate covers both names; ordered now, the ACME manager does
	// not order one for each
	p.orderProvisionCertificate(parent, false)
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
	mode, zone, provider := "automatic", "", ""
	for _, domain := range domains {
		want := p.desiredDNS(domain)
		if want.Problem != "" {
			_ = p.Commands.Settle(step.Id, schema.CommandFailed, want.Problem, nil)
			return
		}
		if want.Manual {
			mode = "manual"
		}
		zone = want.ZoneName
		if want.Provider != nil {
			provider = want.Provider.GetString("name")
		}
		records = append(records, recordsOf(want, domain.GetString("hostname"))...)
		switch domain.GetString("dns_status") {
		case schema.DNSError:
			_ = p.Commands.Settle(step.Id, schema.CommandFailed, domain.GetString("dns_error"), nil)
			return
		case schema.DNSPending, schema.DNSOK, schema.DNSManual:
		default:
			return // not synced yet
		}
	}
	result := map[string]any{"mode": mode, "zone": zone, "provider": provider, "records": records, "created": createdOf(step)}
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
func (p *Panel) workOnPropagation(step *core.Record, domains []*core.Record) {
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
		_ = p.Commands.Settle(step.Id, schema.CommandSucceeded, "", map[string]any{"resolvers": p.checker().Resolvers})
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
	// an upload sent with the new site waits, recorded, for this step
	held, err := p.App.FindRecordsByFilter(schema.Deployments, "site={:s} && command='' && status={:q}", "number", 1, 0,
		dbx.Params{"s": site.Id, "q": schema.DeploymentQueued})
	if err == nil && len(held) == 1 {
		return p.sendDeployment(context.Background(), site, held[0], request)
	}
	_, err = p.startDeployment(context.Background(), site, request)
	return err
}

// orderProvisionCertificate orders one certificate for the flow's names,
// unless one covers them, and starts its issue when now is set.
func (p *Panel) orderProvisionCertificate(parent *core.Record, now bool) {
	payload := payloadOf(parent)
	names := []string{payload.Hostname}
	if payload.WWW != "" {
		names = append(names, payload.WWW)
	}
	cert := p.provisionCertificate(names)
	if cert == nil {
		collection, err := p.App.FindCachedCollectionByNameOrId(schema.Certificates)
		if err != nil {
			return
		}
		cert = core.NewRecord(collection)
		cert.Set("names", names)
		cert.Set("issuer", schema.IssuerACME)
		cert.Set("status", schema.CertPending)
		cert.Set("challenge", payload.Challenge)
		// renewed while a domain needs it, and dropped when none does
		cert.Set("auto", true)
		if err := p.App.Save(cert); err != nil {
			p.Log.Error("failed to order a new site's certificate", "names", names, "error", err)
			return
		}
	}
	if now && cert.GetString("issuer") == schema.IssuerACME && cert.GetString("key_pem") == "" {
		p.startIssue(cert.Id, parent.GetString("requested_label"), false)
	}
}

// provisionCertificate is a certificate that covers all the names: one that
// is valid now, or an ACME one on its way.
func (p *Panel) provisionCertificate(names []string) *core.Record {
	records, err := p.App.FindAllRecords(schema.Certificates)
	if err != nil {
		return nil
	}
	var found *core.Record
	for _, record := range records {
		covered := certificateNames(record)
		if !slices.ContainsFunc(names, func(name string) bool { return !edge.Covers(covered, name) }) {
			if record.GetString("key_pem") != "" && time.Now().Before(record.GetDateTime("not_after").Time()) {
				return record
			}
			if record.GetString("issuer") == schema.IssuerACME && found == nil {
				found = record
			}
		}
	}
	return found
}

// workOnCertificate settles the certificate step once a certificate for the
// names is on the site's edge, or fails it with what the CA said.
func (p *Panel) workOnCertificate(parent, step *core.Record, payload provisionPayload) {
	names := []string{payload.Hostname}
	if payload.WWW != "" {
		names = append(names, payload.WWW)
	}
	cert := p.provisionCertificate(names)
	if cert == nil {
		p.orderProvisionCertificate(parent, true)
		return
	}
	issued := cert.GetString("key_pem") != "" && time.Now().Before(cert.GetDateTime("not_after").Time())
	if issued && installedOf(cert)[parent.GetString("host")] != "" {
		_ = p.Commands.Settle(step.Id, schema.CommandSucceeded, "", map[string]any{
			"certificate": cert.Id, "names": certificateNames(cert), "issuer": cert.GetString("issuer_name"),
			"not_after": cert.GetDateTime("not_after").String(),
		})
		return
	}
	if !issued && cert.GetString("status") == schema.CertFailed {
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
	} else if reason := cert.GetString("last_error"); reason != "" {
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
	var progress struct {
		Attempts int `json:"attempts"`
	}
	raw, _ := json.Marshal(step.Get("progress"))
	_ = json.Unmarshal(raw, &progress)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := "https://" + payload.Hostname + "/"
	answer, err := p.provision.siteCheck(ctx, url, address)
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
		wwwAnswer, err = p.provision.siteCheck(ctx, wwwURL, address)
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
		_ = p.Commands.Settle(step.Id, schema.CommandSucceeded, "", result)
		return
	}
	progress.Attempts++
	if progress.Attempts >= checkAttempts {
		_ = p.Commands.Settle(step.Id, schema.CommandFailed, problem, nil)
		return
	}
	_ = p.Commands.Report(step.Id, map[string]any{"step": "check", "message": problem, "attempts": progress.Attempts}, time.Time{})
}

// pauseProvision holds a flow whose step failed: Retry runs the step again,
// and without one the flow gives up after a day.
func (p *Panel) pauseProvision(parent, step *core.Record) {
	var progress struct {
		Paused string `json:"paused"`
	}
	raw, _ := json.Marshal(parent.Get("progress"))
	_ = json.Unmarshal(raw, &progress)
	if progress.Paused == step.Id {
		return
	}
	payload := payloadOf(parent)
	message := fmt.Sprintf("%s: %s", step.GetString("type"), step.GetString("error"))
	_ = p.Commands.Report(parent.Id, map[string]any{
		"step": step.GetString("type"), "message": message, "paused": step.Id,
	}, time.Now().Add(provisionPause))
	events.Log(p.App, parent.GetString("host"), "site.provision_failed", schema.SeverityWarning,
		fmt.Sprintf("Setting up %s stopped at %s: retry the step or cancel.", payload.Hostname, step.GetString("type")),
		map[string]any{"site": payload.Site, "command": parent.Id})
}

// handleProvisionRetry runs a flow's failed step again.
func (p *Panel) handleProvisionRetry(e *core.RequestEvent) error {
	parent, err := e.App.FindRecordById(schema.Commands, e.Request.PathValue("id"))
	if err != nil || parent.GetString("type") != "site.provision" {
		return e.NotFoundError("The new site's setup does not exist.", err)
	}
	if parent.GetString("status") != schema.CommandRunning {
		return e.BadRequestError("This setup has ended.", nil)
	}
	steps := p.stepsOf(parent.Id)
	if len(steps) == 0 {
		return e.BadRequestError("This setup has no step to retry.", nil)
	}
	last := steps[len(steps)-1]
	switch last.GetString("status") {
	case schema.CommandFailed, schema.CommandTimedOut, schema.CommandCancelled:
	default:
		return e.BadRequestError("No step of this setup has failed.", nil)
	}

	lock := p.provision.locks.of(parent.Id)
	lock.Lock()
	err = p.startProvisionStep(parent, last.GetString("type"))
	if err == nil {
		err = p.Commands.Report(parent.Id, map[string]any{"step": last.GetString("type"), "message": "Retrying"}, time.Now().Add(provisionFor))
	}
	lock.Unlock()
	if err != nil {
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
		if err := p.App.Delete(domain); err != nil {
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
	message := fmt.Sprintf("Setting up %s ended: %s", payload.Hostname, parent.GetString("error"))
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
func askSite(ctx context.Context, url, address string) (siteAnswer, error) {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
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
