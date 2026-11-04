package panel

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/edge"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/version"
)

const (
	// expiringWithin is when a route's certificate counts as expiring soon.
	expiringWithin = 14 * 24 * time.Hour
	// edgeSettle lets a burst of changes, such as several domains saved at once,
	// become one config
	edgeSettle = 200 * time.Millisecond
)

// The edge of a host serves its domains with the certificates that cover them
// (ADR 0002). The panel generates the edge's complete config whenever one of
// them changes, and whenever a heartbeat says the edge serves something else, and
// sends it with edge.apply; certificates go there first with cert.install.
type edgeLocks struct{ locks sync.Map }

func (l *edgeLocks) of(hostID string) *sync.Mutex {
	lock, _ := l.locks.LoadOrStore(hostID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (p *Panel) registerEdge() {
	p.App.OnRecordValidate(schema.Domains).BindFunc(p.validateDomain)

	changed := func(e *core.RecordEvent) error {
		hosts := []string{e.Record.GetString("host")}
		if original := e.Record.Original(); original != nil && original.GetString("host") != hosts[0] {
			hosts = append(hosts, original.GetString("host"))
		}
		p.later.after(edgeSettle, func() {
			for _, host := range hosts {
				p.applyEdgeLogged(host, false)
			}
		})
		return e.Next()
	}
	p.App.OnRecordAfterCreateSuccess(schema.Domains).BindFunc(changed)
	p.App.OnRecordAfterUpdateSuccess(schema.Domains).BindFunc(changed)
	p.App.OnRecordAfterDeleteSuccess(schema.Domains).BindFunc(changed)

	p.App.OnRecordUpdateRequest(schema.Hosts).BindFunc(p.guardEdgeRole)
	p.App.OnRecordAfterUpdateSuccess(schema.Hosts).BindFunc(func(e *core.RecordEvent) error {
		if original := e.Record.Original(); original != nil && hasEdgeRole(original) != hasEdgeRole(e.Record) {
			id := e.Record.Id
			p.later.after(edgeSettle, func() { p.applyEdgeLogged(id, false) })
		}
		return e.Next()
	})

	p.Commands.RegisterEnricher("cert.install", p.enrichCertInstall)
	p.Commands.OnFinish("edge.apply", p.edgeApplyFinished)
	p.Commands.OnFinish("cert.install", p.certInstallFinished)
	p.Commands.OnFinish("cert.remove", p.certRemoveFinished)

	p.App.Cron().MustAdd("pocketcloud_certificates_expiry", "7 * * * *", p.markExpiredCertificates)
}

func (p *Panel) bindEdgeRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	everyone := requireRole(schema.RoleViewer, schema.RoleOperator, schema.RoleAdmin)
	operators := requireRole(schema.RoleOperator, schema.RoleAdmin)
	admins := requireRole(schema.RoleAdmin)

	group.GET("/hosts/{id}/edge", p.handleEdgeState).BindFunc(everyone)
	group.POST("/hosts/{id}/edge/apply", p.handleEdgeApply).BindFunc(operators)
	group.POST("/certificates", p.handleCertificateUpload).BindFunc(admins)
	group.DELETE("/certificates/{id}", p.handleCertificateRemove).BindFunc(admins)
}

func hasEdgeRole(host *core.Record) bool {
	return slices.Contains(host.GetStringSlice("roles"), schema.HostRoleEdge)
}

// validateDomain checks every save of a domain, however it arrives.
func (p *Panel) validateDomain(e *core.RecordEvent) error {
	record := e.Record
	errs := validation.Errors{}

	hostname := strings.ToLower(strings.TrimSpace(record.GetString("hostname")))
	record.Set("hostname", hostname)
	if err := edge.ValidHostname(hostname); err != nil {
		errs["hostname"] = validation.NewError("validation_invalid_hostname", capitalize(err.Error()))
	}

	if record.GetString("tls") == "" {
		record.Set("tls", schema.TLSAuto)
	}

	if host, err := e.App.FindRecordById(schema.Hosts, record.GetString("host")); err == nil && !hasEdgeRole(host) {
		errs["host"] = validation.NewError("validation_no_edge", "The server has no edge role: give it one first.")
	}

	switch record.GetString("target_kind") {
	case schema.TargetContainer:
		name := strings.TrimSpace(record.GetString("container"))
		record.Set("container", name)
		port := record.GetInt("port")
		if problem := checkUpstream(name, port); problem != "" {
			errs["container"] = validation.NewError("validation_invalid_target", problem)
		} else if p.isSystemContainer(e.App, record.GetString("host"), name) {
			errs["container"] = validation.NewError("validation_system_container", "PocketCloud's own containers cannot be published.")
		}
		record.Set("redirect_to", "")
	case schema.TargetRedirect:
		target, err := url.Parse(record.GetString("redirect_to"))
		if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
			errs["redirect_to"] = validation.NewError("validation_invalid_redirect", "The redirect must go to an http or https address.")
		} else if strings.EqualFold(target.Hostname(), hostname) && target.Path == "" {
			errs["redirect_to"] = validation.NewError("validation_redirect_loop", "A domain cannot redirect to itself.")
		}
		record.Set("container", "")
		record.Set("port", 0)
	}

	if len(errs) > 0 {
		return errs
	}
	return e.Next()
}

