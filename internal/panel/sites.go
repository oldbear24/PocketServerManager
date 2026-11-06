package panel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/edge"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/gateway"
	"github.com/oldbear24/PocketServerManager/internal/php"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/sites"
)

// Static sites (#21). A site lives on an edge server: each deployment builds
// a release there (site.deploy), the edge serves the newest one that
// succeeded, and a rollback switches back to an older one that is still on
// the server (site.rollback). Its domains are domains like any other, with
// the site as their target, so DNS records and certificates follow.

const (
	// maxSiteUpload is the largest ZIP a site is deployed from.
	maxSiteUpload = 256 << 20
	// maxWebhookBody is the largest push event read.
	maxWebhookBody = 5 << 20
	// defaultKeepReleases is how many releases a site keeps on its server.
	defaultKeepReleases = 5
	// requestHours is the span of a site's request chart.
	requestHours = 24
	// webhookPath is where Git servers send their pushes.
	webhookPath = "/api/pc/hooks/sites/"
)

func (p *Panel) registerSites() {
	// the records API leaves out hidden fields for everyone but superusers;
	// operators may set a site's secrets with its create or update all the same
	p.App.OnRecordCreateRequest(schema.Sites).BindFunc(p.takeSiteSecrets)
	p.App.OnRecordUpdateRequest(schema.Sites).BindFunc(p.takeSiteSecrets)
	p.App.OnRecordCreateRequest(schema.Sites).BindFunc(p.keepSiteBookkeeping)
	p.App.OnRecordUpdateRequest(schema.Sites).BindFunc(p.keepSiteBookkeeping)
	p.App.OnRecordCreate(schema.Sites).BindFunc(p.siteDefaults)
	p.App.OnRecordValidate(schema.Sites).BindFunc(p.validateSite)
	p.App.OnRecordAfterCreateSuccess(schema.Sites).BindFunc(func(e *core.RecordEvent) error {
		site := e.Record
		// a new PHP site's settings exist before anything is sent for it (#50)
		if site.GetString("type") == schema.SitePHPApp {
			if _, err := p.phpSettings(e.App, site); err != nil {
				p.Log.Error("failed to make a PHP site's settings", "site", site.GetString("name"), "error", err)
			}
		}
		// a new site's setup deploys it as one of its steps
		if _, setup := p.provision.noFirstDeploy.LoadAndDelete(site.Id); setup {
			return e.Next()
		}
		if source := site.GetString("source"); source == sites.SourceGit || source == sites.SourceEmpty {
			request := deploymentRequest{Trigger: schema.TriggerCreate, Requester: "panel"}
			if site.GetString("type") == schema.SitePHPApp {
				// its PHP-FPM starts before its first release (#50)
				request.BeforeSend = func(ctx context.Context) error {
					_, err := p.applyPHP(ctx, site, commands.Options{Requester: "panel"})
					return err
				}
			}
			p.later.after(0, func() {
				if _, err := p.startDeployment(context.Background(), site, request); err != nil {
					p.Log.Error("failed to start a site's first deployment", "site", site.GetString("name"), "error", err)
				}
			})
		}
		return e.Next()
	})
	p.App.OnRecordUpdate(schema.Sites).BindFunc(func(e *core.RecordEvent) error {
		// the stored site, not the record's original: a record saved before
		// can carry an original from before that save
		stored, _ := e.App.FindRecordById(schema.Sites, e.Record.Id)
		if err := e.Next(); err != nil {
			return err
		}
		if stored == nil || servedDifferently(stored, e.Record) {
			host := e.Record.GetString("host")
			p.later.after(edgeSettle, func() { p.applyEdgeLogged(host, false) })
		}
		return nil
	})
	p.App.OnRecordAfterDeleteSuccess(schema.Sites).BindFunc(func(e *core.RecordEvent) error {
		site := e.Record
		p.later.after(0, func() { p.removeSiteFiles(site) })
		return e.Next()
	})

	p.Commands.RegisterEnricher("site.deploy", p.enrichSiteDeploy)
	p.Commands.OnFinish("site.deploy", p.siteDeployFinished)
	p.Commands.OnFinish("site.rollback", p.siteRollbackFinished)
	p.App.OnRecordAfterUpdateSuccess(schema.Commands).BindFunc(func(e *core.RecordEvent) error {
		if e.Record.GetString("type") == "site.deploy" && e.Record.GetString("status") == schema.CommandRunning {
			p.deploymentStarted(e.Record)
		}
		return e.Next()
	})
}

func (p *Panel) bindSiteRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	everyone := requireRole(schema.RoleViewer, schema.RoleOperator, schema.RoleAdmin)
	operators := requireRole(schema.RoleOperator, schema.RoleAdmin)

	group.POST("/sites/{id}/deploy", p.handleSiteDeploy).BindFunc(operators)
	group.POST("/sites/{id}/upload", p.handleSiteUpload).BindFunc(operators).Bind(apis.BodyLimit(maxSiteUpload + 1<<20))
	group.POST("/sites/{id}/rollback", p.handleSiteRollback).BindFunc(operators)
	group.PUT("/sites/{id}/credentials", p.handleSiteCredentials).BindFunc(operators)
	group.GET("/sites/{id}/webhook", p.handleSiteWebhookInfo).BindFunc(operators)
	group.POST("/sites/{id}/webhook", p.handleSiteWebhookRotate).BindFunc(operators)
	group.GET("/sites/{id}/requests", p.handleSiteRequests).BindFunc(everyone)
	// PHP sites (#50)
	group.PUT("/sites/{id}/php", p.handlePHPSettings).BindFunc(operators)
	group.PUT("/sites/{id}/php/secrets", p.handlePHPSecrets).BindFunc(operators)
	group.POST("/sites/{id}/php/apply", p.handlePHPApply).BindFunc(operators)
}

