package panel

import (
	"archive/zip"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/phpapps"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/sites"
)

// PHP application templates (#52): internal/phpapps says what each one is;
// this is how the panel applies one. A new site made with a template gets its
// defaults in its PHP settings (web root, shared paths, release hook, php.ini
// lines, environment, its own new secrets, health check, what backups leave
// out) and, once its first release is live, its scheduled tasks. Each
// deployment carries what the template asks of a release: its files, links,
// caches to empty and environment file. The template's worker is applied with
// PHP-FPM when the site's settings ask for it; its update is a deployment of
// a copy of the live release; and the panel asks the site after each release
// whether it still answers.

// freshInstall is what a fresh install is made with, as the wizard asks:
// WordPress's title, administrator and language.
type freshInstall struct {
	Title      string `json:"title"`
	AdminUser  string `json:"admin_user"`
	AdminEmail string `json:"admin_email"`
	Locale     string `json:"locale"`
	// AdminPassword comes with the request only: it is a secret of the site
	// until the install is done, never part of the flow's record.
	AdminPassword string `json:"admin_password,omitempty"`
}

// installSecret is the site's own secret that holds a fresh install's
// administrator password until the install is done.
const installSecret = "install-admin-password"

// The environment a template's release hook installs a fresh site with. A
// site's own variables cannot start with POCKETCLOUD_, so none is overridden.
const (
	envInstallTitle    = "POCKETCLOUD_INSTALL_TITLE"
	envInstallUser     = "POCKETCLOUD_INSTALL_ADMIN_USER"
	envInstallEmail    = "POCKETCLOUD_INSTALL_ADMIN_EMAIL"
	envInstallPassword = "POCKETCLOUD_INSTALL_ADMIN_PASSWORD"
	envInstallLocale   = "POCKETCLOUD_INSTALL_LOCALE"
	// envMode says releases or classic, as WordPress's wp-config.php reads it
	envMode = "POCKETCLOUD_MODE"
)

var (
	installUser = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,60}$`)
	// a plugin or theme an update adds: a slug of wordpress.org, or the
	// address of a ZIP
	addonSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)
	addonURL  = regexp.MustCompile(`^https?://[^\s'"\\]+$`)
)

// checkInstall checks a fresh install's details.
func checkInstall(install *freshInstall) error {
	install.Title = strings.TrimSpace(install.Title)
	install.AdminUser = strings.TrimSpace(install.AdminUser)
	install.AdminEmail = strings.TrimSpace(install.AdminEmail)
	if install.Locale == "" {
		install.Locale = "en_US"
	}
	if install.Title == "" || utf8.RuneCountInString(install.Title) > 200 || strings.ContainsAny(install.Title, "\n\r") {
		return errors.New("the site's title is 1 to 200 characters on one line")
	}
	if !installUser.MatchString(install.AdminUser) {
		return errors.New("the administrator's user name is up to 60 letters, digits and . _ @ -")
	}
	if address, err := mail.ParseAddress(install.AdminEmail); err != nil || address.Address != install.AdminEmail || address.Name != "" {
		return errors.New("the administrator's e-mail is an address such as admin@example.com")
	}
	if n := utf8.RuneCountInString(install.AdminPassword); n < 12 || n > 200 {
		return errors.New("the administrator's password has 12 to 200 characters")
	}
	return phpapps.ValidLocale(install.Locale)
}

// templateOf is the template of a PHP site's settings; custom has none.
func templateOf(settings *core.Record) (phpapps.Template, bool) {
	return phpapps.Get(settings.GetString("template"))
}

// installOf is the fresh install a site's settings still wait for, if any.
func installOf(settings *core.Record) *freshInstall {
	var install freshInstall
	raw, _ := json.Marshal(settings.Get("install"))
	if json.Unmarshal(raw, &install) != nil || install.AdminUser == "" {
		return nil
	}
	return &install
}

// ownSecrets are a PHP site's own secrets, opened.
func (p *Panel) ownSecrets(settings *core.Record) (map[string]string, error) {
	own := map[string]string{}
	sealed := settings.GetString("secrets")
	if sealed == "" {
		return own, nil
	}
	plain, err := p.Box.OpenString(sealed)
	if err != nil {
		return nil, fmt.Errorf("the site's secrets cannot be opened: %w", err)
	}
	if err := json.Unmarshal([]byte(plain), &own); err != nil {
		return nil, err
	}
	return own, nil
}

