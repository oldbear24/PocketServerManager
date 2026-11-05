package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/dns"
	"github.com/oldbear24/PocketServerManager/internal/edge"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// DNS providers and the records of domains (#22). A provider is connected
// with credentials, which are sealed; its zones are listed by its API, or
// named by hand when the API cannot list them. A domain whose name falls in a
// managed zone gets its A and AAAA records there, pointing at its server's
// public addresses; one in no zone, or in a zone of a manual provider, is in
// manual mode: the page shows the records and the panel checks until they
// resolve. All of it runs in the panel; no agent takes part.

const dnsTimeout = 30 * time.Second

func (p *Panel) registerDNS() {
	p.App.OnRecordAfterCreateSuccess(schema.Domains).BindFunc(func(e *core.RecordEvent) error {
		id := e.Record.Id
		p.later.after(edgeSettle, func() { p.syncDomainDNSLogged(id) })
		return e.Next()
	})
	p.App.OnRecordAfterUpdateSuccess(schema.Domains).BindFunc(func(e *core.RecordEvent) error {
		// the panel's own saves of the DNS state change neither
		if original := e.Record.Original(); original != nil &&
			(original.GetString("hostname") != e.Record.GetString("hostname") || original.GetString("host") != e.Record.GetString("host") ||
				original.GetString("dns_alias") != e.Record.GetString("dns_alias")) {
			id := e.Record.Id
			p.later.after(edgeSettle, func() { p.syncDomainDNSLogged(id) })
		}
		return e.Next()
	})
	// an alias goes with the domain it is an alias of: its name would point
	// at nothing
	p.App.OnRecordDelete(schema.Domains).BindFunc(func(e *core.RecordEvent) error {
		aliases, err := e.App.FindRecordsByFilter(schema.Domains, "dns_alias={:name}", "", 0, 0,
			dbx.Params{"name": e.Record.GetString("hostname")})
		if err != nil {
			return err
		}
		for _, alias := range aliases {
			// by whoever removed the domain
			if err := e.App.DeleteWithContext(e.Context, alias); err != nil {
				return err
			}
		}
		return e.Next()
	})
	// the records of a removed domain go too. The rows are read before the
	// delete clears their relation to it, and removed under the domain's lock
	// once a sync under way is done; a row another domain took over stays.
	p.App.OnRecordDelete(schema.Domains).BindFunc(func(e *core.RecordEvent) error {
		tracked, _ := e.App.FindRecordsByFilter(schema.DNSRecords, "domain={:d}", "", 0, 0, dbx.Params{"d": e.Record.Id})
		if err := e.Next(); err != nil {
			return err
		}
		ids := make([]string, 0, len(tracked))
		for _, row := range tracked {
			ids = append(ids, row.Id)
		}
		if len(ids) > 0 {
			domainID, hostname, host := e.Record.Id, e.Record.GetString("hostname"), e.Record.GetString("host")
			p.later.after(0, func() { p.removeDomainRecords(domainID, hostname, host, ids) })
		}
		return nil
	})
	// a server's public addresses moved: its domains follow. The stored record
	// is read before the save, as a record object that was saved before may
	// carry an original that is not what is stored.
	p.App.OnRecordUpdate(schema.Hosts).BindFunc(func(e *core.RecordEvent) error {
		stored, err := e.App.FindRecordById(schema.Hosts, e.Record.Id)
		moved := err == nil && (stored.GetString("public_ip4") != e.Record.GetString("public_ip4") ||
			stored.GetString("public_ip6") != e.Record.GetString("public_ip6") ||
			// the addresses on its network, for the zones of a LAN
			stored.GetString("ip4") != e.Record.GetString("ip4") || stored.GetString("ip6") != e.Record.GetString("ip6"))
		if err := e.Next(); err != nil {
			return err
		}
		if moved {
			id := e.Record.Id
			p.later.after(edgeSettle, func() { p.syncHostDNS(id) })
		}
		return nil
	})
	p.App.OnRecordValidate(schema.Hosts).BindFunc(p.validatePublicAddresses)
	p.Commands.RegisterEnricher("dns.rewrite", p.enrichDNSRewrite)

	p.App.Cron().MustAdd("pocketcloud_dns_propagation", "* * * * *", p.CheckDNSPropagation)
	p.App.Cron().MustAdd("pocketcloud_dns_drift", "*/15 * * * *", p.CheckDNSDrift)
}

func (p *Panel) bindDNSRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	admins := requireRole(schema.RoleAdmin)
	operators := requireRole(schema.RoleAdmin, schema.RoleOperator)

	group.GET("/dns/kinds", func(e *core.RequestEvent) error { return e.JSON(http.StatusOK, dns.Drivers()) })
	group.POST("/dns-providers", p.handleConnectProvider).BindFunc(admins)
	group.PATCH("/dns-providers/{id}", p.handleChangeProvider).BindFunc(admins)
	group.POST("/dns-providers/{id}/test", p.handleTestProvider).BindFunc(admins)
	group.DELETE("/dns-providers/{id}", p.handleRemoveProvider).BindFunc(admins)
	group.POST("/dns-zones", p.handleAddZone).BindFunc(admins)
	group.PATCH("/dns-zones/{id}", p.handleChangeZone).BindFunc(admins)
	group.DELETE("/dns-zones/{id}", p.handleRemoveZone).BindFunc(admins)
	group.GET("/domains/{id}/dns", p.handleDomainDNS)
	group.POST("/domains/{id}/dns/sync", p.handleSyncDomainDNS).BindFunc(operators)
}