// takeSiteSecrets sets the Git token and the passwords from the request, so a
// private site's first deployment already has its token.
func (p *Panel) takeSiteSecrets(e *core.RecordRequestEvent) error {
	// the request data PocketBase hands on has the hidden fields taken out,
	// so the body is read again
	var body struct {
		GitToken  *string `json:"git_token" form:"git_token"`
		BasicAuth *string `json:"basic_auth" form:"basic_auth"`
	}
	if err := e.BindBody(&body); err == nil {
		if body.GitToken != nil {
			e.Record.Set("git_token", strings.TrimSpace(*body.GitToken))
		}
		if body.BasicAuth != nil {
			e.Record.Set("basic_auth", strings.TrimSpace(*body.BasicAuth))
		}
	}
	return e.Next()
}

// keepSiteBookkeeping keeps what the panel records about a site out of the
// records API: which deployment is live is the panel's to say. An update
// keeps it as it is stored now, not as the request read it, which a
// deployment may have changed since.
func (p *Panel) keepSiteBookkeeping(e *core.RecordRequestEvent) error {
	if e.Record.IsNew() {
		e.Record.Set("active_deployment", "")
		return e.Next()
	}
	stored, err := e.App.FindRecordById(schema.Sites, e.Record.Id)
	if err != nil {
		return e.NotFoundError("The site does not exist.", err)
	}
	e.Record.Set("active_deployment", stored.Get("active_deployment"))
	return e.Next()
}

// siteDefaults fills in what a new site leaves out, and its webhook secret.
func (p *Panel) siteDefaults(e *core.RecordEvent) error {
	site := e.Record
	defaults := map[string]any{"type": schema.SiteStatic, "generator": sites.GeneratorAuto, "keep_releases": defaultKeepReleases}
	for field, value := range defaults {
		if site.GetString(field) == "" || site.GetString(field) == "0" {
			site.Set(field, value)
		}
	}
	if site.GetString("source") == sites.SourceGit && site.GetString("branch") == "" {
		site.Set("branch", "main")
	}
	if site.GetString("webhook_secret") == "" {
		site.Set("webhook_secret", randomSecret())
	}
	return e.Next()
}

func randomSecret() string {
	secret := make([]byte, 24)
	_, _ = rand.Read(secret)
	return hex.EncodeToString(secret)
}

var (
	branchName = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,255}$`)
	imageRef   = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:@-]{0,511}$`)
)

// validateSite checks every save of a site, however it arrives.
func (p *Panel) validateSite(e *core.RecordEvent) error {
	site := e.Record
	errs := validation.Errors{}
	set := func(field, code, message string) { errs[field] = validation.NewError(code, message) }

	if err := sites.ValidName(site.GetString("name")); err != nil {
		set("name", "validation_invalid_site_name", capitalize(err.Error())+".")
	}
	// the stored site, not the record's original: a record saved before can
	// carry an original from before that save
	var stored *core.Record
	if !site.IsNew() {
		stored, _ = e.App.FindRecordById(schema.Sites, site.Id)
	}
	if stored != nil {
		if stored.GetString("name") != site.GetString("name") {
			set("name", "validation_site_name_fixed", "A site keeps its name: its files are kept under it.")
		}
		if stored.GetString("host") != site.GetString("host") {
			set("host", "validation_site_host_fixed", "A site stays on its server.")
		}
		if stored.GetString("type") != site.GetString("type") {
			set("type", "validation_site_type_fixed", "A site keeps its type: make a new site for the other kind.")
		}
	}
	if site.GetString("type") == schema.SitePHPApp && (site.GetBool("spa") || site.GetString("not_found") != "") {
		set("spa", "validation_php_spa", "A PHP application routes its own paths and answers its own 404s.")
	}
	// a site keeps its server, so this is checked where the server is chosen;
	// the panel's own saves of the site must not fail on it later
	if stored == nil || stored.GetString("host") != site.GetString("host") {
		if host, err := e.App.FindRecordById(schema.Hosts, site.GetString("host")); err == nil && !hasEdgeRole(host) {
			set("host", "validation_no_edge", "The server has no edge role: give it one first.")
		}
	}

	switch site.GetString("source") {
	case sites.SourceGit:
		if problem := checkRepoURL(site.GetString("repo_url")); problem != "" {
			set("repo_url", "validation_invalid_repo", problem)
		}
		if branch := site.GetString("branch"); !branchName.MatchString(branch) || strings.Contains(branch, "..") || strings.HasPrefix(branch, "-") {
			set("branch", "validation_invalid_branch", "The branch is a name such as main.")
		}
	case sites.SourceUpload, sites.SourceEmpty:
	}

	if site.GetString("generator") == sites.GeneratorCustom && (site.GetString("build_image") == "" || site.GetString("build_cmd") == "") {
		set("build_cmd", "validation_custom_build", "A custom build needs an image and a command.")
	}
	if image := site.GetString("build_image"); image != "" && !imageRef.MatchString(image) {
		set("build_image", "validation_invalid_image", "The build image is a reference such as node:22-alpine.")
	}
	if output := site.GetString("output_dir"); output != "" && (strings.HasPrefix(output, "/") || slices.Contains(strings.Split(output, "/"), "..")) {
		set("output_dir", "validation_invalid_output", "The output directory is a path inside the source, such as public.")
	}
	if keep := site.GetInt("keep_releases"); keep < 1 || keep > 50 {
		set("keep_releases", "validation_invalid_keep", "A site keeps 1 to 50 releases.")
	}
	if cpus := site.GetFloat("build_cpus"); cpus < 0 || cpus > 64 {
		set("build_cpus", "validation_invalid_cpus", "A build gets up to 64 CPUs.")
	}
	if memory := site.GetInt("build_memory_mb"); memory != 0 && (memory < 256 || memory > 65536) {
		set("build_memory_mb", "validation_invalid_memory", "A build gets 256 MiB to 64 GiB of memory.")
	}

	options, err := siteServing(site)
	if err == nil {
		err = edge.ValidSiteOptions(options)
	}
	if err != nil {
		set("headers", "validation_invalid_serving", capitalize(err.Error())+".")
	}

	// what is shown in place of the secrets, from what is saved now
	if text := site.GetString("basic_auth"); !secrets.IsSealed(text) {
		users := parseBasicAuth(text)
		if text == "" {
			users = nil
		} else if err := checkBasicAuthText(text); err != nil {
			set("basic_auth", "validation_invalid_basic_auth", capitalize(err.Error())+".")
		} else if !site.IsNew() {
			// a password needs HTTPS on every domain of the site
			if plain, _ := e.App.CountRecords(schema.Domains, dbx.HashExp{"site": site.Id, "tls": schema.TLSOff}); plain > 0 {
				set("basic_auth", "validation_invalid_basic_auth", "A password needs HTTPS: a domain of this site is served over plain HTTP.")
			}
		}
		names := []string{}
		for _, user := range users {
			names = append(names, user.Username)
		}
		site.Set("basic_auth_users", names)
	}
	if token := site.GetString("git_token"); !secrets.IsSealed(token) {
		site.Set("git_token_set", token != "")
	}

	if len(errs) > 0 {
		return errs
	}
	return e.Next()
}