// setOwnSecrets seals a PHP site's own secrets into its settings, with their
// names, which are shown.
func (p *Panel) setOwnSecrets(settings *core.Record, own map[string]string) error {
	raw, _ := json.Marshal(own)
	names := make([]string, 0, len(own))
	for name := range own {
		names = append(names, name)
	}
	slices.Sort(names)
	sealed, err := p.Box.SealString(string(raw))
	if err != nil {
		return err
	}
	settings.Set("secrets", sealed)
	settings.Set("secret_names", names)
	return nil
}

// applyTemplate puts a template's defaults into a new site's settings, which
// are saved after: a site made with a template works on its first release.
func (p *Panel) applyTemplate(settings *core.Record, request *provisionPHP, hostname string) error {
	t, ok := phpapps.Get(request.Template)
	if !ok {
		return nil
	}
	lines, err := t.EnvLines(phpapps.Vars{URL: "https://" + hostname, Hostname: hostname}, request.DatabaseServer != "")
	if err != nil {
		return err
	}
	settings.Set("env", strings.Join(lines, "\n"))
	settings.Set("php_ini", t.IniLines())
	settings.Set("shared_paths", t.SharedPaths())
	settings.Set("release_hook", strings.TrimSpace(t.ReleaseHook))
	settings.Set("health_path", t.Health)
	settings.Set("backup_exclude", t.NoBackup())
	settings.Set("queue_worker", request.QueueWorker && t.Worker != nil)
	own, err := t.NewSecrets()
	if err != nil {
		return err
	}
	if request.Install != nil {
		own[installSecret] = request.Install.AdminPassword
		install := *request.Install
		install.AdminPassword = ""
		settings.Set("install", install)
	}
	return p.setOwnSecrets(settings, own)
}

// addTemplateTasks gives a site its template's scheduled tasks, once its first
// release is live: before, there is nothing for them to run in.
func (p *Panel) addTemplateTasks(site, settings *core.Record) {
	t, ok := templateOf(settings)
	if !ok || len(t.Tasks) == 0 {
		return
	}
	if count, err := p.App.CountRecords(schema.ScheduledTasks, dbx.HashExp{"site": site.Id}); err != nil || count > 0 {
		return
	}
	collection, err := p.App.FindCachedCollectionByNameOrId(schema.ScheduledTasks)
	if err != nil {
		return
	}
	ctx := audit.WithActor(context.Background(), audit.Actor{Source: audit.SourcePanel, Label: "template " + t.Name})
	for _, task := range t.Tasks {
		record := core.NewRecord(collection)
		record.Set("target_kind", schema.TaskTargetSite)
		record.Set("site", site.Id)
		record.Set("schedule", task.Schedule)
		record.Set("command", task.Command)
		record.Set("timeout_s", task.TimeoutS)
		record.Set("enabled", true)
		if err := p.App.SaveWithContext(ctx, record); err != nil {
			p.Log.Error("failed to add a template's task", "site", site.GetString("name"), "command", task.Command, "error", err)
		}
	}
}

// templateDeploy adds what a site's template asks of a release, the release a
// copy of which it builds, if any, and a fresh install's details.
func templateDeploy(deploy *agentv1.SitePhpDeploy, settings, deployment *core.Record) {
	if deploy.Env == nil {
		deploy.Env = map[string]string{}
	}
	deploy.Env[envMode] = modeOf(settings)
	if t, ok := templateOf(settings); ok {
		deploy.Files = t.FileContents()
		deploy.Links = t.LinkSpecs()
		deploy.Clear = t.Cleared()
		deploy.EnvFile = t.EnvFile
	}
	if deployment != nil {
		deploy.FromRelease = int32(deployment.GetInt("base_release")) //nolint:gosec // a release number
	}
	if install := installOf(settings); install != nil {
		deploy.Env[envInstallTitle] = install.Title
		deploy.Env[envInstallUser] = install.AdminUser
		deploy.Env[envInstallEmail] = install.AdminEmail
		deploy.Env[envInstallLocale] = install.Locale
	}
}

func modeOf(settings *core.Record) string {
	if settings.GetString("mode") == schema.PHPClassic {
		return schema.PHPClassic
	}
	return schema.PHPReleases
}

