package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/php"
	"github.com/oldbear24/PocketServerManager/internal/phpapps"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/sites"
	"github.com/oldbear24/PocketServerManager/internal/stacks"
)

// PHP sites (#50). A site of type php keeps its PHP settings in site_php,
// which only the panel writes: every change is applied to the site's PHP-FPM
// container with site.php.apply, and a change of the PHP version deploys the
// active release on the new runtime image as a new deployment. Its
// environment may refer to the site's own secrets and to the passwords of the
// host's databases as ${secret:name}; they are resolved when a command is
// delivered, never stored with it.

// FPM pool defaults, as the design shows them.
const (
	defaultFPMProcesses = "dynamic"
	defaultMaxChildren  = 10
	defaultMaxRequests  = 500
	defaultTerminate    = 60
	defaultPHPCPUs      = 1.0
	defaultPHPMemoryMB  = 512
)

// fpmSettings are a site's PHP-FPM pool.
type fpmSettings struct {
	PM          string `json:"pm"`
	MaxChildren int    `json:"max_children"`
	MaxRequests int    `json:"max_requests"`
	// TerminateTimeout ends a request that runs longer, in seconds; 0 never
	TerminateTimeout int `json:"request_terminate_timeout"`
}

func defaultFPM() fpmSettings {
	return fpmSettings{PM: defaultFPMProcesses, MaxChildren: defaultMaxChildren, MaxRequests: defaultMaxRequests, TerminateTimeout: defaultTerminate}
}

func (f fpmSettings) check() error {
	if !slices.Contains([]string{"dynamic", "ondemand", "static"}, f.PM) {
		return errors.New("the process manager is dynamic, ondemand or static")
	}
	if f.MaxChildren < 1 || f.MaxChildren > 256 {
		return errors.New("a pool has 1 to 256 workers")
	}
	if f.MaxRequests < 0 || f.MaxRequests > 1000000 {
		return errors.New("a worker serves up to a million requests before it is replaced, or 0 for no limit")
	}
	if f.TerminateTimeout < 0 || f.TerminateTimeout > 86400 {
		return errors.New("a request may run up to a day, or 0 for no limit")
	}
	return nil
}

// phpSettingsBody is what PUT /sites/{id}/php changes; a field left out stays.
type phpSettingsBody struct {
	PHPVersion  *string      `json:"php_version"`
	Docroot     *string      `json:"docroot"`
	Mode        *string      `json:"mode"`
	Template    *string      `json:"template"`
	FPM         *fpmSettings `json:"fpm"`
	PHPIni      *string      `json:"php_ini"`
	Env         *string      `json:"env"`
	CPULimit    *float64     `json:"cpu_limit"`
	MemLimitMB  *int         `json:"mem_limit_mb"`
	SharedPaths *[]string    `json:"shared_paths"`
	ReleaseHook *string      `json:"release_hook"`
	// of a template's (#52)
	QueueWorker *bool   `json:"queue_worker"`
	HealthPath  *string `json:"health_path"`
}

var (
	docrootPath = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)
	iniLine     = regexp.MustCompile(`^[A-Za-z0-9_.]+(\[[A-Za-z0-9_.-]*\])?\s*=\s*[^\r\n]*$`)
	envName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretName  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

func (p *Panel) registerPHPSites() {
	p.Commands.RegisterEnricher("site.php.apply", p.enrichPHPApply)
	p.Commands.RegisterEnricher("site.rollback", p.enrichRollbackPHP)
	p.Commands.OnFinish("site.php.apply", p.phpApplyFinished)
	p.Commands.RegisterEnricher("site.detect", p.enrichSiteDetect)
}

// phpImage is the runtime image of a PHP version.
func (p *Panel) phpImage(version string) string {
	repository := php.Image
	if p.Config != nil && p.Config.PHPImage != "" {
		repository = p.Config.PHPImage
	}
	return repository + ":" + version + "-fpm"
}

// defaultPHPVersion is the one a new site gets: the newest that has been out
// for a year, as frameworks and plugins need that long to catch up, and is
// supported; or the newest supported one.
func defaultPHPVersion(now time.Time) string {
	versions := php.Versions()
	for _, version := range versions {
		if version.State(now, php.WarnDays) == php.Supported && now.Sub(version.Released) >= 365*24*time.Hour {
			return version.Version
		}
	}
	for _, version := range versions {
		if version.State(now, php.WarnDays) != php.Ended {
			return version.Version
		}
	}
	return versions[0].Version
}

// phpSettings returns a PHP site's settings, made with the defaults the first
// time.
func (p *Panel) phpSettings(app core.App, site *core.Record) (*core.Record, error) {
	if settings, err := app.FindFirstRecordByData(schema.SitePHP, "site", site.Id); err == nil {
		return settings, nil
	}
	collection, err := app.FindCachedCollectionByNameOrId(schema.SitePHP)
	if err != nil {
		return nil, err
	}
	settings := core.NewRecord(collection)
	settings.Load(map[string]any{
		"site":         site.Id,
		"php_version":  defaultPHPVersion(time.Now()),
		"template":     schema.PHPCustom,
		"mode":         schema.PHPReleases,
		"fpm":          defaultFPM(),
		"cpu_limit":    defaultPHPCPUs,
		"mem_limit_mb": defaultPHPMemoryMB,
		"shared_paths": []string{},
		"secret_names": []string{},
		"state":        schema.PHPPending,
	})
	return settings, app.Save(settings)
}