// checkRepoURL says what is wrong with a Git repository's address.
func checkRepoURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "The repository is an HTTPS address, such as https://github.com/me/blog.git."
	}
	if parsed.User != nil {
		return "Put the access token in the token field, not in the address: the address is shown to everyone."
	}
	return ""
}

// siteServing is how the edge serves a site.
func siteServing(site *core.Record) (*edge.SiteOptions, error) {
	options := &edge.SiteOptions{
		Root:     sites.ServedRoot(site.GetString("name")),
		SPA:      site.GetBool("spa"),
		NotFound: site.GetString("not_found"),
	}
	if raw, _ := json.Marshal(site.Get("headers")); string(raw) != "null" && string(raw) != `""` {
		if err := json.Unmarshal(raw, &options.Headers); err != nil {
			return nil, errors.New("the headers are a list of path, name and value")
		}
	}
	if raw, _ := json.Marshal(site.Get("cache_rules")); string(raw) != "null" && string(raw) != `""` {
		if err := json.Unmarshal(raw, &options.Cache); err != nil {
			return nil, errors.New("the cache rules are a list of path and max_age")
		}
	}
	return options, nil
}

// servedDifferently says whether a change of a site changes its routes.
func servedDifferently(before, after *core.Record) bool {
	for _, field := range []string{"https_redirect", "hsts", "compress", "spa", "not_found", "basic_auth"} {
		if before.GetString(field) != after.GetString(field) {
			return true
		}
	}
	for _, field := range []string{"headers", "cache_rules"} {
		a, _ := json.Marshal(before.Get(field))
		b, _ := json.Marshal(after.Get(field))
		if string(a) != string(b) {
			return true
		}
	}
	return false
}

// siteRoute fills in a site domain's route from its site.
func (p *Panel) siteRoute(route *edge.Route, site *core.Record) error {
	options, err := siteServing(site)
	if err != nil {
		return err
	}
	users, err := p.siteBasicAuth(site)
	if err != nil {
		return err
	}
	if site.GetString("type") == schema.SitePHPApp {
		// a PHP site's scripts go to its PHP-FPM, from its web root (#50)
		settings, err := p.App.FindFirstRecordByData(schema.SitePHP, "site", site.Id)
		if err != nil {
			return fmt.Errorf("the PHP site %s has no settings", site.GetString("name"))
		}
		if docroot := settings.GetString("docroot"); docroot != "" {
			options.Root += "/" + docroot
		}
		options.FastCGI = sites.FastCGIUpstream(site.GetString("name"))
		options.SPA, options.NotFound = false, ""
	}
	route.Kind = edge.KindSite
	route.Site = options
	route.HSTS = site.GetBool("hsts")
	route.Compress = site.GetBool("compress")
	route.BasicAuth = users
	return nil
}

// siteBasicAuth opens the users of a site.
func (p *Panel) siteBasicAuth(site *core.Record) ([]edge.BasicAuthUser, error) {
	sealed := site.GetString("basic_auth")
	if sealed == "" {
		return nil, nil
	}
	plain, err := p.Box.OpenString(sealed)
	if err != nil {
		return nil, fmt.Errorf("the passwords of %s cannot be opened: %w", site.GetString("name"), err)
	}
	return parseBasicAuth(plain), nil
}

// deploymentRequest is what starts a deployment.
type deploymentRequest struct {
	Trigger string
	// Source is the site's own unless an upload is given.
	Upload    *filesystem.File
	UploadSHA string
	// Push is the commit a webhook reported.
	Push        sites.Push
	RequestedBy string
	Requester   string
	// Parent is the command of a flow the deployment is a step of, such as a
	// new site's provisioning.
	Parent string
	// Hold records the deployment without sending it: the flow sends it with
	// sendDeployment when its turn comes.
	Hold bool
	// Message describes a deployment that is not from a push or an upload.
	Message string
	// BeforeSend sends what must reach the server first, such as the PHP-FPM
	// of a new PHP site: commands of a site run in the order they were made.
	BeforeSend func(ctx context.Context) error
	// PHPApply changes a PHP site's PHP-FPM with the release, as for a new
	// PHP version, which is newer or older than PHPFrom (#50).
	PHPApply bool
	PHPFrom  string
}