// checker is what asks DNS servers; tests replace it.
func (p *Panel) checker() *dns.Checker {
	if c := p.DNSChecker.Load(); c != nil {
		return c
	}
	return dns.NewChecker()
}

// SetDNSChecker replaces what the panel asks whether records have propagated.
func (p *Panel) SetDNSChecker(c *dns.Checker) {
	p.DNSChecker.Store(c)
}

func (p *Panel) validatePublicAddresses(e *core.RecordEvent) error {
	errs := validation.Errors{}
	for field, kind := range map[string]string{"public_ip4": "A", "public_ip6": "AAAA"} {
		value := strings.TrimSpace(e.Record.GetString(field))
		if value == "" {
			continue
		}
		records, err := dns.AddressRecords("host.invalid", "invalid", value, "")
		switch {
		case err != nil:
			errs[field] = validation.NewError("validation_invalid_address", capitalize(err.Error()))
		case records[0].Type != kind:
			errs[field] = validation.NewError("validation_invalid_address", fmt.Sprintf("The address %s is not an IPv%s address.", value, map[string]string{"A": "4", "AAAA": "6"}[kind]))
		}
	}
	if len(errs) > 0 {
		return errs
	}
	return e.Next()
}

// credentialsOf opens a provider's sealed credentials.
func (p *Panel) credentialsOf(provider *core.Record) (map[string]string, error) {
	sealed := provider.GetString("credentials")
	if sealed == "" {
		return map[string]string{}, nil
	}
	plain, err := p.Box.OpenString(sealed)
	if err != nil {
		return nil, fmt.Errorf("the credentials cannot be opened: %w", err)
	}
	credentials := map[string]string{}
	if err := json.Unmarshal([]byte(plain), &credentials); err != nil {
		return nil, err
	}
	return credentials, nil
}

func (p *Panel) openProvider(provider *core.Record) (dns.Driver, dns.Provider, error) {
	driver, ok := dns.Lookup(provider.GetString("kind"))
	if !ok {
		return dns.Driver{}, nil, fmt.Errorf("PocketCloud does not know the provider kind %q", provider.GetString("kind"))
	}
	if driver.Manual {
		return driver, nil, dns.ErrManual
	}
	if driver.Agent {
		run, err := p.rewriter(provider, driver)
		if err != nil {
			return driver, nil, err
		}
		return driver, dns.OpenRewrites(run), nil
	}
	credentials, err := p.credentialsOf(provider)
	if err != nil {
		return driver, nil, err
	}
	client, err := driver.Open(credentials)
	return driver, client, err
}

type providerBody struct {
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	Credentials map[string]string `json:"credentials"`
	// Zones are named by hand for a kind whose API cannot list them.
	Zones []string `json:"zones"`
	// Host is the server whose agent reaches a DNS server of a LAN.
	Host *string `json:"host"`
}

func (p *Panel) handleConnectProvider(e *core.RequestEvent) error {
	var body providerBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		return e.BadRequestError("Give the provider a name.", nil)
	}
	driver, ok := dns.Lookup(body.Kind)
	if !ok {
		return e.BadRequestError(fmt.Sprintf("Unknown provider kind %q.", body.Kind), nil)
	}
	if body.Credentials == nil {
		body.Credentials = map[string]string{}
	}
	if err := driver.CheckCredentials(body.Credentials); err != nil {
		return e.BadRequestError("Credentials: "+err.Error()+".", nil)
	}
	host := ""
	if driver.Agent {
		if body.Host != nil {
			host = *body.Host
		}
		if err := p.checkLANProvider(body.Credentials, host); err != nil {
			return e.BadRequestError(capitalize(err.Error())+".", nil)
		}
	}
	if driver.ListsZones {
		// its API lists them
		body.Zones = nil
	} else if len(body.Zones) == 0 {
		return e.BadRequestError(driver.Label+" cannot list its zones: name at least one.", nil)
	}
	for i, name := range body.Zones {
		zone, err := p.checkZoneName(name, "", driver.Agent)
		if err != nil {
			return e.BadRequestError(err.Error(), nil)
		}
		body.Zones[i] = zone
	}
	if _, err := p.App.FindFirstRecordByData(schema.DNSProviders, "name", body.Name); err == nil {
		return e.BadRequestError(fmt.Sprintf("A provider named %q exists already.", body.Name), nil)
	}

	collection, err := p.App.FindCollectionByNameOrId(schema.DNSProviders)
	if err != nil {
		return e.InternalServerError("", err)
	}
	provider := core.NewRecord(collection)
	provider.Set("name", body.Name)
	provider.Set("kind", body.Kind)
	provider.Set("host", host)
	provider.Set("status", schema.DNSUnknown)
	if len(body.Credentials) > 0 {
		raw, _ := json.Marshal(body.Credentials)
		sealed, err := p.Box.SealString(string(raw))
		if err != nil {
			return e.InternalServerError("", err)
		}
		provider.Set("credentials", sealed)
	}
	if err := p.App.Save(provider); err != nil {
		return e.BadRequestError("The provider could not be saved: "+err.Error(), nil)
	}

	for _, name := range body.Zones {
		if _, err := p.addZone(provider, name); err != nil {
			return e.BadRequestError(err.Error(), nil)
		}
	}

	ctx, cancel := context.WithTimeout(e.Request.Context(), dnsTimeout)
	defer cancel()
	p.testProvider(ctx, provider)
	return p.providerResponse(e, provider.Id)
}