func fpmOf(settings *core.Record) fpmSettings {
	fpm := defaultFPM()
	raw, _ := json.Marshal(settings.Get("fpm"))
	_ = json.Unmarshal(raw, &fpm)
	return fpm
}

func sharedPathsOf(settings *core.Record) []string {
	paths := []string{}
	raw, _ := json.Marshal(settings.Get("shared_paths"))
	_ = json.Unmarshal(raw, &paths)
	return paths
}

// checkPHPSettings says what is wrong with a site's PHP settings.
func checkPHPSettings(settings *core.Record) error {
	if _, found := php.Find(settings.GetString("php_version")); !found {
		return fmt.Errorf("PocketCloud has no runtime image of PHP %s", settings.GetString("php_version"))
	}
	if docroot := settings.GetString("docroot"); docroot != "" && (!docrootPath.MatchString(docroot) || slices.Contains(strings.Split(docroot, "/"), "..")) {
		return errors.New("the web root is a directory of the release, such as public, or empty for the release itself")
	}
	if mode := settings.GetString("mode"); mode != schema.PHPReleases && mode != schema.PHPClassic {
		return errors.New("the mode is releases or classic")
	}
	if err := fpmOf(settings).check(); err != nil {
		return err
	}
	for i, line := range strings.Split(settings.GetString("php_ini"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if !iniLine.MatchString(line) {
			return fmt.Errorf("php.ini line %d is not a setting such as memory_limit = 256M", i+1)
		}
	}
	if _, _, err := parsePHPEnv(settings.GetString("env")); err != nil {
		return err
	}
	if cpus := settings.GetFloat("cpu_limit"); cpus < 0 || cpus > 64 {
		return errors.New("a site gets up to 64 CPUs")
	}
	if memory := settings.GetInt("mem_limit_mb"); memory != 0 && (memory < 64 || memory > 65536) {
		return errors.New("a site gets 64 MiB to 64 GiB of memory")
	}
	paths := sharedPathsOf(settings)
	if len(paths) > 32 {
		return errors.New("a site has up to 32 shared paths")
	}
	for _, path := range paths {
		if err := sites.ValidSharedPath(path); err != nil {
			return err
		}
	}
	if len(settings.GetString("release_hook")) > 4096 {
		return errors.New("the release hook is a command of up to 4096 characters")
	}
	if template := settings.GetString("template"); template != schema.PHPCustom && template != "" {
		if _, ok := phpapps.Get(template); !ok {
			return fmt.Errorf("the application is custom or one of %s", strings.Join(phpapps.Names(), ", "))
		}
	}
	if health := settings.GetString("health_path"); health != "" && (!strings.HasPrefix(health, "/") || len(health) > 255 || strings.ContainsAny(health, " #\r\n\t")) {
		return errors.New("the health check is a path of the site, such as /up, or empty for none")
	}
	if t, ok := templateOf(settings); settings.GetBool("queue_worker") && (!ok || t.Worker == nil) {
		return errors.New("only an application whose template has a worker, such as Laravel, gets one")
	}
	return nil
}

// parsePHPEnv splits a site's environment into its plain variables and those
// that refer to secrets, whose values stay as they are written.
func parsePHPEnv(text string) (plain map[string]string, withSecrets map[string]string, err error) {
	plain, withSecrets = map[string]string{}, map[string]string{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, found := strings.Cut(line, "=")
		name = strings.TrimSpace(strings.TrimPrefix(name, "export "))
		if !found || !envName.MatchString(name) {
			return nil, nil, fmt.Errorf("environment line %d is not NAME=value", i+1)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
		if strings.HasPrefix(name, "POCKETCLOUD_") {
			return nil, nil, fmt.Errorf("%s: names that start with POCKETCLOUD_ are the panel's", name)
		}
		if stacks.SecretReference.MatchString(value) {
			withSecrets[name] = value
		} else {
			plain[name] = value
		}
	}
	return plain, withSecrets, nil
}

// phpSecrets resolves the environment variables of a site that refer to
// secrets: the site's own, by name, or db/<database>, a password of a
// database on the site's server.
func (p *Panel) phpSecrets(site, settings *core.Record) (map[string]string, error) {
	resolved, _, err := p.phpSecretValues(site, settings)
	return resolved, err
}

// phpSecretValues are a site's variables with secrets, resolved, and the
// secrets' own values, which a release hook's output hides wherever they
// appear, as within a longer variable such as DATABASE_URL.
func (p *Panel) phpSecretValues(site, settings *core.Record) (map[string]string, []string, error) {
	_, withSecrets, err := parsePHPEnv(settings.GetString("env"))
	if err != nil {
		return nil, nil, err
	}
	own := map[string]string{}
	if sealed := settings.GetString("secrets"); sealed != "" {
		plain, err := p.Box.OpenString(sealed)
		if err != nil {
			return nil, nil, fmt.Errorf("the site's secrets cannot be opened: %w", err)
		}
		if err := json.Unmarshal([]byte(plain), &own); err != nil {
			return nil, nil, err
		}
	}
	resolved := map[string]string{}
	values := []string{}
	for name, value := range withSecrets {
		var failure error
		resolved[name] = stacks.SecretReference.ReplaceAllStringFunc(value, func(reference string) string {
			secret := stacks.SecretReference.FindStringSubmatch(reference)[1]
			if strings.HasPrefix(secret, "db/") {
				password, err := p.databaseSecret(site.GetString("host"), secret)
				if err != nil {
					failure = err
				}
				values = append(values, password)
				return password
			}
			text, ok := own[secret]
			if !ok {
				failure = fmt.Errorf("%s: the site has no secret %s: %w", name, secret, stacks.ErrUnknownSecret)
			}
			values = append(values, text)
			return text
		})
		if failure != nil {
			return nil, nil, failure
		}
	}
	for _, value := range own {
		values = append(values, value)
	}
	slices.Sort(values)
	return resolved, slices.Compact(values), nil
}

// fpmConf is a site's pool, after the image's own: its process manager, and
// what it may not change, as its PHP sees only its own directory and /tmp.
func fpmConf(site string, fpm fpmSettings) string {
	lines := []string{
		"; Written by PocketCloud: the pool of the site " + site + ", after the image's own.",
		"[global]",
		"; a reload after a release switch lets the requests under way finish",
		"process_control_timeout = 30s",
		"",
		"[www]",
		"pm = " + fpm.PM,
		"pm.max_children = " + strconv.Itoa(fpm.MaxChildren),
	}
	if fpm.PM == "dynamic" {
		spare := max(1, fpm.MaxChildren/4)
		lines = append(lines,
			"pm.start_servers = "+strconv.Itoa(spare),
			"pm.min_spare_servers = 1",
			"pm.max_spare_servers = "+strconv.Itoa(max(spare, min(fpm.MaxChildren, fpm.MaxChildren/2+1))),
		)
	}
	lines = append(lines,
		"pm.max_requests = "+strconv.Itoa(fpm.MaxRequests),
		"request_terminate_timeout = "+strconv.Itoa(fpm.TerminateTimeout)+"s",
		"php_admin_value[open_basedir] = "+sites.Root+"/"+site+":/tmp",
		"php_admin_value[upload_tmp_dir] = /tmp",
		"php_admin_value[sys_temp_dir] = /tmp",
		"php_admin_value[session.save_path] = /tmp",
		"php_admin_flag[allow_url_include] = off",
		"",
	)
	return strings.Join(lines, "\n")
}

// phpIni is a site's own php.ini lines, after the image's.
func phpIni(settings *core.Record) string {
	return "; Written by PocketCloud: the site's own php.ini lines.\n" + strings.TrimSpace(settings.GetString("php_ini")) + "\n"
}

// siteNetworks are the database networks a site's containers join.
func (p *Panel) siteNetworks(site *core.Record) []string {
	networks, err := p.App.FindRecordsByFilter(schema.DBNetworks, "site={:s}", "name", 0, 0, dbx.Params{"s": site.Id})
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(networks))
	for _, network := range networks {
		names = append(names, network.GetString("name"))
	}
	return names
}

// phpApplyPayload is a site's PHP-FPM as its settings have it, without its
// secrets, which are added when the command is delivered.
func (p *Panel) phpApplyPayload(site *core.Record) (*agentv1.SitePhpApply, *core.Record, error) {
	settings, err := p.phpSettings(p.App, site)
	if err != nil {
		return nil, nil, err
	}
	return p.phpApplyPayloadOf(site, settings)
}

// phpApplyPayloadOf is the apply of settings that need not be stored yet, as
// a rollback's PHP version, kept once the rollback succeeded.
func (p *Panel) phpApplyPayloadOf(site, settings *core.Record) (*agentv1.SitePhpApply, *core.Record, error) {
	plain, _, err := parsePHPEnv(settings.GetString("env"))
	if err != nil {
		return nil, nil, err
	}
	if plain == nil {
		plain = map[string]string{}
	}
	plain[envMode] = modeOf(settings)
	return &agentv1.SitePhpApply{
		Site:          site.GetString("name"),
		Image:         p.phpImage(settings.GetString("php_version")),
		FpmConf:       fpmConf(site.GetString("name"), fpmOf(settings)),
		PhpIni:        phpIni(settings),
		Env:           p.smtpEnv(plain),
		WorkerCommand: workerCommand(settings),
		CpuLimit:      settings.GetFloat("cpu_limit"),
		MemLimitBytes: uint64(max(settings.GetInt("mem_limit_mb"), 0)) << 20, //nolint:gosec // checked above 0
		Networks:      p.siteNetworks(site),
		Classic:       settings.GetString("mode") == schema.PHPClassic,
	}, settings, nil
}

// applyPHP sends a site's PHP settings to its PHP-FPM container.
func (p *Panel) applyPHP(ctx context.Context, site *core.Record, options commands.Options) (*core.Record, error) {
	payload, settings, err := p.phpApplyPayload(site)
	if err != nil {
		return nil, err
	}
	options.Host = site.GetString("host")
	if options.Label == "" {
		options.Label = site.GetString("name")
	}
	options.Command = &agentv1.Command{Payload: &agentv1.Command_SitePhpApply{SitePhpApply: payload}}
	command, err := p.Commands.Create(ctx, options)
	if err != nil {
		return nil, err
	}
	p.setFields(settings, map[string]any{"state": schema.PHPPending})
	return command, nil
}

// phpDeploy is the PHP part of a deployment.
func (p *Panel) phpDeploy(site, deployment *core.Record) (*agentv1.SitePhpDeploy, []string, string, error) {
	settings, err := p.phpSettings(p.App, site)
	if err != nil {
		return nil, nil, "", err
	}
	plain, _, err := parsePHPEnv(settings.GetString("env"))
	if err != nil {
		return nil, nil, "", err
	}
	version := settings.GetString("php_version")
	if deployment != nil && deployment.GetString("php_version") != "" {
		version = deployment.GetString("php_version")
	}
	deploy := &agentv1.SitePhpDeploy{
		Image:    p.phpImage(version),
		Docroot:  settings.GetString("docroot"),
		Classic:  settings.GetString("mode") == schema.PHPClassic,
		Env:      plain,
		Networks: p.siteNetworks(site),
	}
	templateDeploy(deploy, settings, deployment)
	return deploy, sharedPathsOf(settings), settings.GetString("release_hook"), nil
}

// siteOfCommand finds the site a command of a host names.
func (p *Panel) siteOfCommand(host, name string) (*core.Record, error) {
	site, err := p.App.FindFirstRecordByFilter(schema.Sites, "host={:h} && name={:n}", dbx.Params{"h": host, "n": name})
	if err != nil {
		return nil, errors.New("the site was removed")
	}
	return site, nil
}

// enrichPHPApply adds the site's secrets as the command is delivered.
func (p *Panel) enrichPHPApply(record *core.Record, command *agentv1.Command) error {
	payload := command.GetSitePhpApply()
	site, err := p.siteOfCommand(record.GetString("host"), payload.GetSite())
	if err != nil {
		return err
	}
	settings, err := p.phpSettings(p.App, site)
	if err != nil {
		return err
	}
	secrets, err := p.phpSecrets(site, settings)
	if err != nil {
		return err
	}
	if payload.Secrets, err = p.smtpSecret(secrets); err != nil {
		return err
	}
	return nil
}

// enrichRollbackPHP adds the site's secrets to the PHP-FPM of a rollback to a
// release of another PHP version.
func (p *Panel) enrichRollbackPHP(record *core.Record, command *agentv1.Command) error {
	apply := command.GetSiteRollback().GetApply()
	if apply == nil {
		return nil
	}
	site, err := p.siteOfCommand(record.GetString("host"), apply.GetSite())
	if err != nil {
		return err
	}
	settings, err := p.phpSettings(p.App, site)
	if err != nil {
		return err
	}
	if apply.Secrets, err = p.phpSecrets(site, settings); err != nil {
		return err
	}
	apply.Secrets, err = p.smtpSecret(apply.Secrets)
	return err
}

// phpApplied keeps the state of a site's PHP-FPM container after an apply, of
// its own or with a deployment.
func (p *Panel) phpApplied(site *core.Record, result map[string]any, failure string) {
	settings, err := p.App.FindFirstRecordByData(schema.SitePHP, "site", site.Id)
	if err != nil {
		return
	}
	if failure != "" {
		p.setFields(settings, map[string]any{"state": schema.PHPFailed, "last_error": truncate(failure, 4096)})
		events.Log(p.App, site.GetString("host"), "site.php_failed", schema.SeverityWarning,
			truncate(fmt.Sprintf("PHP-FPM of %s could not be applied: %s", site.GetString("name"), failure), 2048),
			map[string]any{"site": site.Id})
		return
	}
	// the tag it was pulled by, pinned by its digest: repo:8.4-fpm@sha256:…
	image, _ := result["image"].(string)
	if digest, _ := result["digest"].(string); digest != "" {
		if _, pin, found := strings.Cut(digest, "@"); found && image != "" && !strings.Contains(image, "@") {
			image += "@" + pin
		} else {
			image = digest
		}
	}
	p.setFields(settings, map[string]any{"state": schema.PHPRunning, "image": image, "applied_at": types.NowDateTime(), "last_error": ""})
}

// phpDeployApplied keeps the state of a site's PHP-FPM after a deployment
// that came with one.
func (p *Panel) phpDeployApplied(site, command *core.Record) {
	var result struct {
		PHP      map[string]any `json:"php"`
		PHPError string         `json:"php_error"`
	}
	raw, _ := json.Marshal(command.Get("result"))
	_ = json.Unmarshal(raw, &result)
	switch {
	case result.PHPError != "":
		p.phpApplied(site, nil, result.PHPError)
	case len(result.PHP) > 0:
		p.phpApplied(site, result.PHP, "")
	}
}

// keepLivePHPVersion goes back to the PHP version of the release a site
// serves after a deployment for another version failed: its PHP-FPM may have
// changed already, and must run what is live.
func (p *Panel) keepLivePHPVersion(site, failed *core.Record) {
	// a deployment after it, which may be for yet another version, decides
	if later, err := p.App.FindRecordsByFilter(schema.Deployments, "site={:s} && number>{:n} && status!={:failed}", "", 1, 0,
		dbx.Params{"s": site.Id, "n": failed.GetInt("number"), "failed": schema.DeploymentFailed}); err == nil && len(later) > 0 {
		return
	}
	live, err := p.App.FindRecordById(schema.Deployments, site.GetString("active_deployment"))
	if err != nil || live.GetString("php_version") == "" || live.GetString("php_version") == failed.GetString("php_version") {
		return
	}
	settings, err := p.App.FindFirstRecordByData(schema.SitePHP, "site", site.Id)
	if err != nil || settings.GetString("php_version") == live.GetString("php_version") {
		return
	}
	version := live.GetString("php_version")
	p.setFields(settings, map[string]any{"php_version": version})
	if _, err := p.applyPHP(context.Background(), site, commands.Options{Requester: "panel"}); err != nil {
		p.Log.Error("failed to go back to the live release's PHP", "site", site.GetString("name"), "version", version, "error", err)
	}
	events.Log(p.App, site.GetString("host"), "site.php_kept", schema.SeverityInfo,
		fmt.Sprintf("%s stays on PHP %s, as the deployment for PHP %s failed.", site.GetString("name"), version, failed.GetString("php_version")),
		map[string]any{"site": site.Id, "deployment": failed.Id})
}

// phpApplyFinished keeps the state of a site's PHP-FPM container.
func (p *Panel) phpApplyFinished(command *core.Record) {
	var payload struct {
		Site  string `json:"site"`
		Image string `json:"image"`
	}
	raw, _ := json.Marshal(command.Get("payload"))
	_ = json.Unmarshal(raw, &payload)
	site, err := p.siteOfCommand(command.GetString("host"), payload.Site)
	if err != nil {
		return
	}
	if command.GetString("status") != schema.CommandSucceeded {
		p.phpApplied(site, nil, command.GetString("error"))
		return
	}
	result := map[string]any{}
	raw, _ = json.Marshal(command.Get("result"))
	_ = json.Unmarshal(raw, &result)
	p.phpApplied(site, result, "")
}

// handlePHPSettings changes a PHP site's settings and applies them: a new
// version deploys the active release on its image as a new deployment, and a
// new web root changes the edge's route.
func (p *Panel) handlePHPSettings(e *core.RequestEvent) error {
	site, err := p.findSite(e)
	if err != nil {
		return err
	}
	if site.GetString("type") != schema.SitePHPApp {
		return e.BadRequestError("This is not a PHP site.", nil)
	}
	var body phpSettingsBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}
	settings, err := p.phpSettings(e.App, site)
	if err != nil {
		return e.InternalServerError("", err)
	}
	before := settings.GetString("php_version")
	beforeRoot := settings.GetString("docroot")
	beforeEnv := settings.GetString("env")
	set := func(field string, value any) { settings.Set(field, value) }
	if body.PHPVersion != nil {
		set("php_version", strings.TrimSpace(*body.PHPVersion))
	}
	if body.Docroot != nil {
		set("docroot", strings.Trim(strings.TrimSpace(*body.Docroot), "/"))
	}
	if body.Mode != nil {
		set("mode", *body.Mode)
	}
	if body.Template != nil {
		set("template", *body.Template)
	}
	if body.FPM != nil {
		set("fpm", *body.FPM)
	}
	if body.PHPIni != nil {
		set("php_ini", *body.PHPIni)
	}
	if body.Env != nil {
		set("env", *body.Env)
	}
	if body.CPULimit != nil {
		set("cpu_limit", *body.CPULimit)
	}
	if body.MemLimitMB != nil {
		set("mem_limit_mb", *body.MemLimitMB)
	}
	if body.SharedPaths != nil {
		set("shared_paths", *body.SharedPaths)
	}
	if body.ReleaseHook != nil {
		set("release_hook", strings.TrimSpace(*body.ReleaseHook))
	}
	if body.QueueWorker != nil {
		set("queue_worker", *body.QueueWorker)
	}
	if body.HealthPath != nil {
		set("health_path", strings.TrimSpace(*body.HealthPath))
	}
	if err := checkPHPSettings(settings); err != nil {
		return e.BadRequestError(capitalize(err.Error())+".", nil)
	}
	if _, err := p.phpSecrets(site, settings); err != nil {
		return e.BadRequestError(capitalize(err.Error())+".", nil)
	}
	if err := e.App.SaveWithContext(audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e)), settings); err != nil {
		return e.BadRequestError("The settings could not be saved: "+err.Error(), nil)
	}

	id, label := requester(e)
	result := map[string]any{}
	version := settings.GetString("php_version")
	live, _ := p.App.FindRecordById(schema.Deployments, site.GetString("active_deployment"))
	// in classic mode the code is the application's to change, and a new
	// release would replace what it wrote: PHP-FPM changes on its own
	if version != before && live != nil && settings.GetString("mode") != schema.PHPClassic {
		// the live release, built again on the new image as a deployment of its
		// own, which a rollback takes back to the old version; PHP-FPM changes
		// with it
		deployment, err := p.startDeployment(e.Request.Context(), site, deploymentRequest{
			Trigger: schema.TriggerManual, RequestedBy: id, Requester: label,
			Message: fmt.Sprintf("PHP %s → %s", before, version), PHPApply: true, PHPFrom: before, Again: live,
		})
		if err != nil {
			return e.BadRequestError(capitalize(err.Error())+".", nil)
		}
		result["deployment"] = deployment.Id
	} else if live, redeploys := p.redeploysOnEnv(site, settings); redeploys && settings.GetString("env") != beforeEnv {
		// the application caches its environment at the release (#52)
		deployment, err := p.redeployForEnv(e.Request.Context(), site, settings, live, commands.Options{RequestedBy: id, Requester: label})
		if err != nil {
			return e.BadRequestError(capitalize(err.Error())+".", nil)
		}
		result["deployment"] = deployment.Id
	} else {
		command, err := p.applyPHP(e.Request.Context(), site, commands.Options{RequestedBy: id, Requester: label})
		if err != nil {
			return e.InternalServerError("The settings were saved, but not applied.", err)
		}
		result["command"] = command.Id
	}
	if settings.GetString("docroot") != beforeRoot {
		host := site.GetString("host")
		p.later.after(0, func() { p.applyEdgeLogged(host, false) })
	}
	result["settings"] = settings
	return e.JSON(http.StatusOK, result)
}