// startDeployment records a deployment and sends it to the site's server.
func (p *Panel) startDeployment(ctx context.Context, site *core.Record, request deploymentRequest) (*core.Record, error) {
	lock := p.edgeLocks.of("site:" + site.Id)
	lock.Lock()
	defer lock.Unlock()

	source := site.GetString("source")
	artifact := ""
	if request.Upload != nil {
		source = sites.SourceUpload
	} else if source == sites.SourceUpload {
		// deploy again: the newest upload
		last, err := p.App.FindRecordsByFilter(schema.Deployments, "site={:s} && upload!=''", "-number", 1, 0, dbx.Params{"s": site.Id})
		if err != nil || len(last) == 0 {
			return nil, errNoUpload
		}
		artifact = last[0].Id
	}

	number := 1
	if last, err := p.App.FindRecordsByFilter(schema.Deployments, "site={:s}", "-number", 1, 0, dbx.Params{"s": site.Id}); err == nil && len(last) > 0 {
		number = last[0].GetInt("number") + 1
	}

	collection, err := p.App.FindCachedCollectionByNameOrId(schema.Deployments)
	if err != nil {
		return nil, err
	}
	deployment := core.NewRecord(collection)
	deployment.Set("site", site.Id)
	deployment.Set("number", number)
	deployment.Set("status", schema.DeploymentQueued)
	deployment.Set("trigger", request.Trigger)
	deployment.Set("source", source)
	deployment.Set("requested_by", request.Requester)
	if source == sites.SourceGit {
		deployment.Set("branch", site.GetString("branch"))
		deployment.Set("commit_sha", request.Push.Commit)
		deployment.Set("message", truncate(request.Push.Message, 1000))
	}
	if request.Upload != nil {
		deployment.Set("upload", request.Upload)
		deployment.Set("upload_sha256", request.UploadSHA)
		deployment.Set("message", truncate(request.Upload.OriginalName, 1000))
	}
	if request.Message != "" {
		deployment.Set("message", truncate(request.Message, 1000))
	}
	if site.GetString("type") == schema.SitePHPApp {
		// a rollback to it goes back to its PHP version
		if settings, err := p.phpSettings(p.App, site); err == nil {
			deployment.Set("php_version", settings.GetString("php_version"))
		}
	}
	if err := p.App.SaveWithContext(ctx, deployment); err != nil {
		return nil, err
	}
	if artifact == "" && request.Upload != nil {
		artifact = deployment.Id
	}
	if request.Hold {
		return deployment, nil
	}
	if request.BeforeSend != nil {
		if err := request.BeforeSend(ctx); err != nil {
			p.setFields(deployment, map[string]any{"status": schema.DeploymentFailed, "error": truncate(err.Error(), 4096), "finished_at": types.NowDateTime()})
			return deployment, err
		}
	}
	return deployment, p.sendDeploymentLocked(ctx, site, deployment, artifact, request)
}

// sendDeployment sends a deployment that was held, as a step of its flow.
func (p *Panel) sendDeployment(ctx context.Context, site, deployment *core.Record, request deploymentRequest) error {
	lock := p.edgeLocks.of("site:" + site.Id)
	lock.Lock()
	defer lock.Unlock()
	artifact := ""
	if deployment.GetString("upload") != "" {
		artifact = deployment.Id
	}
	return p.sendDeploymentLocked(ctx, site, deployment, artifact, request)
}

// sendDeploymentLocked sends a recorded deployment to the site's server, under
// the site's lock.
func (p *Panel) sendDeploymentLocked(ctx context.Context, site, deployment *core.Record, artifact string, request deploymentRequest) error {
	number := deployment.GetInt("number")
	source := deployment.GetString("source")
	memory := int64(site.GetInt("build_memory_mb")) << 20
	var phpDeploy *agentv1.SitePhpDeploy
	var shared []string
	var hook string
	if site.GetString("type") == schema.SitePHPApp {
		var err error
		if phpDeploy, shared, hook, err = p.phpDeploy(site, deployment); err == nil && request.PHPApply {
			var settings *core.Record
			if phpDeploy.Apply, settings, err = p.phpApplyPayload(site); err == nil {
				// a newer PHP runs the release served now; an older one the new release
				phpDeploy.ApplyFirst = php.Compare(settings.GetString("php_version"), request.PHPFrom) > 0
			}
		}
		if err != nil {
			p.setFields(deployment, map[string]any{"status": schema.DeploymentFailed, "error": truncate(err.Error(), 4096), "finished_at": types.NowDateTime()})
			return err
		}
	}
	command, err := p.Commands.Create(ctx, commands.Options{
		Host:        site.GetString("host"),
		Label:       fmt.Sprintf("%s #%d", site.GetString("name"), number),
		RequestedBy: request.RequestedBy,
		Requester:   request.Requester,
		Parent:      request.Parent,
		Correlation: request.Parent,
		Command: &agentv1.Command{Payload: &agentv1.Command_SiteDeploy{SiteDeploy: &agentv1.SiteDeploy{
			Site:          site.GetString("name"),
			Deployment:    int32(number), //nolint:gosec // a counter
			Source:        source,
			RepoUrl:       site.GetString("repo_url"),
			Branch:        site.GetString("branch"),
			ArtifactId:    artifact,
			Generator:     site.GetString("generator"),
			BuildImage:    site.GetString("build_image"),
			BuildCmd:      site.GetString("build_cmd"),
			OutputDir:     site.GetString("output_dir"),
			KeepReleases:  int32(site.GetInt("keep_releases")), //nolint:gosec // at most 50
			CpuLimit:      site.GetFloat("build_cpus"),
			MemLimitBytes: memory,
			Php:           phpDeploy,
			SharedPaths:   shared,
			ReleaseHook:   hook,
		}}},
	})
	if err != nil {
		p.setFields(deployment, map[string]any{"status": schema.DeploymentFailed, "error": truncate(err.Error(), 4096), "finished_at": types.NowDateTime()})
		return err
	}
	p.setFields(deployment, map[string]any{"command": command.Id})
	return nil
}