// checkUpstream returns what is wrong with a container target, for the form.
func checkUpstream(name string, port int) string {
	if port < 1 || port > 65535 {
		return "Choose a port between 1 and 65535."
	}
	config := edge.Config{Routes: []edge.Route{{Hostname: "check.example.com", Kind: edge.KindProxy, Upstream: name + ":" + strconv.Itoa(port)}}}
	if name == "" || config.Validate() != nil {
		return "Name a container of this server, as Docker knows it."
	}
	return ""
}

func capitalize(text string) string {
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// isSystemContainer tells whether a container of the inventory belongs to
// PocketCloud itself.
func (p *Panel) isSystemContainer(app core.App, hostID, name string) bool {
	container, err := app.FindFirstRecordByFilter(schema.Containers, "host={:h} && name={:n}", dbx.Params{"h": hostID, "n": name})
	if err != nil {
		return false
	}
	switch container.GetString("system_role") {
	case "agent", "updater", "edge", "job":
		return true
	}
	return false
}

// guardEdgeRole keeps the edge role while domains still point to the server: the
// edge would go, and with it every one of them.
func (p *Panel) guardEdgeRole(e *core.RecordRequestEvent) error {
	stored, err := e.App.FindRecordById(schema.Hosts, e.Record.Id)
	if err == nil && hasEdgeRole(stored) && !hasEdgeRole(e.Record) {
		count, err := e.App.CountRecords(schema.Domains, dbx.HashExp{"host": e.Record.Id})
		if err == nil && count > 0 {
			return e.BadRequestError(fmt.Sprintf("%d domains are served by this server's edge: remove them or move them to another server first.", count), nil)
		}
	}
	return e.Next()
}

// certificateRecord is what the panel reads of a certificate to serve it.
type certificateRecord struct {
	record   *core.Record
	names    []string
	file     string
	notAfter time.Time
}

func (p *Panel) certificates(app core.App) ([]certificateRecord, error) {
	records, err := app.FindAllRecords(schema.Certificates)
	if err != nil {
		return nil, err
	}
	out := make([]certificateRecord, 0, len(records))
	for _, record := range records {
		var names []string
		if err := json.Unmarshal([]byte(record.GetString("names")), &names); err != nil {
			continue
		}
		file, err := edge.CertFileID(record.Id, []byte(record.GetString("chain_pem")))
		if err != nil {
			continue
		}
		out = append(out, certificateRecord{record: record, names: names, file: file, notAfter: record.GetDateTime("not_after").Time()})
	}
	return out, nil
}

// coveringCertificate picks the certificate for a name: one that has not
// expired, the one that lasts longest; an expired one only when there is no
// other, as a sudden switch to plain HTTP breaks sites that ask for HTTPS.
func coveringCertificate(certs []certificateRecord, hostname string) *certificateRecord {
	var best *certificateRecord
	for i := range certs {
		cert := &certs[i]
		if !edge.Covers(cert.names, hostname) {
			continue
		}
		if best == nil || cert.notAfter.After(best.notAfter) {
			best = cert
		}
	}
	return best
}

// edgeConfig is everything an edge serves, built from its domains.
type edgeConfig struct {
	config  edge.Config
	attach  []string
	certs   []certificateRecord
	domains []*core.Record
	// chosen maps a domain to its certificate
	chosen map[string]*certificateRecord
}

func (p *Panel) buildEdgeConfig(app core.App, host *core.Record) (edgeConfig, error) {
	domains, err := app.FindRecordsByFilter(schema.Domains, "host={:h}", "hostname", 0, 0, dbx.Params{"h": host.Id})
	if err != nil {
		return edgeConfig{}, err
	}
	certs, err := p.certificates(app)
	if err != nil {
		return edgeConfig{}, err
	}

	built := edgeConfig{domains: domains, chosen: map[string]*certificateRecord{}}
	used := map[string]bool{}

	for _, domain := range domains {
		route := edge.Route{
			Hostname: domain.GetString("hostname"),
			HSTS:     domain.GetBool("hsts"),
			Compress: domain.GetBool("compress"),
		}
		switch domain.GetString("target_kind") {
		case schema.TargetContainer:
			route.Kind = edge.KindProxy
			route.Upstream = domain.GetString("container") + ":" + strconv.Itoa(domain.GetInt("port"))
			if !slices.Contains(built.attach, domain.GetString("container")) {
				built.attach = append(built.attach, domain.GetString("container"))
			}
		case schema.TargetRedirect:
			route.Kind = edge.KindRedirect
			route.RedirectTo = domain.GetString("redirect_to")
			route.RedirectPermanent = domain.GetBool("redirect_permanent")
		default:
			continue
		}

		if domain.GetString("tls") != schema.TLSOff {
			if cert := coveringCertificate(certs, route.Hostname); cert != nil {
				route.Certificate = cert.file
				route.HTTPSRedirect = domain.GetBool("https_redirect")
				built.chosen[domain.Id] = cert
				if !used[cert.file] {
					used[cert.file] = true
					built.certs = append(built.certs, *cert)
					built.config.Certificates = append(built.config.Certificates, edge.Certificate{ID: cert.file})
				}
			}
		}

		built.config.Routes = append(built.config.Routes, route)
	}

	slices.Sort(built.attach)
	return built, nil
}

func (p *Panel) applyEdgeLogged(hostID string, force bool) {
	if p.ManualEdges.Load() {
		return
	}
	if _, err := p.applyEdge(hostID, force, ""); err != nil {
		p.Log.Error("failed to send the edge its config", "host", hostID, "error", err)
	}
}

// edgeCommands are the edge.apply commands of a host in a state.
func (p *Panel) edgeCommands(app core.App, hostID string, statuses ...string) ([]*core.Record, error) {
	filter := []string{}
	params := dbx.Params{"h": hostID, "t": "edge.apply"}
	for i, status := range statuses {
		key := "s" + strconv.Itoa(i)
		filter = append(filter, "status={:"+key+"}")
		params[key] = status
	}
	return app.FindRecordsByFilter(schema.Commands, "host={:h} && type={:t} && ("+strings.Join(filter, " || ")+")", "-created", 0, 0, params)
}

// commandDigest is the digest of the config an edge.apply carries.
func commandDigest(command *core.Record) string {
	var payload struct {
		CaddyJSON  string `json:"caddy_json"`
		CaddyImage string `json:"caddy_image"`
	}
	raw, _ := json.Marshal(command.Get("payload"))
	_ = json.Unmarshal(raw, &payload)
	if payload.CaddyJSON == "" {
		return ""
	}
	return edge.Digest(payload.CaddyImage, payload.CaddyJSON)
}

var errNoEdgeAgent = errors.New("the agent of this server cannot run an edge proxy yet: update it first")

// applyEdge sends a host's edge the config the panel wants it to serve, unless
// it already serves it, an apply of it is on its way, or the last one of it
// failed. force sends it anyway, as "Apply again" does. It returns the queued
// command, if any.
func (p *Panel) applyEdge(hostID string, force bool, requester string) (*core.Record, error) {
	lock := p.edgeLocks.of(hostID)
	lock.Lock()
	defer lock.Unlock()

	host, err := p.App.FindRecordById(schema.Hosts, hostID)
	if err != nil {
		return nil, err
	}

	if capabilities := host.GetStringSlice("capabilities"); len(capabilities) > 0 && !slices.Contains(capabilities, "edge.apply") {
		if hasEdgeRole(host) {
			return nil, errNoEdgeAgent
		}
		return nil, nil
	}

	payload := &agentv1.EdgeApply{}
	digest := ""
	var built edgeConfig

	if hasEdgeRole(host) {
		built, err = p.buildEdgeConfig(p.App, host)
		if err != nil {
			return nil, err
		}
		raw, err := edge.Build(built.config)
		if err != nil {
			return nil, fmt.Errorf("the edge config of %s: %w", host.GetString("name"), err)
		}
		payload.CaddyJson = string(raw)
		payload.CaddyImage = version.EdgeRef()
		payload.AttachContainers = built.attach
		digest = edge.Digest(payload.CaddyImage, payload.CaddyJson)
	} else if host.GetString("edge_applied_digest") == "" && host.GetString("edge_digest") == "" && !force {
		// no edge to remove
		return nil, nil
	}

	if digest != host.GetString("edge_digest") {
		p.setFields(host, map[string]any{"edge_digest": digest})
	}
	if !force && digest == host.GetString("edge_applied_digest") {
		return nil, nil
	}

	open, err := p.edgeCommands(p.App, hostID, schema.CommandQueued, schema.CommandDispatched, schema.CommandRunning)
	if err != nil {
		return nil, err
	}
	for _, command := range open {
		if commandDigest(command) == digest && !force {
			return nil, nil
		}
	}
	// the ones still waiting are replaced by this one
	for _, command := range open {
		if command.GetString("status") == schema.CommandQueued {
			if err := p.Commands.Cancel(command.Id, "replaced by a newer config of the edge"); err != nil {
				p.Log.Warn("failed to cancel an older edge.apply", "command", command.Id, "error", err)
			}
		}
	}

	if !force {
		finished, err := p.edgeCommands(p.App, hostID, schema.CommandSucceeded, schema.CommandFailed, schema.CommandTimedOut)
		if err != nil {
			return nil, err
		}
		// a config the edge refused is not sent again by itself
		if len(finished) > 0 && commandDigest(finished[0]) == digest && finished[0].GetString("status") != schema.CommandSucceeded {
			return nil, nil
		}
	}

	ctx := context.Background()
	for _, cert := range built.certs {
		if err := p.ensureCertificateOn(ctx, host, cert, requester); err != nil {
			return nil, err
		}
	}

	revision := host.GetInt("edge_revision") + 1
	payload.Revision = int64(revision)

	label := "Apply the edge config (revision " + strconv.Itoa(revision) + ")"
	if payload.CaddyJson == "" {
		label = "Remove the edge proxy"
	}

	command, err := p.Commands.Create(ctx, commands.Options{
		Host:      hostID,
		Command:   &agentv1.Command{Payload: &agentv1.Command_EdgeApply{EdgeApply: payload}},
		Label:     label,
		Requester: requester,
	})
	if err != nil {
		return nil, err
	}
	p.setFields(host, map[string]any{"edge_revision": revision})

	return command, nil
}

// ensureCertificateOn queues the installation of a certificate on a host, unless
// that exact version is there or on its way. It runs before the edge.apply that
// uses it: both are ordered by the edge's resource.
func (p *Panel) ensureCertificateOn(ctx context.Context, host *core.Record, cert certificateRecord, requester string) error {
	installed := installedOf(cert.record)
	if installed[host.Id] == cert.file {
		return nil
	}

	pending, err := p.App.FindRecordsByFilter(schema.Commands,
		"host={:h} && type='cert.install' && (status='queued' || status='dispatched' || status='running')", "", 0, 0, dbx.Params{"h": host.Id})
	if err != nil {
		return err
	}
	for _, command := range pending {
		if strings.Contains(fmt.Sprint(command.Get("payload")), cert.record.Id) {
			return nil
		}
	}

	var names []string
	_ = json.Unmarshal([]byte(cert.record.GetString("names")), &names)

	_, err = p.Commands.Create(ctx, commands.Options{
		Host: host.Id,
		Command: &agentv1.Command{Payload: &agentv1.Command_CertInstall{CertInstall: &agentv1.CertInstall{
			CertificateId: cert.record.Id, Names: names,
		}}},
		Label:     "Install the certificate for " + strings.Join(names, ", "),
		Requester: requester,
	})
	return err
}

// enrichCertInstall adds the chain and the key when the command is delivered, so
// the key is never stored in the command.
func (p *Panel) enrichCertInstall(_ *core.Record, command *agentv1.Command) error {
	payload := command.GetCertInstall()
	cert, err := p.App.FindRecordById(schema.Certificates, payload.GetCertificateId())
	if err != nil {
		return fmt.Errorf("the certificate is gone: %w", err)
	}
	key, err := p.Box.OpenString(cert.GetString("key_pem"))
	if err != nil {
		return fmt.Errorf("the certificate's key cannot be opened: %w", err)
	}
	payload.ChainPem = []byte(cert.GetString("chain_pem"))
	payload.KeyPem = []byte(key)
	return nil
}

func (p *Panel) edgeApplyFinished(command *core.Record) {
	host, err := p.App.FindRecordById(schema.Hosts, command.GetString("host"))
	if err != nil {
		return
	}
	switch command.GetString("status") {
	case schema.CommandSucceeded:
		var payload struct {
			Revision any `json:"revision"`
		}
		raw, _ := json.Marshal(command.Get("payload"))
		_ = json.Unmarshal(raw, &payload)
		revision, _ := strconv.Atoi(fmt.Sprint(payload.Revision))
		p.setFields(host, map[string]any{
			"edge_applied_digest": commandDigest(command), "edge_applied_revision": revision, "edge_error": "",
		})
	case schema.CommandFailed, schema.CommandTimedOut:
		p.setFields(host, map[string]any{"edge_error": command.GetString("error")})
	}
}

func (p *Panel) certificateOf(command *core.Record) (*core.Record, string) {
	var payload struct {
		ID string `json:"certificate_id"`
	}
	raw, _ := json.Marshal(command.Get("payload"))
	_ = json.Unmarshal(raw, &payload)
	cert, err := p.App.FindRecordById(schema.Certificates, payload.ID)
	if err != nil {
		return nil, payload.ID
	}
	return cert, payload.ID
}

// installedOf reads where a certificate is installed: {host: file}.
func installedOf(cert *core.Record) map[string]string {
	installed := map[string]string{}
	_ = json.Unmarshal([]byte(cert.GetString("installed")), &installed)
	if installed == nil {
		// a field without a value is null, which empties the map
		installed = map[string]string{}
	}
	return installed
}

func (p *Panel) setInstalled(cert *core.Record, hostID, file string) {
	err := p.update(context.Background(), cert, func(fresh *core.Record) error {
		installed := installedOf(fresh)
		if file == "" {
			delete(installed, hostID)
		} else {
			installed[hostID] = file
		}
		fresh.Set("installed", installed)
		return nil
	})
	if err != nil {
		p.Log.Error("failed to record where a certificate is installed", "certificate", cert.Id, "error", err)
	}
}

func (p *Panel) certInstallFinished(command *core.Record) {
	cert, _ := p.certificateOf(command)
	if cert == nil {
		return
	}
	if command.GetString("status") != schema.CommandSucceeded {
		p.setFields(cert, map[string]any{"last_error": command.GetString("error")})
		return
	}
	var result struct {
		File string `json:"file"`
	}
	raw, _ := json.Marshal(command.Get("result"))
	_ = json.Unmarshal(raw, &result)
	p.setInstalled(cert, command.GetString("host"), result.File)
}

func (p *Panel) certRemoveFinished(command *core.Record) {
	if command.GetString("status") != schema.CommandSucceeded {
		return
	}
	if cert, _ := p.certificateOf(command); cert != nil {
		p.setInstalled(cert, command.GetString("host"), "")
	}
}

// onEdgeStatus follows a heartbeat: an edge that serves something else than the
// panel wants gets the config again, as after a reinstall or a command that
// expired while the server was away.
func (p *Panel) onEdgeStatus(hostID string) {
	if p.ManualEdges.Load() {
		return
	}
	host, err := p.App.FindRecordById(schema.Hosts, hostID)
	if err != nil {
		return
	}
	wanted, applied := host.GetString("edge_digest"), host.GetString("edge_applied_digest")
	if hasEdgeRole(host) && (wanted == "" || wanted != applied) || !hasEdgeRole(host) && applied != "" {
		p.later.after(edgeSettle, func() { p.applyEdgeLogged(hostID, false) })
	}
}

// markExpiredCertificates keeps the state of certificates up to date for the
// pages; an edge keeps serving an expired one until there is another.
func (p *Panel) markExpiredCertificates() {
	now := types.NowDateTime()
	records, err := p.App.FindRecordsByFilter(schema.Certificates, "status!='expired' && not_after!='' && not_after<{:now}", "", 0, 0, dbx.Params{"now": now.String()})
	if err != nil {
		p.Log.Error("failed to look for expired certificates", "error", err)
		return
	}
	for _, record := range records {
		p.setFields(record, map[string]any{"status": schema.CertExpired})
	}
}

// edgeRoute is one route of an edge as the server page shows it.
type edgeRoute struct {
	ID            string          `json:"id"`
	Hostname      string          `json:"hostname"`
	TargetKind    string          `json:"target_kind"`
	Target        string          `json:"target"`
	TLS           string          `json:"tls"`
	Certificate   *edgeCertBrief  `json:"certificate,omitempty"`
	HTTPSRedirect bool            `json:"https_redirect"`
	HSTS          bool            `json:"hsts"`
	Compress      bool            `json:"compress"`
	Domain        json.RawMessage `json:"-"`
}

type edgeCertBrief struct {
	ID       string   `json:"id"`
	Names    []string `json:"names"`
	Issuer   string   `json:"issuer"`
	NotAfter string   `json:"not_after"`
}

// TLS states of a route.
const (
	tlsValid     = "valid"
	tlsExpiring  = "expiring"
	tlsExpired   = "expired"
	tlsInstall   = "installing"
	tlsNone      = "none"
	tlsTurnedOff = "off"
)

func (p *Panel) handleEdgeState(e *core.RequestEvent) error {
	host, err := e.App.FindRecordById(schema.Hosts, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The server does not exist.", err)
	}

	built, err := p.buildEdgeConfig(e.App, host)
	if err != nil {
		return e.InternalServerError("Failed to read the edge's routes.", err)
	}

	routes := make([]edgeRoute, 0, len(built.domains))
	for _, domain := range built.domains {
		route := edgeRoute{
			ID:            domain.Id,
			Hostname:      domain.GetString("hostname"),
			TargetKind:    domain.GetString("target_kind"),
			HTTPSRedirect: domain.GetBool("https_redirect"),
			HSTS:          domain.GetBool("hsts"),
			Compress:      domain.GetBool("compress"),
		}
		if route.TargetKind == schema.TargetContainer {
			route.Target = domain.GetString("container") + ":" + strconv.Itoa(domain.GetInt("port"))
		} else {
			route.Target = domain.GetString("redirect_to")
		}

		cert := built.chosen[domain.Id]
		switch {
		case domain.GetString("tls") == schema.TLSOff:
			route.TLS = tlsTurnedOff
		case cert == nil:
			route.TLS = tlsNone
		default:
			installed := installedOf(cert.record)
			switch {
			case installed[host.Id] != cert.file:
				route.TLS = tlsInstall
			case time.Now().After(cert.notAfter):
				route.TLS = tlsExpired
			case time.Until(cert.notAfter) < expiringWithin:
				route.TLS = tlsExpiring
			default:
				route.TLS = tlsValid
			}
			route.Certificate = &edgeCertBrief{
				ID: cert.record.Id, Names: cert.names, Issuer: cert.record.GetString("issuer"),
				NotAfter: cert.notAfter.UTC().Format(time.RFC3339),
			}
		}
		routes = append(routes, route)
	}

	open, err := p.edgeCommands(e.App, host.Id, schema.CommandQueued, schema.CommandDispatched, schema.CommandRunning)
	if err != nil {
		return e.InternalServerError("Failed to read the edge's commands.", err)
	}
	var last map[string]any
	if finished, err := p.edgeCommands(e.App, host.Id, schema.CommandSucceeded, schema.CommandFailed, schema.CommandTimedOut, schema.CommandCancelled); err == nil && len(finished) > 0 {
		last = map[string]any{"id": finished[0].Id, "status": finished[0].GetString("status"), "error": finished[0].GetString("error")}
	}

	wanted, applied := host.GetString("edge_digest"), host.GetString("edge_applied_digest")
	return e.JSON(http.StatusOK, map[string]any{
		"routes": routes,
		"edge": map[string]any{
			"role":             hasEdgeRole(host),
			"in_sync":          wanted != "" && wanted == applied,
			"applying":         len(open) > 0,
			"running":          host.GetBool("edge_running"),
			"error":            host.GetString("edge_error"),
			"revision":         host.GetInt("edge_applied_revision"),
			"caddy_version":    host.GetString("edge_caddy_version"),
			"requests_per_s":   host.GetFloat("edge_requests_per_s"),
			"image_version":    version.EdgeVersion,
			"last_apply":       last,
			"routes_with_tls":  countTLS(routes),
			"agent_has_edge":   slices.Contains(host.GetStringSlice("capabilities"), "edge.apply"),
			"applied_revision": host.GetInt("edge_applied_revision"),
		},
	})
}

func countTLS(routes []edgeRoute) int {
	count := 0
	for _, route := range routes {
		if route.Certificate != nil {
			count++
		}
	}
	return count
}

func (p *Panel) handleEdgeApply(e *core.RequestEvent) error {
	host, err := e.App.FindRecordById(schema.Hosts, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The server does not exist.", err)
	}
	command, err := p.applyEdge(host.Id, true, requesterOf(e))
	if errors.Is(err, errNoEdgeAgent) {
		return e.BadRequestError(capitalize(err.Error())+".", nil)
	}
	if err != nil {
		return e.BadRequestError("The edge config could not be built: "+err.Error(), nil)
	}
	if command == nil {
		return e.JSON(http.StatusOK, map[string]any{"command": nil})
	}
	return e.JSON(http.StatusAccepted, map[string]any{"command": command.Id})
}

func requesterOf(e *core.RequestEvent) string {
	if e.Auth == nil {
		return ""
	}
	if e.HasSuperuserAuth() {
		return "superuser " + e.Auth.Email()
	}
	return e.Auth.Email()
}

// parsedCertificate is an uploaded certificate, checked.
type parsedCertificate struct {
	chain       []byte
	key         []byte
	names       []string
	notBefore   time.Time
	notAfter    time.Time
	fingerprint string
	subject     string
	issuer      string
}

// parseCertificate checks an uploaded chain and key: they belong together, the
// certificate names hostnames and has not expired.
func parseCertificate(chainPEM, keyPEM string) (*parsedCertificate, error) {
	pair, err := tls.X509KeyPair([]byte(chainPEM), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("the certificate and the key do not fit together: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	if time.Now().After(leaf.NotAfter) {
		return nil, fmt.Errorf("the certificate expired on %s", leaf.NotAfter.UTC().Format("2006-01-02"))
	}

	names := []string{}
	for _, name := range leaf.DNSNames {
		name = strings.ToLower(name)
		if edge.ValidHostname(name) == nil && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, errors.New("the certificate names no hostname (subject alternative names)")
	}

	var chain strings.Builder
	for _, der := range pair.Certificate {
		_ = pem.Encode(&chain, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("the key cannot be stored: %w", err)
	}

	sum := sha256.Sum256(leaf.Raw)
	return &parsedCertificate{
		chain:       []byte(chain.String()),
		key:         pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		names:       names,
		notBefore:   leaf.NotBefore,
		notAfter:    leaf.NotAfter,
		fingerprint: hex.EncodeToString(sum[:]),
		subject:     leaf.Subject.String(),
		issuer:      leaf.Issuer.String(),
	}, nil
}

func (p *Panel) handleCertificateUpload(e *core.RequestEvent) error {
	var body struct {
		Chain string `json:"chain_pem"`
		Key   string `json:"key_pem"`
		Note  string `json:"note"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid request.", err)
	}

	parsed, err := parseCertificate(body.Chain, body.Key)
	if err != nil {
		return e.BadRequestError(capitalize(err.Error())+".", nil)
	}

	if existing, err := e.App.FindFirstRecordByData(schema.Certificates, "fingerprint", parsed.fingerprint); err == nil {
		return e.Error(http.StatusConflict, "This certificate is already uploaded.", map[string]any{"id": existing.Id})
	}

	collection, err := e.App.FindCachedCollectionByNameOrId(schema.Certificates)
	if err != nil {
		return e.InternalServerError("", err)
	}
	record := core.NewRecord(collection)
	record.Set("names", parsed.names)
	record.Set("issuer", schema.IssuerManual)
	record.Set("not_before", parsed.notBefore.UTC())
	record.Set("not_after", parsed.notAfter.UTC())
	record.Set("status", schema.CertActive)
	record.Set("chain_pem", string(parsed.chain))
	record.Set("key_pem", string(parsed.key))
	record.Set("fingerprint", parsed.fingerprint)
	record.Set("subject", truncateText(parsed.subject, 512))
	record.Set("issuer_name", truncateText(parsed.issuer, 512))
	record.Set("note", truncateText(strings.TrimSpace(body.Note), 512))

	if err := e.App.SaveWithContext(audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e)), record); err != nil {
		return e.BadRequestError("Failed to store the certificate.", err)
	}

	p.applyEdgesCovering(parsed.names, requesterOf(e))

	return e.JSON(http.StatusCreated, map[string]any{
		"id": record.Id, "names": parsed.names, "not_after": parsed.notAfter.UTC().Format(time.RFC3339),
	})
}

func truncateText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}

// applyEdgesCovering sends a new config to every edge with a domain the names cover.
func (p *Panel) applyEdgesCovering(names []string, requester string) {
	domains, err := p.App.FindAllRecords(schema.Domains)
	if err != nil {
		p.Log.Error("failed to read the domains", "error", err)
		return
	}
	hosts := []string{}
	for _, domain := range domains {
		if edge.Covers(names, domain.GetString("hostname")) && !slices.Contains(hosts, domain.GetString("host")) {
			hosts = append(hosts, domain.GetString("host"))
		}
	}
	for _, host := range hosts {
		if _, err := p.applyEdge(host, false, requester); err != nil {
			p.Log.Error("failed to send the edge its config", "host", host, "error", err)
		}
	}
}

// handleCertificateRemove takes a certificate off every edge and then removes
// it: first a config without it, then its files.
func (p *Panel) handleCertificateRemove(e *core.RequestEvent) error {
	cert, err := e.App.FindRecordById(schema.Certificates, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The certificate does not exist.", err)
	}

	installed := installedOf(cert)

	if err := e.App.DeleteWithContext(audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e)), cert); err != nil {
		return e.BadRequestError("Failed to remove the certificate.", err)
	}

	requester := requesterOf(e)
	for hostID := range installed {
		if _, err := p.applyEdge(hostID, false, requester); err != nil {
			p.Log.Error("failed to send the edge its config", "host", hostID, "error", err)
		}
		_, err := p.Commands.Create(context.Background(), commands.Options{
			Host:      hostID,
			Command:   &agentv1.Command{Payload: &agentv1.Command_CertRemove{CertRemove: &agentv1.CertRemove{CertificateId: cert.Id}}},
			Label:     "Remove a certificate from the edge",
			Requester: requester,
		})
		if err != nil {
			p.Log.Error("failed to queue the removal of a certificate", "host", hostID, "error", err)
		}
	}

	return e.NoContent(http.StatusNoContent)
}