func (p *Panel) handleChangeProvider(e *core.RequestEvent) error {
	provider, err := p.App.FindRecordById(schema.DNSProviders, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The provider does not exist.", err)
	}
	var body providerBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}
	if name := strings.TrimSpace(body.Name); name != "" {
		provider.Set("name", name)
	}
	driver, _ := dns.Lookup(provider.GetString("kind"))
	if driver.Agent && (body.Host != nil || body.Credentials != nil) {
		host := provider.GetString("host")
		if body.Host != nil {
			host = *body.Host
		}
		credentials := body.Credentials
		if credentials == nil {
			if credentials, err = p.credentialsOf(provider); err != nil {
				return e.InternalServerError("", err)
			}
		}
		if err := p.checkLANProvider(credentials, host); err != nil {
			return e.BadRequestError(capitalize(err.Error())+".", nil)
		}
		provider.Set("host", host)
	}
	if body.Credentials != nil {
		if err := driver.CheckCredentials(body.Credentials); err != nil {
			return e.BadRequestError("Credentials: "+err.Error()+".", nil)
		}
		raw, _ := json.Marshal(body.Credentials)
		sealed, err := p.Box.SealString(string(raw))
		if err != nil {
			return e.InternalServerError("", err)
		}
		provider.Set("credentials", sealed)
	}
	if err := p.App.Save(provider); err != nil {
		return e.BadRequestError("The provider could not be saved: "+err.Error(), nil)
	}
	ctx, cancel := context.WithTimeout(e.Request.Context(), dnsTimeout)
	defer cancel()
	p.testProvider(ctx, provider)
	return p.providerResponse(e, provider.Id)
}

func (p *Panel) handleTestProvider(e *core.RequestEvent) error {
	provider, err := p.App.FindRecordById(schema.DNSProviders, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The provider does not exist.", err)
	}
	ctx, cancel := context.WithTimeout(e.Request.Context(), dnsTimeout)
	defer cancel()
	p.testProvider(ctx, provider)
	return p.providerResponse(e, provider.Id)
}

func (p *Panel) providerResponse(e *core.RequestEvent, id string) error {
	provider, err := p.App.FindRecordById(schema.DNSProviders, id)
	if err != nil {
		return e.NotFoundError("The provider does not exist.", err)
	}
	zones, err := p.App.FindRecordsByFilter(schema.DNSZones, "provider={:p}", "name", 0, 0, dbx.Params{"p": id})
	if err != nil {
		return e.InternalServerError("", err)
	}
	return e.JSON(http.StatusOK, map[string]any{"provider": provider, "zones": zones})
}

// testProvider asks a provider's API what it can: the zones of an API that
// lists them, otherwise the records of each zone named. A manual provider
// has nothing to test.
func (p *Panel) testProvider(ctx context.Context, provider *core.Record) {
	now := types.NowDateTime()
	driver, client, err := p.openProvider(provider)
	if errors.Is(err, dns.ErrManual) {
		p.setFields(provider, map[string]any{"status": schema.DNSOK, "last_error": "", "last_check": now})
		return
	}
	if err != nil {
		p.setFields(provider, map[string]any{"status": schema.DNSError, "last_error": err.Error(), "last_check": now})
		return
	}

	zones, _ := p.App.FindRecordsByFilter(schema.DNSZones, "provider={:p}", "", 0, 0, dbx.Params{"p": provider.Id})

	if driver.ListsZones {
		listed, err := dns.ListZones(ctx, client)
		if err != nil {
			p.setFields(provider, map[string]any{"status": schema.DNSError, "last_error": driver.Explain(err), "last_check": now})
			return
		}
		known := map[string]*core.Record{}
		for _, zone := range zones {
			known[zone.GetString("name")] = zone
		}
		for _, name := range listed {
			zone, ok := known[name]
			if !ok {
				var err error
				if zone, err = p.addZone(provider, name); err != nil {
					// the zone is known from another provider; that one keeps it
					p.Log.Warn("a listed zone was not added", "zone", name, "error", err)
					continue
				}
			}
			p.setFields(zone, map[string]any{"status": schema.DNSOK, "last_error": "", "last_check": now})
			delete(known, name)
		}
		for name, zone := range known {
			p.setFields(zone, map[string]any{"status": schema.DNSError, "last_error": driver.Label + " no longer lists " + name + " for these credentials.", "last_check": now})
		}
		p.setFields(provider, map[string]any{"status": schema.DNSOK, "last_error": "", "last_check": now})
		p.syncAllDomainsLater()
		return
	}

	failed := ""
	for _, zone := range zones {
		if err := dns.Check(ctx, client, zone.GetString("name")); err != nil {
			text := driver.Explain(err)
			p.setFields(zone, map[string]any{"status": schema.DNSError, "last_error": text, "last_check": now})
			if failed == "" {
				failed = zone.GetString("name") + ": " + text
			}
			continue
		}
		p.setFields(zone, map[string]any{"status": schema.DNSOK, "last_error": "", "last_check": now})
	}
	if failed != "" {
		p.setFields(provider, map[string]any{"status": schema.DNSError, "last_error": failed, "last_check": now})
		return
	}
	p.setFields(provider, map[string]any{"status": schema.DNSOK, "last_error": "", "last_check": now})
	p.syncAllDomainsLater()
}