var errNoUpload = errors.New("this site is deployed from an upload: upload a ZIP first")

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}

// enrichSiteDeploy adds the site's Git token as the command is delivered, so
// it is never stored with the command.
func (p *Panel) enrichSiteDeploy(record *core.Record, command *agentv1.Command) error {
	payload := command.GetSiteDeploy()
	if payload.GetSource() != sites.SourceGit && payload.GetPhp() == nil {
		return nil
	}
	site, err := p.App.FindFirstRecordByFilter(schema.Sites, "host={:h} && name={:n}", dbx.Params{"h": record.GetString("host"), "n": payload.GetSite()})
	if err != nil {
		return errors.New("the site was removed")
	}
	// a PHP site's release hook gets the site's secrets (#50)
	if payload.GetPhp() != nil {
		settings, err := p.phpSettings(p.App, site)
		if err != nil {
			return err
		}
		secrets, err := p.phpSecrets(site, settings)
		if err != nil {
			return err
		}
		payload.Php.Secrets = secrets
		if payload.Php.Apply != nil {
			payload.Php.Apply.Secrets = secrets
		}
	}
	if payload.GetSource() != sites.SourceGit {
		return nil
	}
	if sealed := site.GetString("git_token"); sealed != "" {
		token, err := p.Box.OpenString(sealed)
		if err != nil {
			return fmt.Errorf("the site's Git token cannot be opened: %w", err)
		}
		payload.GitToken = token
	}
	return nil
}

// deploymentOf is the deployment of a site.deploy command. A command that
// fails at once can finish before its deployment records it, so the site and
// the number in its payload find it too.
func (p *Panel) deploymentOf(command *core.Record) (*core.Record, error) {
	if deployment, err := p.App.FindFirstRecordByData(schema.Deployments, "command", command.Id); err == nil {
		return deployment, nil
	}
	var payload struct {
		Site       string `json:"site"`
		Deployment any    `json:"deployment"`
	}
	raw, _ := json.Marshal(command.Get("payload"))
	_ = json.Unmarshal(raw, &payload)
	number, _ := strconv.Atoi(fmt.Sprint(payload.Deployment))
	site, err := p.App.FindFirstRecordByFilter(schema.Sites, "host={:h} && name={:n}", dbx.Params{"h": command.GetString("host"), "n": payload.Site})
	if err != nil {
		return nil, err
	}
	deployment, err := p.App.FindFirstRecordByFilter(schema.Deployments, "site={:s} && number={:n}", dbx.Params{"s": site.Id, "n": number})
	if err != nil {
		return nil, err
	}
	// the command is this one's, whichever saved first
	if deployment.GetString("command") == "" {
		deployment.Set("command", command.Id)
	}
	return deployment, nil
}

func (p *Panel) deploymentStarted(command *core.Record) {
	deployment, err := p.deploymentOf(command)
	if err != nil || deployment.GetString("status") != schema.DeploymentQueued {
		return
	}
	p.setFields(deployment, map[string]any{"status": schema.DeploymentBuilding, "started_at": types.NowDateTime()})
}

// siteDeployResult is what the agent reports of a deployment.
type siteDeployResult struct {
	Release   int     `json:"release"`
	SizeBytes float64 `json:"size_bytes"`
	Files     float64 `json:"files"`
	Releases  []int   `json:"releases"`
	Commit    string  `json:"commit"`
	Message   string  `json:"message"`
	Generator string  `json:"generator"`
	Image     string  `json:"image"`
	Command   string  `json:"command"`
	BuildMs   float64 `json:"build_ms"`
}

func (p *Panel) siteDeployFinished(command *core.Record) {
	deployment, err := p.deploymentOf(command)
	if err != nil {
		return
	}
	site, err := p.App.FindRecordById(schema.Sites, deployment.GetString("site"))
	if err != nil {
		return
	}
	fields := map[string]any{"finished_at": types.NowDateTime()}
	if deployment.GetString("started_at") == "" {
		fields["started_at"] = types.NowDateTime()
	}
	label := fmt.Sprintf("%s #%d", site.GetString("name"), deployment.GetInt("number"))

	// a new PHP version's PHP-FPM may have been applied, with the release or
	// before a switch that failed (#50)
	if site.GetString("type") == schema.SitePHPApp {
		p.phpDeployApplied(site, command)
		if command.GetString("status") != schema.CommandSucceeded {
			p.keepLivePHPVersion(site, deployment)
		}
	}
	if command.GetString("status") != schema.CommandSucceeded {
		fields["status"] = schema.DeploymentFailed
		fields["error"] = truncate(command.GetString("error"), 4096)
		p.setFields(deployment, fields)
		events.Log(p.App, site.GetString("host"), "site.deploy_failed", schema.SeverityWarning,
			fmt.Sprintf("Deployment %s failed; the site still serves the release it served before: %s", label, command.GetString("error")),
			map[string]any{"site": site.Id, "deployment": deployment.Id})
		return
	}

	var result siteDeployResult
	raw, _ := json.Marshal(command.Get("result"))
	_ = json.Unmarshal(raw, &result)
	fields["status"] = schema.DeploymentSucceeded
	fields["on_server"] = true
	// a command that finished while its agent was away is settled without
	// its result: what it built is not known, and nothing is guessed
	if result.Release != 0 {
		fields["size_bytes"] = int64(result.SizeBytes)
		fields["files"] = int64(result.Files)
		fields["build_ms"] = int64(result.BuildMs)
		fields["generator"] = result.Generator
		fields["image"] = result.Image
		fields["build_cmd"] = truncate(result.Command, 4096)
		if result.Commit != "" {
			fields["commit_sha"] = result.Commit
			fields["message"] = truncate(result.Message, 1000)
		}
	}
	// the deployment is live and the site's active one at once, so nothing
	// sees one without the other
	if err := p.setFieldsTogether(
		fieldChange{deployment, fields},
		fieldChange{site, map[string]any{"active_deployment": deployment.Id}},
	); err != nil {
		p.Log.Error("failed to record a live deployment", "site", site.GetString("name"), "deployment", deployment.Id, "error", err)
	}
	if len(result.Releases) > 0 {
		p.markReleasesOnServer(site, result.Releases)
	}
	events.Log(p.App, site.GetString("host"), "site.deployed", schema.SeverityInfo,
		fmt.Sprintf("Deployment %s is live.", label), map[string]any{"site": site.Id, "deployment": deployment.Id})
}