// enrichInstall adds a fresh install's administrator password to the release
// hook as the deployment is delivered, so it is never stored with it.
func (p *Panel) enrichInstall(deploy *agentv1.SitePhpDeploy, settings *core.Record) error {
	if installOf(settings) == nil {
		return nil
	}
	own, err := p.ownSecrets(settings)
	if err != nil {
		return err
	}
	if password, ok := own[installSecret]; ok {
		if deploy.Secrets == nil {
			deploy.Secrets = map[string]string{}
		}
		deploy.Secrets[envInstallPassword] = password
	}
	return nil
}

// installDone forgets a fresh install's details once a release of the site
// went live: the release hook installed it, and the password is no secret
// of the site's to keep.
func (p *Panel) installDone(site *core.Record) {
	settings, err := p.phpSettings(p.App, site)
	if err != nil || installOf(settings) == nil {
		return
	}
	own, err := p.ownSecrets(settings)
	if err != nil {
		p.Log.Error("failed to forget a fresh install's password", "site", site.GetString("name"), "error", err)
		return
	}
	delete(own, installSecret)
	if err := p.setOwnSecrets(settings, own); err != nil {
		p.Log.Error("failed to forget a fresh install's password", "site", site.GetString("name"), "error", err)
		return
	}
	settings.Set("install", nil)
	ctx := audit.WithActor(context.Background(), audit.Actor{Source: audit.SourcePanel, Label: "fresh install"})
	if err := p.App.SaveWithContext(ctx, settings); err != nil {
		p.Log.Error("failed to forget a fresh install's password", "site", site.GetString("name"), "error", err)
	}
}

// templateDatabaseEnv is what a site's template adds to its environment for a
// database connected to it, as Laravel's DB_DATABASE.
func templateDatabaseEnv(settings *core.Record, connection databases.Connection) []string {
	t, ok := templateOf(settings)
	if !ok {
		return nil
	}
	lines, err := t.DatabaseEnvLines(phpapps.DatabaseVars{
		Engine: connection.Engine.Name, Host: connection.Server, Port: connection.Engine.Port,
		Database: connection.Database, User: connection.User,
	})
	if err != nil {
		return nil
	}
	return lines
}

// workerCommand is what a site's worker runs, if its settings ask for one.
func workerCommand(settings *core.Record) string {
	t, ok := templateOf(settings)
	if !ok || t.Worker == nil || !settings.GetBool("queue_worker") {
		return ""
	}
	return t.Worker.Command
}

// redeploysOnEnv says whether a change of the site's environment deploys its
// live release again, as Laravel caches its environment at the release: the
// copy of the live release runs the release hook again with the new one.
func (p *Panel) redeploysOnEnv(site, settings *core.Record) (*core.Record, bool) {
	t, ok := templateOf(settings)
	if !ok || !t.RedeployOnEnv || settings.GetString("mode") == schema.PHPClassic {
		return nil, false
	}
	live, err := p.App.FindRecordById(schema.Deployments, site.GetString("active_deployment"))
	if err != nil {
		return nil, false
	}
	return live, true
}

// redeployForEnv deploys a copy of the live release with a changed
// environment, and its PHP-FPM with it.
func (p *Panel) redeployForEnv(ctx context.Context, site, settings, live *core.Record, options commands.Options) (*core.Record, error) {
	return p.startDeployment(ctx, site, deploymentRequest{
		Trigger: schema.TriggerManual, RequestedBy: options.RequestedBy, Requester: options.Requester,
		// the page says what it is, a copy of the live release, in its language
		Again: live,
		Base:     live.GetInt("number"),
		PHPApply: true,
		PHPFrom:  settings.GetString("php_version"),
	})
}