// checkZoneName says whether a zone name can be added for a provider: a name
// such as example.com that no other provider holds.
func (p *Panel) checkZoneName(name, providerID string, lan bool) (string, error) {
	name = dns.NormalizeZone(name)
	valid := edge.ValidHostname(name) == nil
	if lan && !strings.Contains(name, ".") {
		// a LAN has zones of one label, such as lan or home
		valid = edge.ValidHostname(name+".x") == nil
	}
	if !valid || strings.HasPrefix(name, "*") || !lan && !strings.Contains(name, ".") {
		if lan {
			return "", fmt.Errorf("%q is not a zone such as lan or home.arpa", name)
		}
		return "", fmt.Errorf("%q is not a zone such as example.com", name)
	}
	if existing, err := p.App.FindFirstRecordByData(schema.DNSZones, "name", name); err == nil && existing.GetString("provider") != providerID {
		return "", fmt.Errorf("the zone %s belongs to another provider already", name)
	}
	return name, nil
}

func (p *Panel) addZone(provider *core.Record, name string) (*core.Record, error) {
	name, err := p.checkZoneName(name, provider.Id, lanDriver(provider))
	if err != nil {
		return nil, err
	}
	if existing, err := p.App.FindFirstRecordByData(schema.DNSZones, "name", name); err == nil {
		return existing, nil
	}
	collection, err := p.App.FindCollectionByNameOrId(schema.DNSZones)
	if err != nil {
		return nil, err
	}
	zone := core.NewRecord(collection)
	zone.Set("provider", provider.Id)
	zone.Set("name", name)
	zone.Set("managed", true)
	zone.Set("status", schema.DNSUnknown)
	if err := p.App.Save(zone); err != nil {
		return nil, err
	}
	return zone, nil
}

// trackedRecords counts the records PocketCloud manages in a provider's zones,
// or in one zone.
func (p *Panel) trackedRecords(providerID, zoneID string) (int, error) {
	var count struct {
		N int `db:"n"`
	}
	query := "SELECT count(*) AS n FROM dns_records r JOIN dns_zones z ON z.id = r.zone WHERE z.provider = {:p}"
	params := dbx.Params{"p": providerID}
	if zoneID != "" {
		query = "SELECT count(*) AS n FROM dns_records WHERE zone = {:z}"
		params = dbx.Params{"z": zoneID}
	}
	err := p.App.DB().NewQuery(query).Bind(params).One(&count)
	return count.N, err
}

func (p *Panel) handleRemoveProvider(e *core.RequestEvent) error {
	provider, err := p.App.FindRecordById(schema.DNSProviders, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The provider does not exist.", err)
	}
	count, err := p.trackedRecords(provider.Id, "")
	if err != nil {
		return e.InternalServerError("", err)
	}
	if count > 0 {
		return e.BadRequestError(fmt.Sprintf("PocketCloud manages %d records at this provider: stop managing its zones first, which keeps the records but forgets them, or remove their domains. Records of domains removed already are retried every 15 minutes.", count), nil)
	}
	if err := p.App.Delete(provider); err != nil {
		return e.InternalServerError("", err)
	}
	p.syncAllDomainsLater()
	return e.NoContent(http.StatusNoContent)
}

func (p *Panel) handleAddZone(e *core.RequestEvent) error {
	var body struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}
	provider, err := p.App.FindRecordById(schema.DNSProviders, body.Provider)
	if err != nil {
		return e.NotFoundError("The provider does not exist.", err)
	}
	zone, err := p.addZone(provider, body.Name)
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	ctx, cancel := context.WithTimeout(e.Request.Context(), dnsTimeout)
	defer cancel()
	p.testProvider(ctx, provider)
	return p.providerResponse(e, zone.GetString("provider"))
}

func (p *Panel) handleChangeZone(e *core.RequestEvent) error {
	zone, err := p.App.FindRecordById(schema.DNSZones, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The zone does not exist.", err)
	}
	var body struct {
		Managed *bool `json:"managed"`
	}
	if err := e.BindBody(&body); err != nil || body.Managed == nil {
		return e.BadRequestError("Say whether PocketCloud manages the zone.", err)
	}
	zone.Set("managed", *body.Managed)
	if err := p.App.Save(zone); err != nil {
		return e.InternalServerError("", err)
	}
	if !*body.Managed {
		// its records stay at the provider; PocketCloud forgets them
		if err := p.forgetZoneRecords(zone.Id); err != nil {
			return e.InternalServerError("", err)
		}
	}
	p.syncAllDomainsLater()
	return e.JSON(http.StatusOK, zone)
}

func (p *Panel) handleRemoveZone(e *core.RequestEvent) error {
	zone, err := p.App.FindRecordById(schema.DNSZones, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The zone does not exist.", err)
	}
	count, err := p.trackedRecords("", zone.Id)
	if err != nil {
		return e.InternalServerError("", err)
	}
	if count > 0 {
		return e.BadRequestError(fmt.Sprintf("PocketCloud manages %d records in %s: stop managing the zone first, which keeps the records but forgets them, or remove their domains.", count, zone.GetString("name")), nil)
	}
	if err := p.App.Delete(zone); err != nil {
		return e.InternalServerError("", err)
	}
	p.syncAllDomainsLater()
	return e.NoContent(http.StatusNoContent)
}

func (p *Panel) forgetZoneRecords(zoneID string) error {
	records, err := p.App.FindRecordsByFilter(schema.DNSRecords, "zone={:z}", "", 0, 0, dbx.Params{"z": zoneID})
	if err != nil {
		return err
	}
	for _, record := range records {
		if err := p.App.Delete(record); err != nil {
			return err
		}
	}
	return nil
}