// markReleasesOnServer records which releases the server still has, after a
// deployment pruned the old ones. The upload of a release that is gone goes
// too, unless it is the newest, which deploying again uses.
func (p *Panel) markReleasesOnServer(site *core.Record, releases []int) {
	deployments, err := p.App.FindRecordsByFilter(schema.Deployments, "site={:s} && (on_server=true || upload!='')", "-number", 0, 0, dbx.Params{"s": site.Id})
	if err != nil {
		return
	}
	newestUpload := ""
	for _, deployment := range deployments {
		if newestUpload == "" && deployment.GetString("upload") != "" {
			newestUpload = deployment.Id
		}
	}
	for _, deployment := range deployments {
		if slices.Contains(releases, deployment.GetInt("number")) {
			continue
		}
		// one a new site's setup holds for its deploy step has not run yet
		if deployment.GetString("command") == "" && deployment.GetString("status") == schema.DeploymentQueued {
			continue
		}
		fields := map[string]any{}
		if deployment.GetBool("on_server") {
			fields["on_server"] = false
		}
		if deployment.GetString("upload") != "" && deployment.Id != newestUpload {
			fields["upload"] = nil
		}
		if len(fields) > 0 {
			p.setFields(deployment, fields)
		}
	}
}

func (p *Panel) siteRollbackFinished(command *core.Record) {
	if command.GetString("status") != schema.CommandSucceeded {
		return
	}
	var payload struct {
		Site       string `json:"site"`
		Deployment any    `json:"deployment"`
	}
	raw, _ := json.Marshal(command.Get("payload"))
	_ = json.Unmarshal(raw, &payload)
	number, _ := strconv.Atoi(fmt.Sprint(payload.Deployment))
	site, err := p.App.FindFirstRecordByFilter(schema.Sites, "host={:h} && name={:n}", dbx.Params{"h": command.GetString("host"), "n": payload.Site})
	if err != nil {
		return
	}
	deployment, err := p.App.FindFirstRecordByFilter(schema.Deployments, "site={:s} && number={:n}", dbx.Params{"s": site.Id, "n": number})
	if err != nil {
		return
	}
	p.setFields(site, map[string]any{"active_deployment": deployment.Id})
	events.Log(p.App, site.GetString("host"), "site.rolled_back", schema.SeverityInfo,
		fmt.Sprintf("%s is back on deployment #%d.", site.GetString("name"), number), map[string]any{"site": site.Id, "deployment": deployment.Id})
}

// removeSiteFiles removes a removed site's releases from its server.
func (p *Panel) removeSiteFiles(site *core.Record) {
	if _, err := p.App.FindRecordById(schema.Hosts, site.GetString("host")); err != nil {
		return
	}
	_, err := p.Commands.Create(context.Background(), commands.Options{
		Host:      site.GetString("host"),
		Label:     site.GetString("name"),
		Requester: "panel",
		Command:   &agentv1.Command{Payload: &agentv1.Command_SiteRemove{SiteRemove: &agentv1.SiteRemove{Site: site.GetString("name")}}},
	})
	if err != nil {
		p.Log.Error("failed to remove a site's files", "site", site.GetString("name"), "error", err)
	}
}

// fetchArtifact opens a site's upload for the agent of the server that builds
// it.
func (p *Panel) fetchArtifact(hostID, id string) (*gateway.Artifact, error) {
	deployment, err := p.App.FindRecordById(schema.Deployments, id)
	if err != nil || deployment.GetString("upload") == "" {
		return nil, gateway.ErrNoArtifact
	}
	site, err := p.App.FindRecordById(schema.Sites, deployment.GetString("site"))
	if err != nil || site.GetString("host") != hostID {
		return nil, gateway.ErrNoArtifact
	}
	files, err := p.App.NewFilesystem()
	if err != nil {
		return nil, err
	}
	reader, err := files.GetReader(deployment.BaseFilesPath() + "/" + deployment.GetString("upload"))
	if err != nil {
		_ = files.Close()
		return nil, err
	}
	size := reader.Size()
	if size < 0 {
		size = 0
	}
	return &gateway.Artifact{
		Reader: &closeBoth{ReadSeekCloser: reader, also: files},
		Size:   uint64(size),
		SHA256: deployment.GetString("upload_sha256"),
	}, nil
}

// closeBoth closes a file and the filesystem it came from.
type closeBoth struct {
	io.ReadSeekCloser
	also io.Closer
}

func (c *closeBoth) Close() error {
	err := c.ReadSeekCloser.Close()
	if closeErr := c.also.Close(); err == nil {
		err = closeErr
	}
	return err
}