// handlePHPSecrets sets a PHP site's own secrets, which its environment refers
// to as ${secret:name}: a name with a value sets it, one with null removes it.
func (p *Panel) handlePHPSecrets(e *core.RequestEvent) error {
	site, err := p.findSite(e)
	if err != nil {
		return err
	}
	if site.GetString("type") != schema.SitePHPApp {
		return e.BadRequestError("This is not a PHP site.", nil)
	}
	var body map[string]*string
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body: an object of names and values.", err)
	}
	settings, err := p.phpSettings(e.App, site)
	if err != nil {
		return e.InternalServerError("", err)
	}
	own, err := p.ownSecrets(settings)
	if err != nil {
		return e.InternalServerError("The site's secrets cannot be opened.", err)
	}
	for name, value := range body {
		if !secretName.MatchString(name) {
			return e.BadRequestError(fmt.Sprintf("%q is not a secret's name: letters, digits, . _ and -.", name), nil)
		}
		if value == nil {
			delete(own, name)
		} else {
			own[name] = *value
		}
	}
	if err := p.setOwnSecrets(settings, own); err != nil {
		return e.InternalServerError("", err)
	}
	names := settings.GetStringSlice("secret_names")
	// a secret the environment refers to stays
	if _, err := p.phpSecrets(site, settings); err != nil {
		return e.BadRequestError(capitalize(err.Error())+".", nil)
	}
	if err := e.App.SaveWithContext(audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e)), settings); err != nil {
		return e.BadRequestError("The secrets could not be saved: "+err.Error(), nil)
	}
	id, label := requester(e)
	if live, redeploys := p.redeploysOnEnv(site, settings); redeploys {
		// the application caches its environment, secrets and all (#52)
		deployment, err := p.redeployForEnv(e.Request.Context(), site, settings, live, commands.Options{RequestedBy: id, Requester: label})
		if err != nil {
			return e.BadRequestError(capitalize(err.Error())+".", nil)
		}
		return e.JSON(http.StatusOK, map[string]any{"deployment": deployment.Id, "secret_names": names})
	}
	command, err := p.applyPHP(e.Request.Context(), site, commands.Options{RequestedBy: id, Requester: label})
	if err != nil {
		return e.InternalServerError("The secrets were saved, but not applied.", err)
	}
	return e.JSON(http.StatusOK, map[string]any{"command": command.Id, "secret_names": names})
}