// domainDNS is what a domain needs: its zone and provider, and its records.
type domainDNS struct {
	Zone     *core.Record
	Provider *core.Record
	ZoneName string
	Records  []dns.Record
	// Manual is true when the records are created by hand.
	Manual bool
	// Problem is why no records can be built, such as a missing address.
	Problem string
	// LAN is true for a zone of a DNS server of a LAN: it answers what it
	// took at once, and the world never sees it.
	LAN bool
}

func (p *Panel) desiredDNS(domain *core.Record) domainDNS {
	hostname := domain.GetString("hostname")
	var want domainDNS

	zones, _ := p.App.FindRecordsByFilter(schema.DNSZones, "", "", 0, 0)
	names := make([]string, 0, len(zones))
	byName := map[string]*core.Record{}
	for _, zone := range zones {
		names = append(names, zone.GetString("name"))
		byName[zone.GetString("name")] = zone
	}
	if name, ok := dns.ZoneFor(hostname, names); ok {
		want.Zone = byName[name]
		want.ZoneName = name
		if provider, err := p.App.FindRecordById(schema.DNSProviders, want.Zone.GetString("provider")); err == nil {
			want.Provider = provider
		}
	}
	want.Manual = want.Zone == nil || !want.Zone.GetBool("managed") || want.Provider == nil
	if want.Provider != nil {
		if driver, ok := dns.Lookup(want.Provider.GetString("kind")); !ok || driver.Manual {
			want.Manual = true
		}
	}

	zone := want.ZoneName
	if zone == "" {
		zone = hostname
	}
	// an alias points at the other name, wherever that one points
	if alias := domain.GetString("dns_alias"); alias != "" {
		want.Records = []dns.Record{dns.CNAMERecord(hostname, zone, alias)}
		return want
	}

	host, err := p.App.FindRecordById(schema.Hosts, domain.GetString("host"))
	if err != nil {
		want.Problem = "The domain's server does not exist."
		return want
	}
	if !want.Manual && lanDriver(want.Provider) {
		// a name of the LAN points at the server's address on its network,
		// as its agent reports it
		ip4, ip6 := host.GetString("ip4"), host.GetString("ip6")
		if ip4 == "" && ip6 == "" {
			want.Problem = fmt.Sprintf("The address of %s on its network is not known yet: its agent reports it once it connects.", host.GetString("name"))
			return want
		}
		records, err := dns.LANAddressRecords(hostname, zone, ip4, ip6)
		if err != nil {
			want.Problem = err.Error()
			return want
		}
		want.LAN = true
		want.Records = records
		return want
	}
	ip4, ip6 := host.GetString("public_ip4"), host.GetString("public_ip6")
	if ip4 == "" && ip6 == "" {
		want.Problem = fmt.Sprintf("Set the public IP address of %s on its server page, so its domains can point at it.", host.GetString("name"))
		return want
	}
	records, err := dns.AddressRecords(hostname, zone, ip4, ip6)
	if err != nil {
		want.Problem = err.Error()
		return want
	}
	want.Records = records
	return want
}

func (p *Panel) syncDomainDNSLogged(id string) {
	if err := p.syncDomainDNS(context.Background(), id, false); err != nil {
		p.Log.Warn("failed to sync a domain's DNS records", "domain", id, "error", err)
	}
}