// onRequests stores the requests each hostname of an edge had.
func (p *Panel) onRequests(hostID string, byHost map[string]uint64) {
	if p.Metrics == nil {
		return
	}
	if err := p.Metrics.AddRequests(hostID, byHost); err != nil {
		p.Log.Error("failed to store request counts", "host", hostID, "error", err)
	}
}

func (p *Panel) findSite(e *core.RequestEvent) (*core.Record, error) {
	site, err := e.App.FindRecordById(schema.Sites, e.Request.PathValue("id"))
	if err != nil {
		return nil, e.NotFoundError("The site does not exist.", err)
	}
	return site, nil
}

func (p *Panel) handleSiteDeploy(e *core.RequestEvent) error {
	site, err := p.findSite(e)
	if err != nil {
		return err
	}
	id, label := requester(e)
	deployment, err := p.startDeployment(e.Request.Context(), site, deploymentRequest{Trigger: schema.TriggerManual, RequestedBy: id, Requester: label})
	if errors.Is(err, errNoUpload) {
		return e.BadRequestError("This site is deployed from an upload: upload a ZIP first.", nil)
	}
	if err != nil {
		return e.InternalServerError("Failed to start the deployment.", err)
	}
	return e.JSON(http.StatusOK, deployment)
}

func (p *Panel) handleSiteUpload(e *core.RequestEvent) error {
	site, err := p.findSite(e)
	if err != nil {
		return err
	}
	files, err := e.FindUploadedFiles("file")
	if err != nil || len(files) != 1 {
		return e.BadRequestError("Send one ZIP file as file.", err)
	}
	file := files[0]
	if file.Size > maxSiteUpload {
		return e.BadRequestError(fmt.Sprintf("The upload is larger than %d MiB.", maxSiteUpload>>20), nil)
	}
	digest, err := zipDigest(file)
	if err != nil {
		return e.BadRequestError("The upload is not a ZIP file.", err)
	}
	id, label := requester(e)
	deployment, err := p.startDeployment(e.Request.Context(), site, deploymentRequest{
		Trigger: schema.TriggerUpload, Upload: file, UploadSHA: digest, RequestedBy: id, Requester: label,
	})
	if err != nil {
		return e.InternalServerError("Failed to start the deployment.", err)
	}
	return e.JSON(http.StatusOK, deployment)
}

// zipDigest checks that an upload is a ZIP and returns its SHA-256.
func zipDigest(file *filesystem.File) (string, error) {
	reader, err := file.Reader.Open()
	if err != nil {
		return "", err
	}
	defer reader.Close()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(reader, magic); err != nil || (string(magic) != "PK\x03\x04" && string(magic) != "PK\x05\x06") {
		return "", errors.New("no ZIP header")
	}
	hash := sha256.New()
	hash.Write(magic)
	if _, err := io.Copy(hash, reader); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (p *Panel) handleSiteRollback(e *core.RequestEvent) error {
	site, err := p.findSite(e)
	if err != nil {
		return err
	}
	var body struct {
		Deployment string `json:"deployment"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Name the deployment to go back to.", err)
	}
	deployment, err := e.App.FindRecordById(schema.Deployments, body.Deployment)
	if err != nil || deployment.GetString("site") != site.Id {
		return e.NotFoundError("The deployment is not one of this site's.", err)
	}
	if deployment.GetString("status") != schema.DeploymentSucceeded || !deployment.GetBool("on_server") {
		return e.BadRequestError("Only a release that is still on the server can come back: deploy it again instead.", nil)
	}
	if site.GetString("active_deployment") == deployment.Id {
		return e.BadRequestError("The site serves this release already.", nil)
	}
	id, label := requester(e)
	isPHP := site.GetString("type") == schema.SitePHPApp
	if isPHP {
		// a release built for another PHP version goes back to it: PHP-FPM
		// changes first, as the site's commands run in order (#50)
		settings, err := p.phpSettings(e.App, site)
		if err != nil {
			return e.InternalServerError("", err)
		}
		if version := deployment.GetString("php_version"); version != "" && version != settings.GetString("php_version") {
			current := settings.GetString("php_version")
			settings.Set("php_version", version)
			if err := e.App.SaveWithContext(audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e)), settings); err != nil {
				return e.InternalServerError("", err)
			}
			// a newer PHP-FPM first, as it runs the release served now; an
			// older one after the switch, as the current one runs the release
			// it goes back to
			ctx := context.WithoutCancel(e.Request.Context())
			applyPHP := func() error {
				_, err := p.applyPHP(ctx, site, commands.Options{RequestedBy: id, Requester: label})
				return err
			}
			if php.Compare(version, current) > 0 {
				if err := applyPHP(); err != nil {
					return e.InternalServerError("Failed to go back to PHP "+version+".", err)
				}
			} else {
				defer func() {
					if err := applyPHP(); err != nil {
						p.Log.Error("failed to go back to an older PHP", "site", site.GetString("name"), "version", version, "error", err)
					}
				}()
			}
		}
	}
	command, err := p.Commands.Create(e.Request.Context(), commands.Options{
		Host:        site.GetString("host"),
		Label:       fmt.Sprintf("%s #%d", site.GetString("name"), deployment.GetInt("number")),
		RequestedBy: id,
		Requester:   label,
		Command: &agentv1.Command{Payload: &agentv1.Command_SiteRollback{SiteRollback: &agentv1.SiteRollback{
			Site: site.GetString("name"), Deployment: int32(deployment.GetInt("number")), //nolint:gosec // a counter
			Php: isPHP,
		}}},
	})
	if err != nil {
		return e.InternalServerError("Failed to go back.", err)
	}
	return e.JSON(http.StatusOK, map[string]any{"command": command.Id})
}

// handleSiteCredentials sets the secrets of a site, which the records API
// neither shows nor takes: the Git token and the passwords. A field left out
// stays as it is; an empty one removes it.
func (p *Panel) handleSiteCredentials(e *core.RequestEvent) error {
	site, err := p.findSite(e)
	if err != nil {
		return err
	}
	var body struct {
		GitToken  *string `json:"git_token"`
		BasicAuth *string `json:"basic_auth"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("The body is git_token and basic_auth.", err)
	}
	ctx := audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e))
	err = p.update(ctx, site, func(fresh *core.Record) error {
		if body.GitToken != nil {
			fresh.Set("git_token", strings.TrimSpace(*body.GitToken))
		}
		if body.BasicAuth != nil {
			fresh.Set("basic_auth", strings.TrimSpace(*body.BasicAuth))
		}
		return nil
	})
	if err != nil {
		var errs validation.Errors
		if errors.As(err, &errs) {
			return e.BadRequestError(saveErrorText(err), errs)
		}
		return e.InternalServerError("Failed to save the credentials.", err)
	}
	return e.JSON(http.StatusOK, site)
}