// handlePHPApply applies a PHP site's settings again, as after its container
// was removed by hand.
func (p *Panel) handlePHPApply(e *core.RequestEvent) error {
	site, err := p.findSite(e)
	if err != nil {
		return err
	}
	if site.GetString("type") != schema.SitePHPApp {
		return e.BadRequestError("This is not a PHP site.", nil)
	}
	id, label := requester(e)
	command, err := p.applyPHP(e.Request.Context(), site, commands.Options{RequestedBy: id, Requester: label})
	if err != nil {
		return e.BadRequestError(capitalize(err.Error())+".", nil)
	}
	return e.JSON(http.StatusOK, map[string]any{"command": command.Id})
}

// siteNetwork names the database network of a site. Its dot keeps it apart
// from a stack's, as stack names have none.
func siteNetwork(site string) string {
	return databases.NetworkName("site." + site)
}

// connectSite gives a PHP site a database of a server on its own host: the
// server joins a database network of the site's, the site's environment gets
// DB_HOST, DB_NAME, DB_USER and DB_PASSWORD (as ${secret:db/<name>}), and its
// PHP-FPM, when it runs, joins the network with them.
func (p *Panel) connectSite(ctx context.Context, request databaseRequest, server, database, site *core.Record, apply bool) error {
	if site.GetString("host") != server.GetString("host") {
		return errors.New("a site can only use the database servers of its own server")
	}
	engine, err := engineOfServer(server)
	if err != nil {
		return err
	}
	network := siteNetwork(site.GetString("name"))
	if _, err := p.App.FindFirstRecordByFilter(schema.DBNetworks, "server={:server} && site={:site}",
		dbx.Params{"server": server.Id, "site": site.Id}); err != nil {
		collection, err := p.App.FindCachedCollectionByNameOrId(schema.DBNetworks)
		if err != nil {
			return err
		}
		record := core.NewRecord(collection)
		record.Set("server", server.Id)
		record.Set("site", site.Id)
		record.Set("name", network)
		if err := p.App.SaveWithContext(audit.WithActor(ctx, request.Actor), record); err != nil {
			return err
		}
	}
	account, err := p.userOf(server, database.GetString("name"))
	if err != nil {
		return err
	}
	settings, err := p.phpSettings(p.App, site)
	if err != nil {
		return err
	}
	connection := databases.Connection{Engine: engine, Server: server.GetString("name"), Database: database.GetString("name"), User: account.GetString("name")}
	// and the names the site's application gives them, as Laravel's (#52)
	env, _ := databases.AddEnv(settings.GetString("env"), append(connection.EnvLines(), templateDatabaseEnv(settings, connection)...))
	settings.Set("env", env)
	settings.Set("database", database.Id)
	if err := p.App.SaveWithContext(audit.WithActor(ctx, request.Actor), settings); err != nil {
		return err
	}
	if _, err := p.Commands.Create(ctx, commands.Options{
		Host:        site.GetString("host"),
		Label:       site.GetString("name") + " → " + server.GetString("name"),
		RequestedBy: request.UserID,
		Command: &agentv1.Command{Payload: &agentv1.Command_DbNetworkAttach{DbNetworkAttach: &agentv1.DbNetworkAttach{
			Network:           network,
			ServerContainerId: server.GetString("container_ref"),
			ServerAlias:       server.GetString("name"),
		}}},
	}); err != nil {
		return err
	}
	if apply {
		options := commands.Options{RequestedBy: request.UserID, Requester: request.Label}
		if live, redeploys := p.redeploysOnEnv(site, settings); redeploys {
			_, err = p.redeployForEnv(ctx, site, settings, live, options)
		} else {
			_, err = p.applyPHP(ctx, site, options)
		}
	}
	return err
}