// syncDomainDNS gives a domain's zone the records it needs, and removes the
// ones PocketCloud made for it before that it does not need any more. A
// domain whose records are all set already is left as it is, drift included,
// unless force asks to set them again, as the page's Sync does.
//
// The rows are saved before the provider is asked, so that a delete of the
// domain under way finds them; the new records are set before old ones go,
// so a moved name never has none, and an old zone that fails does not stop
// the records of the new one.
func (p *Panel) syncDomainDNS(ctx context.Context, id string, force bool) error {
	lock := p.dnsLocks.of(id)
	lock.Lock()
	defer lock.Unlock()

	domain, err := p.App.FindRecordById(schema.Domains, id)
	if err != nil {
		// removed meanwhile; its delete takes the records
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()

	want := p.desiredDNS(domain)
	zoneID := ""
	if want.Zone != nil {
		zoneID = want.Zone.Id
	}
	managed := !want.Manual && want.Problem == ""

	tracked, err := p.App.FindRecordsByFilter(schema.DNSRecords, "domain={:d}", "", 0, 0, dbx.Params{"d": id})
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	if managed {
		for _, r := range want.Records {
			keep[zoneID+" "+r.Key()] = true
		}
	}
	var stale []*core.Record
	for _, row := range tracked {
		if !keep[row.GetString("zone")+" "+rowRecord(row).Key()] {
			stale = append(stale, row)
		}
	}

	if !force && managed && len(stale) == 0 && domain.GetString("zone") == zoneID && len(tracked) == len(keep) {
		switch domain.GetString("dns_status") {
		case schema.DNSPending, schema.DNSOK, schema.DNSDrift:
			return nil
		}
	}

	if !managed {
		staleErr := p.removeRecords(ctx, stale)
		switch {
		case want.Problem != "":
			p.setFields(domain, map[string]any{"zone": zoneID, "dns_status": schema.DNSError, "dns_error": want.Problem})
		case staleErr != nil:
			return p.domainDNSFailed(domain, zoneID, staleErr)
		default:
			p.setFields(domain, map[string]any{"zone": zoneID, "dns_status": schema.DNSManual, "dns_error": ""})
		}
		return nil
	}

	driver, client, err := p.openProvider(want.Provider)
	if err != nil {
		return p.domainDNSFailed(domain, zoneID, err)
	}

	rows, err := p.trackRecords(id, zoneID, want.Records)
	if err != nil {
		// the domain went while this ran: nothing was set yet
		return nil
	}

	name := dns.RelativeName(domain.GetString("hostname"), want.ZoneName)
	if err := dns.Set(ctx, client, want.ZoneName, want.Records, dns.AddressSets(name)...); err != nil {
		text := driver.Explain(err)
		for _, row := range rows {
			p.setFields(row, map[string]any{"status": schema.DNSError, "last_error": text})
		}
		return p.domainDNSFailed(domain, zoneID, errors.New(text))
	}

	// the stale records of the same zone and name were replaced by the Set
	var elsewhere []*core.Record
	for _, row := range stale {
		if row.GetString("zone") == zoneID && strings.EqualFold(row.GetString("name"), name) &&
			(row.GetString("type") == "A" || row.GetString("type") == "AAAA") {
			if err := p.App.Delete(row); err != nil {
				return err
			}
			continue
		}
		elsewhere = append(elsewhere, row)
	}
	staleErr := p.removeRecords(ctx, elsewhere)

	if _, err := p.App.FindRecordById(schema.Domains, id); err != nil {
		// removed while its records were set: they go again
		fresh := make([]*core.Record, 0, len(rows))
		for _, row := range rows {
			if r, err := p.App.FindRecordById(schema.DNSRecords, row.Id); err == nil {
				fresh = append(fresh, r)
			}
		}
		return p.removeRecords(ctx, fresh)
	}
	if staleErr != nil {
		return p.domainDNSFailed(domain, zoneID, fmt.Errorf("the new records are set, but old ones could not be removed: %w", staleErr))
	}
	if want.LAN {
		// the server that took them answers them: nothing propagates
		now := types.NowDateTime()
		for _, row := range rows {
			p.setFields(row, map[string]any{"status": schema.DNSOK, "last_check": now})
		}
		p.setFields(domain, map[string]any{"zone": zoneID, "dns_status": schema.DNSOK, "dns_error": ""})
		return nil
	}
	p.setFields(domain, map[string]any{"zone": zoneID, "dns_status": schema.DNSPending, "dns_error": ""})
	return nil
}

// trackRecords saves the rows of a domain's records, pending, before they are set.
func (p *Panel) trackRecords(domainID, zoneID string, records []dns.Record) ([]*core.Record, error) {
	collection, err := p.App.FindCollectionByNameOrId(schema.DNSRecords)
	if err != nil {
		return nil, err
	}
	rows := make([]*core.Record, 0, len(records))
	for _, r := range records {
		row, err := p.App.FindFirstRecordByFilter(schema.DNSRecords, "zone={:z} && name={:n} && type={:t} && value={:v}",
			dbx.Params{"z": zoneID, "n": r.Name, "t": r.Type, "v": r.Value})
		if err != nil {
			row = core.NewRecord(collection)
		}
		row.Set("zone", zoneID)
		row.Set("domain", domainID)
		row.Set("name", r.Name)
		row.Set("type", r.Type)
		row.Set("value", r.Value)
		row.Set("ttl", r.TTL)
		row.Set("status", schema.DNSPending)
		row.Set("last_error", "")
		row.Set("drift", nil)
		if err := p.App.Save(row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (p *Panel) domainDNSFailed(domain *core.Record, zoneID string, err error) error {
	p.setFields(domain, map[string]any{"zone": zoneID, "dns_status": schema.DNSError, "dns_error": err.Error()})
	events.Log(p.App, domain.GetString("host"), "dns.failed", schema.SeverityWarning,
		fmt.Sprintf("The DNS records of %s could not be set: %s", domain.GetString("hostname"), err),
		map[string]any{"domain": domain.GetString("hostname"), "error": err.Error()})
	return err
}

func rowRecord(row *core.Record) dns.Record {
	return dns.Record{Name: row.GetString("name"), Type: row.GetString("type"), Value: row.GetString("value"), TTL: row.GetInt("ttl")}
}

// removeDomainRecords removes the records of a removed domain, under its
// lock: a sync under way finishes first. A row another domain took over
// meanwhile stays.
func (p *Panel) removeDomainRecords(domainID, hostname, host string, ids []string) {
	lock := p.dnsLocks.of(domainID)
	lock.Lock()
	defer lock.Unlock()

	var rows []*core.Record
	for _, id := range ids {
		row, err := p.App.FindRecordById(schema.DNSRecords, id)
		if err != nil || (row.GetString("domain") != "" && row.GetString("domain") != domainID) {
			continue
		}
		rows = append(rows, row)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dnsTimeout)
	defer cancel()
	if err := p.removeRecords(ctx, rows); err != nil {
		p.Log.Warn("failed to remove the DNS records of a removed domain", "domain", hostname, "error", err)
		events.Log(p.App, host, "dns.remove_failed", schema.SeverityWarning,
			fmt.Sprintf("The DNS records of the removed domain %s could not be removed: %s. PocketCloud tries again every 15 minutes.", hostname, err),
			map[string]any{"domain": hostname, "error": err.Error()})
	}
}

// removeRecords deletes records PocketCloud made, at their providers and here.
// A row whose zone is not managed any more is only forgotten.
func (p *Panel) removeRecords(ctx context.Context, rows []*core.Record) error {
	byZone := map[string][]*core.Record{}
	for _, row := range rows {
		byZone[row.GetString("zone")] = append(byZone[row.GetString("zone")], row)
	}
	for zoneID, zoneRows := range byZone {
		zone, err := p.App.FindRecordById(schema.DNSZones, zoneID)
		if err == nil && zone.GetBool("managed") {
			provider, err := p.App.FindRecordById(schema.DNSProviders, zone.GetString("provider"))
			if err != nil {
				return err
			}
			driver, client, err := p.openProvider(provider)
			if err == nil {
				records := make([]dns.Record, 0, len(zoneRows))
				for _, row := range zoneRows {
					records = append(records, rowRecord(row))
				}
				if err := dns.Delete(ctx, client, zone.GetString("name"), records); err != nil {
					for _, row := range zoneRows {
						p.setFields(row, map[string]any{"status": schema.DNSError, "last_error": driver.Explain(err)})
					}
					return errors.New(driver.Explain(err))
				}
			} else if !errors.Is(err, dns.ErrManual) {
				return err
			}
		}
		for _, row := range zoneRows {
			if err := p.App.Delete(row); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Panel) syncHostDNS(hostID string) {
	domains, err := p.App.FindRecordsByFilter(schema.Domains, "host={:h}", "", 0, 0, dbx.Params{"h": hostID})
	if err != nil {
		return
	}
	for _, domain := range domains {
		p.syncDomainDNSLogged(domain.Id)
	}
}

// syncAllDomainsLater follows a change of zones: a domain may move into a
// managed zone, or out of one.
func (p *Panel) syncAllDomainsLater() {
	p.later.after(edgeSettle, func() {
		domains, err := p.App.FindRecordsByFilter(schema.Domains, "", "", 0, 0)
		if err != nil {
			return
		}
		for _, domain := range domains {
			p.syncDomainDNSLogged(domain.Id)
		}
	})
}

// CheckDNSPropagation asks the DNS servers about the records of domains that
// wait for them: those just set, and those created by hand. A domain whose
// records could not be set is tried again every five minutes.
func (p *Panel) CheckDNSPropagation() {
	domains, err := p.App.FindRecordsByFilter(schema.Domains, "dns_status={:p} || dns_status={:m} || dns_status={:e}", "", 0, 0,
		dbx.Params{"p": schema.DNSPending, "m": schema.DNSManual, "e": schema.DNSError})
	if err != nil {
		return
	}
	for _, domain := range domains {
		if domain.GetString("dns_status") == schema.DNSError {
			if time.Since(domain.GetDateTime("updated").Time()) > 5*time.Minute && p.desiredDNS(domain).Problem == "" {
				p.syncDomainDNSLogged(domain.Id)
			}
			continue
		}
		p.checkDomainPropagation(context.Background(), domain)
	}
}

// checkDomainPropagation asks about a domain's records. Only a domain that
// waits for them, pending or manual, is marked done; the answers of others
// are returned and not stored.
func (p *Panel) checkDomainPropagation(ctx context.Context, domain *core.Record) []dns.Propagation {
	want := p.desiredDNS(domain)
	if want.Problem != "" || len(want.Records) == 0 {
		return nil
	}
	zone := want.ZoneName
	if zone == "" {
		zone = domain.GetString("hostname")
	}
	results := p.checker().Check(ctx, zone, want.Records)
	resolved := len(results) > 0
	for _, result := range results {
		resolved = resolved && result.Resolved
	}
	if !resolved {
		return results
	}

	lock := p.dnsLocks.of(domain.Id)
	lock.Lock()
	defer lock.Unlock()
	fresh, err := p.App.FindRecordById(schema.Domains, domain.Id)
	if err != nil {
		return results
	}
	// what was checked must still be what is wanted
	if again := p.desiredDNS(fresh); !sameRecords(again.Records, want.Records) {
		return results
	}
	switch fresh.GetString("dns_status") {
	case schema.DNSPending, schema.DNSManual:
		p.setFields(fresh, map[string]any{"dns_status": schema.DNSOK, "dns_error": ""})
		rows, _ := p.App.FindRecordsByFilter(schema.DNSRecords, "domain={:d}", "", 0, 0, dbx.Params{"d": fresh.Id})
		for _, row := range rows {
			p.setFields(row, map[string]any{"status": schema.DNSOK, "last_check": types.NowDateTime()})
		}
	}
	return results
}

func sameRecords(a, b []dns.Record) bool {
	if len(a) != len(b) {
		return false
	}
	keys := map[string]bool{}
	for _, r := range a {
		keys[r.Key()] = true
	}
	for _, r := range b {
		if !keys[r.Key()] {
			return false
		}
	}
	return true
}

// CheckDNSDrift compares the records PocketCloud made with what their zones
// hold now: a record someone removed or changed is drift, and a domain whose
// zone is right again is no longer. Records of domains removed already, whose
// removal failed, are tried again.
func (p *Panel) CheckDNSDrift() {
	ctx := context.Background()
	if orphans, err := p.App.FindRecordsByFilter(schema.DNSRecords, "domain=''", "", 0, 0); err == nil && len(orphans) > 0 {
		if err := p.removeRecords(ctx, orphans); err != nil {
			p.Log.Warn("failed to remove the DNS records of removed domains again", "error", err)
		}
	}
	zones, err := p.App.FindRecordsByFilter(schema.DNSZones, "managed=true", "", 0, 0)
	if err != nil {
		return
	}
	for _, zone := range zones {
		p.checkZoneDrift(ctx, zone)
	}
}

func (p *Panel) checkZoneDrift(ctx context.Context, zone *core.Record) {
	rows, err := p.App.FindRecordsByFilter(schema.DNSRecords, "zone={:z} && domain!=''", "", 0, 0, dbx.Params{"z": zone.Id})
	if err != nil || len(rows) == 0 {
		return
	}
	provider, err := p.App.FindRecordById(schema.DNSProviders, zone.GetString("provider"))
	if err != nil {
		return
	}
	// each look at a DNS server of a LAN is a command of its agent: one an
	// hour keeps the history readable
	if lanDriver(provider) && time.Since(zone.GetDateTime("last_check").Time()) < time.Hour-time.Minute {
		return
	}
	driver, client, err := p.openProvider(provider)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	present, err := dns.Get(ctx, client, zone.GetString("name"))
	now := types.NowDateTime()
	if err != nil {
		p.setFields(zone, map[string]any{"status": schema.DNSError, "last_error": driver.Explain(err), "last_check": now})
		return
	}
	p.setFields(zone, map[string]any{"status": schema.DNSOK, "last_error": "", "last_check": now})

	byDomain := map[string][]*core.Record{}
	for _, row := range rows {
		byDomain[row.GetString("domain")] = append(byDomain[row.GetString("domain")], row)
	}
	for domainID, domainRows := range byDomain {
		p.checkDomainDrift(domainID, domainRows, present, driver, now)
	}
}

// checkDomainDrift compares one domain's records with the zone, under the
// domain's lock: its A and AAAA sets must hold its records and nothing else.
func (p *Panel) checkDomainDrift(domainID string, rows []*core.Record, present []dns.Record, driver dns.Driver, now types.DateTime) {
	lock := p.dnsLocks.of(domainID)
	lock.Lock()
	defer lock.Unlock()

	domain, err := p.App.FindRecordById(schema.Domains, domainID)
	if err != nil {
		return
	}
	wanted := make([]dns.Record, 0, len(rows))
	var own []dns.RRSet
	for _, row := range rows {
		wanted = append(wanted, rowRecord(row))
		own = append(own, dns.AddressSets(row.GetString("name"))...)
	}
	drift := dns.Compare(wanted, present, own...)

	missing := map[string]bool{}
	for _, r := range drift.Missing {
		missing[r.Key()] = true
	}
	for _, row := range rows {
		fields := map[string]any{"last_check": now}
		switch {
		case missing[rowRecord(row).Key()] || len(drift.Foreign) > 0:
			fields["status"] = schema.DNSDrift
			fields["drift"] = drift
		case row.GetString("status") == schema.DNSDrift:
			fields["status"] = schema.DNSOK
			fields["drift"] = nil
		}
		p.setFields(row, fields)
	}

	switch {
	case !drift.None() && domain.GetString("dns_status") != schema.DNSDrift:
		p.setFields(domain, map[string]any{"dns_status": schema.DNSDrift, "dns_error": "Its records were changed at " + driver.Label + " outside PocketCloud."})
		events.Log(p.App, domain.GetString("host"), "dns.drift", schema.SeverityWarning,
			fmt.Sprintf("The DNS records of %s were changed outside PocketCloud", domain.GetString("hostname")),
			map[string]any{"domain": domain.GetString("hostname")})
	case drift.None() && domain.GetString("dns_status") == schema.DNSDrift:
		p.setFields(domain, map[string]any{"dns_status": schema.DNSOK, "dns_error": ""})
	}
}

// handleDomainDNS says what a domain needs in DNS and what the servers answer.
func (p *Panel) handleDomainDNS(e *core.RequestEvent) error {
	domain, err := p.App.FindRecordById(schema.Domains, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The domain does not exist.", err)
	}
	want := p.desiredDNS(domain)
	response := map[string]any{
		"status":  domain.GetString("dns_status"),
		"error":   domain.GetString("dns_error"),
		"zone":    want.ZoneName,
		"manual":  want.Manual,
		"problem": want.Problem,
		"records": fullRecords(domain.GetString("hostname"), want.Records),
		// a DNS server of a LAN answers what it took: there is no propagation
		"lan": want.LAN,
	}
	if want.Provider != nil {
		response["provider"] = map[string]any{"id": want.Provider.Id, "name": want.Provider.GetString("name"), "kind": want.Provider.GetString("kind")}
	}
	if e.Request.URL.Query().Get("check") == "1" && !want.LAN {
		ctx, cancel := context.WithTimeout(e.Request.Context(), dnsTimeout)
		defer cancel()
		response["propagation"] = p.checkDomainPropagation(ctx, domain)
		if fresh, err := p.App.FindRecordById(schema.Domains, domain.Id); err == nil {
			response["status"] = fresh.GetString("dns_status")
		}
	}
	return e.JSON(http.StatusOK, response)
}

// fullRecords names records by their full hostname, as a DNS host's form asks.
func fullRecords(hostname string, records []dns.Record) []map[string]any {
	out := make([]map[string]any, 0, len(records))
	for _, r := range records {
		out = append(out, map[string]any{"name": r.Name, "hostname": dns.NormalizeZone(hostname), "type": r.Type, "value": r.Value, "ttl": r.TTL})
	}
	return out
}

func (p *Panel) handleSyncDomainDNS(e *core.RequestEvent) error {
	domain, err := p.App.FindRecordById(schema.Domains, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The domain does not exist.", err)
	}
	if problem := p.desiredDNS(domain).Problem; problem != "" {
		return e.BadRequestError(problem, nil)
	}
	if err := p.syncDomainDNS(e.Request.Context(), domain.Id, true); err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	return p.handleDomainDNS(e)
}