func (p *Panel) handleSiteWebhookInfo(e *core.RequestEvent) error {
	site, err := p.findSite(e)
	if err != nil {
		return err
	}
	secret, err := p.Box.OpenString(site.GetString("webhook_secret"))
	if err != nil {
		return e.InternalServerError("The webhook's secret cannot be opened.", err)
	}
	return e.JSON(http.StatusOK, map[string]string{"url": p.webhookURL(e, site), "secret": secret})
}

func (p *Panel) handleSiteWebhookRotate(e *core.RequestEvent) error {
	site, err := p.findSite(e)
	if err != nil {
		return err
	}
	secret := randomSecret()
	ctx := audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e))
	if err := p.update(ctx, site, func(fresh *core.Record) error {
		fresh.Set("webhook_secret", secret)
		return nil
	}); err != nil {
		return e.InternalServerError("Failed to make a new secret.", err)
	}
	return e.JSON(http.StatusOK, map[string]string{"url": p.webhookURL(e, site), "secret": secret})
}

func (p *Panel) webhookURL(e *core.RequestEvent, site *core.Record) string {
	base := strings.TrimSuffix(p.Config.PublicURL, "/")
	if base == "" {
		scheme := "http"
		if e.Request.TLS != nil {
			scheme = "https"
		}
		base = scheme + "://" + e.Request.Host
	}
	return base + webhookPath + site.Id
}

// handleSiteWebhook takes a push from a Git server. It needs no account: the
// site's secret signs it.
func (p *Panel) handleSiteWebhook(e *core.RequestEvent) error {
	site, err := e.App.FindRecordById(schema.Sites, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("No such site.", nil)
	}
	body, err := io.ReadAll(io.LimitReader(e.Request.Body, maxWebhookBody))
	if err != nil {
		return e.BadRequestError("The body could not be read.", err)
	}
	secret, err := p.Box.OpenString(site.GetString("webhook_secret"))
	if err != nil {
		return e.InternalServerError("The webhook's secret cannot be opened.", err)
	}
	push, err := sites.ParsePush(e.Request.Header, body, secret)
	switch {
	case errors.Is(err, sites.ErrSignature):
		return e.UnauthorizedError("The signature does not match this site's secret.", nil)
	case errors.Is(err, sites.ErrNotAPush):
		return e.JSON(http.StatusAccepted, map[string]string{"ignored": "not a push to a branch"})
	case err != nil:
		return e.BadRequestError(err.Error(), nil)
	}
	if site.GetString("source") != sites.SourceGit {
		return e.JSON(http.StatusAccepted, map[string]string{"ignored": "the site is not deployed from Git"})
	}
	if push.Branch != site.GetString("branch") {
		return e.JSON(http.StatusAccepted, map[string]string{"ignored": "a push to " + push.Branch + ", not " + site.GetString("branch")})
	}
	deployment, err := p.startDeployment(e.Request.Context(), site, deploymentRequest{Trigger: schema.TriggerPush, Push: push, Requester: "webhook"})
	if err != nil {
		return e.InternalServerError("Failed to start the deployment.", err)
	}
	return e.JSON(http.StatusCreated, map[string]any{"deployment": deployment.Id, "number": deployment.GetInt("number")})
}

// handleSiteRequests are the requests of a site's domains in each of the last
// 24 hours.
func (p *Panel) handleSiteRequests(e *core.RequestEvent) error {
	site, err := p.findSite(e)
	if err != nil {
		return err
	}
	domains, err := e.App.FindRecordsByFilter(schema.Domains, "site={:s}", "", 0, 0, dbx.Params{"s": site.Id})
	if err != nil {
		return e.InternalServerError("Failed to read the site's domains.", err)
	}
	names := []string{}
	for _, domain := range domains {
		names = append(names, domain.GetString("hostname"))
	}
	// names under a wildcard of the site that the edge serves for something else
	others, err := e.App.FindRecordsByFilter(schema.Domains, "host={:h} && site!={:s}", "", 0, 0,
		dbx.Params{"h": site.GetString("host"), "s": site.Id})
	if err != nil {
		return e.InternalServerError("Failed to read the server's domains.", err)
	}
	exact := []string{}
	for _, domain := range others {
		exact = append(exact, domain.GetString("hostname"))
	}
	counts := make([]int64, requestHours)
	if p.Metrics != nil {
		if counts, err = p.Metrics.Requests(site.GetString("host"), names, exact, requestHours); err != nil {
			return e.InternalServerError("Failed to read the requests.", err)
		}
	}
	var total int64
	for _, count := range counts {
		total += count
	}
	from := time.Now().UTC().Truncate(time.Hour).Add(-(requestHours - 1) * time.Hour)
	return e.JSON(http.StatusOK, map[string]any{"from": from.Format(time.RFC3339), "hours": counts, "total": total})
}