// handlePHPUpdate updates a PHP application, as WordPress with its plugins and
// themes, in a new release: a copy of the live one, with the template's
// update run in it before its release hook. A broken update is one rollback
// away. Plugins and themes to add come with it, as slugs or addresses of
// ZIPs.
func (p *Panel) handlePHPUpdate(e *core.RequestEvent) error {
	site, err := p.findSite(e)
	if err != nil {
		return err
	}
	if site.GetString("type") != schema.SitePHPApp {
		return e.BadRequestError("This is not a PHP site.", nil)
	}
	var body struct {
		Plugins []string `json:"plugins"`
		Themes  []string `json:"themes"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}
	settings, err := p.phpSettings(e.App, site)
	if err != nil {
		return e.InternalServerError("", err)
	}
	t, ok := templateOf(settings)
	if !ok || t.Update == nil {
		return e.BadRequestError("This site's application has no update of PocketCloud's.", nil)
	}
	if settings.GetString("mode") == schema.PHPClassic {
		return e.BadRequestError(fmt.Sprintf("In classic mode %s updates itself.", t.Title), nil)
	}
	live, err := e.App.FindRecordById(schema.Deployments, site.GetString("active_deployment"))
	if err != nil {
		return e.BadRequestError("The site has no live release to update.", nil)
	}
	hook, err := updateHook(t, body.Plugins, body.Themes)
	if err != nil {
		return e.BadRequestError(capitalize(err.Error())+".", nil)
	}
	id, label := requester(e)
	ctx := audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e))
	deployment, err := p.startDeployment(ctx, site, deploymentRequest{
		Trigger: schema.TriggerUpdate, RequestedBy: id, Requester: label,
		Again:   live,
		Base:    live.GetInt("number"),
		PreHook: hook,
	})
	if err != nil {
		return e.BadRequestError(capitalize(err.Error())+".", nil)
	}
	return e.JSON(http.StatusOK, map[string]any{"deployment": deployment.Id})
}

// updateHook is what an update runs before the release hook: the template's
// update, then the plugins and themes it adds.
func updateHook(t phpapps.Template, plugins, themes []string) (string, error) {
	hook := strings.TrimSpace(t.Update.Command)
	for _, add := range []struct {
		names   []string
		command string
		kind    string
	}{{plugins, t.Update.AddPlugin, "plugin"}, {themes, t.Update.AddTheme, "theme"}} {
		if len(add.names) == 0 {
			continue
		}
		if add.command == "" {
			return "", fmt.Errorf("%s adds no %ss", t.Title, add.kind)
		}
		if len(add.names) > 20 {
			return "", fmt.Errorf("at most 20 %ss at once", add.kind)
		}
		quoted := make([]string, 0, len(add.names))
		for _, name := range add.names {
			name = strings.TrimSpace(name)
			if !addonSlug.MatchString(name) && !addonURL.MatchString(name) {
				return "", fmt.Errorf("%q is not a %s: its slug, such as akismet, or the address of its ZIP", name, add.kind)
			}
			quoted = append(quoted, "'"+name+"'")
		}
		hook += "\n" + add.command + " " + strings.Join(quoted, " ")
	}
	return hook, nil
}

// maxDetectUpload is the largest ZIP whose files the wizard looks at.
const maxDetectUpload = maxSiteUpload

// detectWait is how long the wizard waits for a server to look at a repository.
const detectWait = 90 * time.Second

// detectTokens are the Git tokens of repositories being looked at, by the
// command that looks, until it is delivered: never stored with it.
var detectTokens sync.Map

// handleSiteDetect says which PHP application a new site's source is, for the
// wizard: an upload's files are read here, a repository's on the server that
// would build it, without downloading them.
func (p *Panel) handleSiteDetect(e *core.RequestEvent) error {
	var body struct {
		Host     string `json:"host"`
		Source   string `json:"source"`
		RepoURL  string `json:"repo_url"`
		Branch   string `json:"branch"`
		GitToken string `json:"git_token"`
	}
	if data := e.Request.FormValue("data"); data != "" {
		if err := json.Unmarshal([]byte(data), &body); err != nil {
			return e.BadRequestError("Invalid request.", err)
		}
	} else if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid request.", err)
	}
	answer := func(template string) error {
		title := ""
		if t, ok := phpapps.Get(template); ok {
			title = t.Title
		}
		return e.JSON(http.StatusOK, map[string]any{"template": template, "title": title})
	}

	switch body.Source {
	case sites.SourceUpload:
		files, err := e.FindUploadedFiles("file")
		if err != nil || len(files) != 1 {
			return e.BadRequestError("Send the site's ZIP file with it.", err)
		}
		if files[0].Size > maxDetectUpload {
			return e.BadRequestError(fmt.Sprintf("The upload is larger than %d MiB.", maxDetectUpload>>20), nil)
		}
		reader, err := files[0].Reader.Open()
		if err != nil {
			return e.BadRequestError("The upload cannot be read.", err)
		}
		defer reader.Close()
		at, ok := reader.(io.ReaderAt)
		if !ok {
			return e.InternalServerError("The upload cannot be read.", nil)
		}
		paths, err := zipPaths(at, files[0].Size)
		if err != nil {
			return e.BadRequestError("The upload is not a ZIP file.", err)
		}
		return answer(phpapps.Detect(paths))
	case sites.SourceGit:
	default:
		return e.BadRequestError("The source is git or upload.", nil)
	}

	host, err := e.App.FindRecordById(schema.Hosts, body.Host)
	if err != nil {
		return e.BadRequestError("Choose the server that builds and serves the site.", nil)
	}
	if problem := checkRepoURL(body.RepoURL); problem != "" {
		return e.BadRequestError(problem, nil)
	}
	if branch := strings.TrimSpace(body.Branch); branch != "" && (!branchName.MatchString(branch) || strings.Contains(branch, "..") || strings.HasPrefix(branch, "-")) {
		return e.BadRequestError("The branch is a name such as main.", nil)
	}
	id, label := requester(e)
	ctx := e.Request.Context()
	// the token is there before the command can be delivered
	commandID := core.GenerateDefaultRandomId()
	if body.GitToken != "" {
		detectTokens.Store(commandID, body.GitToken)
	}
	defer detectTokens.Delete(commandID)
	command, err := p.Commands.Create(ctx, commands.Options{
		ID:    commandID,
		Host:  host.Id,
		Label: "Look at " + truncate(body.RepoURL, 200),
		Command: &agentv1.Command{Payload: &agentv1.Command_SiteDetect{SiteDetect: &agentv1.SiteDetect{
			RepoUrl: body.RepoURL, Branch: strings.TrimSpace(body.Branch),
		}}},
		RequestedBy: id,
		Requester:   label,
	})
	if err != nil {
		return e.BadRequestError(capitalize(err.Error())+".", nil)
	}
	done := p.waitForCommand(ctx, command.Id, detectWait)
	if done == nil {
		_ = p.Commands.Cancel(command.Id, "The panel stopped waiting for it.")
		return e.Error(http.StatusGatewayTimeout, "The server did not answer in time.", nil)
	}
	// a look the wizard makes as one types: once answered, it is no history
	defer func() {
		if err := p.App.Delete(done); err != nil {
			p.Log.Warn("failed to remove a look at a repository", "command", done.Id, "error", err)
		}
	}()
	if done.GetString("status") != schema.CommandSucceeded {
		message := done.GetString("error")
		if message == "" {
			message = "the command " + done.GetString("status")
		}
		return e.Error(http.StatusBadGateway, capitalize(message)+".", nil)
	}
	var result struct {
		Template string `json:"template"`
	}
	raw, _ := json.Marshal(done.Get("result"))
	_ = json.Unmarshal(raw, &result)
	if _, ok := phpapps.Get(result.Template); !ok {
		result.Template = ""
	}
	return answer(result.Template)
}

// enrichSiteDetect adds the Git token of the repository being looked at.
func (p *Panel) enrichSiteDetect(record *core.Record, command *agentv1.Command) error {
	if token, ok := detectTokens.Load(record.Id); ok {
		command.GetSiteDetect().GitToken, _ = token.(string)
	}
	return nil
}

// zipPaths are the files of a ZIP, as the site tool unpacks it: one folder
// that holds everything is its root.
func zipPaths(reader io.ReaderAt, size int64) ([]string, error) {
	archive, err := zip.NewReader(reader, size)
	if err != nil {
		return nil, err
	}
	prefix := sites.CommonFolder(archive.File)
	names := make([]string, 0, len(archive.File))
	for _, file := range archive.File {
		if !file.FileInfo().IsDir() {
			name := strings.TrimPrefix(filepath.ToSlash(file.Name), "./")
			names = append(names, strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(name, prefix)), "/"))
		}
	}
	return names, nil
}

// watchHealth asks a site after a release went live whether it still answers
// at its health path, for as long as a new site's check would, and keeps how
// it answered with the deployment: a release that broke the site is one
// rollback away, and the site's page says so.
func (p *Panel) watchHealth(siteID, deploymentID string) {
	started := time.Now()
	var look func()
	look = func() {
		if again := p.lookAtHealth(siteID, deploymentID, started); again {
			p.later.after(p.provision.checkEvery, look)
		}
	}
	p.later.after(0, look)
}

// lookAtHealth asks once, and says whether to ask again.
func (p *Panel) lookAtHealth(siteID, deploymentID string, started time.Time) bool {
	site, err := p.App.FindRecordById(schema.Sites, siteID)
	if err != nil || site.GetString("active_deployment") != deploymentID {
		// another release is live by now, and is asked about itself
		return false
	}
	deployment, err := p.App.FindRecordById(schema.Deployments, deploymentID)
	if err != nil {
		return false
	}
	settings, err := p.phpSettings(p.App, site)
	if err != nil || settings.GetString("health_path") == "" {
		return false
	}
	domain, err := p.App.FindFirstRecordByFilter(schema.Domains, "site={:s} && target_kind!={:redirect}",
		dbx.Params{"s": site.Id, "redirect": schema.TargetRedirect})
	if err != nil {
		// a site without a name on the web yet has no one to answer
		return false
	}
	host, err := p.App.FindRecordById(schema.Hosts, site.GetString("host"))
	if err != nil {
		return false
	}
	address, roots := p.siteAddress(host, domain)
	url := "https://" + domain.GetString("hostname") + settings.GetString("health_path")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	answer, err := p.provision.siteCheck(ctx, url, address, roots)
	health := map[string]any{"url": url, "checked_at": types.NowDateTime(), "status": answer.Status}
	switch {
	case err != nil:
		health["ok"], health["error"] = false, err.Error()
	case answer.Status >= 400 && answer.Status != http.StatusUnauthorized:
		health["ok"], health["error"] = false, fmt.Sprintf("%s answered %d.", url, answer.Status)
	default:
		health["ok"] = true
		p.setFields(deployment, map[string]any{"health": health})
		return false
	}
	if time.Since(started) < p.provision.checkFor {
		return true
	}
	p.setFields(deployment, map[string]any{"health": health})
	label := fmt.Sprintf("%s #%d", site.GetString("name"), deployment.GetInt("number"))
	events.Log(p.App, site.GetString("host"), "site.unhealthy", schema.SeverityWarning,
		fmt.Sprintf("Since release %s went live the site does not answer as it should: %s Roll it back on the site's page.", label, health["error"]),
		map[string]any{"site": site.Id, "deployment": deployment.Id})
	return false
}

// siteAddress is where the panel reaches a domain of a server, and the roots
// its certificate is checked against: the server's public address, or for a
// name of the LAN, with internal TLS or in a zone of a DNS server of the LAN,
// its address there with the internal CA.
func (p *Panel) siteAddress(host, domain *core.Record) (string, *x509.CertPool) {
	// a name with internal TLS is the LAN's, whether a DNS server of the LAN
	// holds its zone or not
	lan := domain.GetString("tls") == schema.TLSInternal
	if want := p.desiredDNS(domain); !want.Manual && lanDriver(want.Provider) {
		lan = true
	}
	if lan {
		address := host.GetString("ip4")
		if address == "" {
			address = host.GetString("ip6")
		}
		var roots *x509.CertPool
		if authority := p.authority.Load(); authority != nil {
			roots = x509.NewCertPool()
			roots.AppendCertsFromPEM(authority.RootPEM())
		}
		return address, roots
	}
	address := host.GetString("public_ip4")
	if address == "" {
		address = host.GetString("public_ip6")
	}
	return address, nil
}

// freshBase is the release that a deployment of a site installed fresh builds
// a copy of: its live one, as the site has no source of its own and its
// install would start it anew. Any other site's builds its source.
func (p *Panel) freshBase(site *core.Record) int {
	if site.GetString("type") != schema.SitePHPApp || site.GetString("source") != sites.SourceEmpty || site.GetString("build_cmd") == "" {
		return 0
	}
	settings, err := p.phpSettings(p.App, site)
	if err != nil {
		return 0
	}
	if t, ok := templateOf(settings); !ok || t.Fresh == nil {
		return 0
	}
	live, err := p.App.FindRecordById(schema.Deployments, site.GetString("active_deployment"))
	if err != nil || !live.GetBool("on_server") {
		return 0
	}
	return live.GetInt("number")
}

// templateReleased follows a PHP site's release going live: its template's
// tasks once it first is, a fresh install forgotten once done, and a look at
// whether the site still answers, unless the flow of a new site looks itself.
func (p *Panel) templateReleased(site, deployment, command *core.Record, firstLive bool) {
	settings, err := p.phpSettings(p.App, site)
	if err != nil {
		return
	}
	if firstLive {
		p.addTemplateTasks(site, settings)
	}
	p.installDone(site)
	if command.GetString("parent") == "" {
		p.watchHealth(site.Id, deployment.Id)
	}
}