// phpStatusWait is how long a request waits for the PHP-FPM status of a site.
const phpStatusWait = 15 * time.Second

// phpStatus is what the site page shows of a site's PHP-FPM: its workers, as
// FPM's status page counts them, and how well OPcache serves.
type phpStatus struct {
	PHPVersion string `json:"php_version"`
	// none when FPM's status page could not be read: the errors say why
	Workers *phpWorkers `json:"workers"`
	OPcache *phpOPcache `json:"opcache"`
	// the extensions the runtime image added to PHP's own
	Extensions []string `json:"extensions"`
	// the sizes of the shared paths, in bytes; a path that could not be
	// measured has none
	Shared []phpShared    `json:"shared"`
	Errors []string       `json:"errors,omitempty"`
	ReadAt types.DateTime `json:"read_at"`
}

type phpShared struct {
	Path  string `json:"path"`
	Bytes *int64 `json:"bytes"`
}

type phpWorkers struct {
	Active int64 `json:"active"`
	Idle   int64 `json:"idle"`
	Total  int64 `json:"total"`
	// the most a site may have, of its settings
	Max                int64 `json:"max"`
	MaxActive          int64 `json:"max_active"`
	MaxChildrenReached int64 `json:"max_children_reached"`
	ListenQueue        int64 `json:"listen_queue"`
	Accepted           int64 `json:"accepted"`
	SlowRequests       int64 `json:"slow_requests"`
	// how long FPM has run, in seconds
	Uptime int64 `json:"uptime"`
}

type phpOPcache struct {
	Enabled       bool    `json:"enabled"`
	HitRate       float64 `json:"hit_rate"`
	UsedBytes     int64   `json:"used_bytes"`
	FreeBytes     int64   `json:"free_bytes"`
	CachedScripts int64   `json:"cached_scripts"`
	Full          bool    `json:"full"`
}

// readPHPStatus picks out of what pocketcloud-php-status printed what the
// site page shows.
func readPHPStatus(raw string, fpm fpmSettings, shared []string) (phpStatus, error) {
	var answer struct {
		PHPVersion string           `json:"php_version"`
		Extensions []string         `json:"extensions"`
		Shared     map[string]int64 `json:"shared"`
		FPM        *struct {
			Active             int64 `json:"active processes"`
			Idle               int64 `json:"idle processes"`
			Total              int64 `json:"total processes"`
			MaxActive          int64 `json:"max active processes"`
			MaxChildrenReached int64 `json:"max children reached"`
			ListenQueue        int64 `json:"listen queue"`
			Accepted           int64 `json:"accepted conn"`
			SlowRequests       int64 `json:"slow requests"`
			Since              int64 `json:"start since"`
		} `json:"fpm"`
		OPcache *struct {
			Enabled bool `json:"opcache_enabled"`
			Full    bool `json:"cache_full"`
			Memory  struct {
				Used int64 `json:"used_memory"`
				Free int64 `json:"free_memory"`
			} `json:"memory_usage"`
			Statistics struct {
				Scripts int64   `json:"num_cached_scripts"`
				HitRate float64 `json:"opcache_hit_rate"`
			} `json:"opcache_statistics"`
		} `json:"opcache"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(raw), &answer); err != nil {
		return phpStatus{}, fmt.Errorf("the agent's answer cannot be read: %w", err)
	}
	status := phpStatus{
		PHPVersion: answer.PHPVersion, Extensions: answer.Extensions, Shared: []phpShared{},
		Errors: answer.Errors, ReadAt: types.NowDateTime(),
	}
	if status.Extensions == nil {
		status.Extensions = []string{}
	}
	for _, path := range shared {
		item := phpShared{Path: path}
		// the agent names a path as path.Clean does
		if size, ok := answer.Shared[filepath.Clean(path)]; ok {
			item.Bytes = &size
		}
		status.Shared = append(status.Shared, item)
	}
	if f := answer.FPM; f != nil {
		status.Workers = &phpWorkers{
			Active: f.Active, Idle: f.Idle, Total: f.Total, Max: int64(fpm.MaxChildren), MaxActive: f.MaxActive,
			MaxChildrenReached: f.MaxChildrenReached, ListenQueue: f.ListenQueue, Accepted: f.Accepted,
			SlowRequests: f.SlowRequests, Uptime: f.Since,
		}
	}
	if o := answer.OPcache; o != nil {
		status.OPcache = &phpOPcache{
			Enabled: o.Enabled, HitRate: o.Statistics.HitRate, UsedBytes: o.Memory.Used,
			FreeBytes: o.Memory.Free, CachedScripts: o.Statistics.Scripts, Full: o.Full,
		}
	}
	return status, nil
}

// handlePHPStatus asks the site's agent how its PHP-FPM is doing.
func (p *Panel) handlePHPStatus(e *core.RequestEvent) error {
	site, err := p.findSite(e)
	if err != nil {
		return err
	}
	if site.GetString("type") != schema.SitePHPApp {
		return e.BadRequestError("This is not a PHP site.", nil)
	}
	settings, err := p.phpSettings(p.App, site)
	if err != nil {
		return e.InternalServerError("Failed to read the site's PHP settings.", err)
	}
	ctx := e.Request.Context()
	id, label := requester(e)
	command, err := p.Commands.Create(ctx, commands.Options{
		Host: site.GetString("host"),
		Command: &agentv1.Command{Payload: &agentv1.Command_SitePhpStatus{
			SitePhpStatus: &agentv1.SitePhpStatus{Site: site.GetString("name"), SharedPaths: sharedPathsOf(settings)},
		}},
		Label:       "Read the PHP-FPM status of " + site.GetString("name"),
		RequestedBy: id,
		Requester:   label,
	})
	if err != nil {
		return e.BadRequestError(capitalize(err.Error())+".", nil)
	}
	done := p.waitForCommand(ctx, command.Id, phpStatusWait)
	if done == nil {
		_ = p.Commands.Cancel(command.Id, "The panel stopped waiting for it.")
		return e.Error(http.StatusGatewayTimeout, "The server did not answer in time.", nil)
	}
	// a read the page makes every minute: once answered, it is no history
	defer func() {
		if err := p.App.Delete(done); err != nil {
			p.Log.Warn("failed to remove a read PHP status", "command", done.Id, "error", err)
		}
	}()
	if done.GetString("status") != schema.CommandSucceeded {
		message := done.GetString("error")
		if message == "" {
			message = "the command " + done.GetString("status")
		}
		return e.Error(http.StatusBadGateway, capitalize(message)+".", nil)
	}
	status, err := readPHPStatus(done.GetString("result"), fpmOf(settings), sharedPathsOf(settings))
	if err != nil {
		return e.Error(http.StatusBadGateway, capitalize(err.Error())+".", nil)
	}
	return e.JSON(http.StatusOK, status)
}

// handlePHPLogs follows the log of a site's PHP-FPM: PHP's errors and what
// its scripts write to stderr.
func (p *Panel) handlePHPLogs(e *core.RequestEvent) error {
	site, err := p.findSite(e)
	if err != nil {
		return err
	}
	if site.GetString("type") != schema.SitePHPApp {
		return e.BadRequestError("This is not a PHP site.", nil)
	}
	name := sites.PHPContainer(site.GetString("name"))
	container, err := p.App.FindFirstRecordByFilter(schema.Containers, "host={:host} && name={:name}",
		dbx.Params{"host": site.GetString("host"), "name": name})
	if err != nil {
		return e.NotFoundError("The site's PHP-FPM does not run.", err)
	}
	return p.serveLogs(e, site.GetString("host"), []string{container.GetString("docker_id")}, name)
}
