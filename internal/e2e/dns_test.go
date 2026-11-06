package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/agent"
	"github.com/oldbear24/PocketServerManager/internal/dns"
	"github.com/oldbear24/PocketServerManager/internal/dns/fakecloudflare"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// fakeResolvers answer what tests say a name resolves to, as the public
// resolvers and the zone's name servers would.
type fakeResolvers struct {
	mu      sync.Mutex
	answers map[string][]string
}

func (f *fakeResolvers) set(name, kind string, values ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[strings.ToLower(name)+" "+kind] = values
}

func (f *fakeResolvers) checker() *dns.Checker {
	return &dns.Checker{
		Resolvers:   []string{"1.1.1.1:53", "8.8.8.8:53"},
		NameServers: func(context.Context, string) ([]string, error) { return []string{"ns1.example.net:53"}, nil },
		Lookup: func(_ context.Context, _, name, kind string) ([]string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.answers[strings.ToLower(name)+" "+kind], nil
		},
	}
}

// dnsHarness is a panel with an edge server that has public addresses, a
// fake Cloudflare API and fake resolvers.
type dnsHarness struct {
	*harness
	api       *fakecloudflare.API
	resolvers *fakeResolvers
	host      *core.Record
	// edge is the agent of host, and caddy the fake Caddy it runs
	edge  *runningAgent
	caddy *fakeEdge
	admin string
}

func newDNSHarness(t *testing.T, zones ...string) *dnsHarness {
	return newDNSHarnessWith(t, nil, zones...)
}

// newDNSHarnessWith starts the edge agent with a change of its config.
func newDNSHarnessWith(t *testing.T, configure func(*agent.Config), zones ...string) *dnsHarness {
	h := newHarness(t)
	api := fakecloudflare.New("cf-token", zones...)
	t.Cleanup(api.Close)
	dns.SetHTTPClient(api.Client())
	t.Cleanup(func() { dns.SetHTTPClient(http.DefaultClient) })

	resolvers := &fakeResolvers{answers: map[string][]string{}}
	h.panel.SetDNSChecker(resolvers.checker())

	changes := []func(*agent.Config){}
	if configure != nil {
		changes = append(changes, configure)
	}
	edgeAgent, host, caddy := h.managedEdge("dns-edge", changes...)
	host = h.host(host.Id)
	host.Set("public_ip4", "203.0.114.7")
	host.Set("public_ip6", "2a01:4f8::7")
	if err := h.app.Save(host); err != nil {
		t.Fatal(err)
	}
	_, admin := testutil.NewUser(t, h.app, "admin@example.com", schema.RoleAdmin)
	return &dnsHarness{harness: h, api: api, resolvers: resolvers, host: host, edge: edgeAgent, caddy: caddy, admin: admin}
}

func (d *dnsHarness) connect(token string) (int, map[string]any) {
	d.t.Helper()
	return d.connectNamed("Cloudflare", token)
}

func (d *dnsHarness) connectNamed(name, token string) (int, map[string]any) {
	d.t.Helper()
	body := fmt.Sprintf(`{"name":%q,"kind":"cloudflare","credentials":{"api_token":%q}}`, name, token)
	status, raw := testutil.Do(d.t, d.handler, testutil.Request{Method: "POST", URL: "/api/pc/dns-providers", Token: d.admin, Body: body})
	var out map[string]any
	_ = json.Unmarshal([]byte(raw), &out)
	return status, out
}

func (d *dnsHarness) domain(hostname string) *core.Record {
	d.t.Helper()
	status, raw := addDomain(d.t, d.harness, d.admin, fmt.Sprintf(`{"hostname":%q,"host":%q,"target_kind":"redirect","redirect_to":"https://example.org"}`, hostname, d.host.Id))
	if status != http.StatusOK {
		d.t.Fatalf("%d %s", status, raw)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(raw), &created)
	record, err := d.app.FindRecordById(schema.Domains, created.ID)
	if err != nil {
		d.t.Fatal(err)
	}
	return record
}

func (d *dnsHarness) dnsStatus(id string) string {
	record, err := d.app.FindRecordById(schema.Domains, id)
	if err != nil {
		return "gone"
	}
	return record.GetString("dns_status")
}

// synced waits until a domain's records are set and tracked: n rows, and the
// domain waiting for them to propagate. The records reach the provider before
// the rows are saved.
func (d *dnsHarness) synced(id string, n int) {
	d.t.Helper()
	d.waitFor("the domain's records to be tracked", func() bool {
		rows, err := d.app.FindRecordsByFilter(schema.DNSRecords, "domain={:d}", "", 0, 0, dbx.Params{"d": id})
		return err == nil && len(rows) == n && d.dnsStatus(id) == schema.DNSPending
	})
}

// setCloudflareA changes an A record at Cloudflare by hand.
func (d *dnsHarness) setCloudflareA(t *testing.T, name, value string) {
	t.Helper()
	for _, r := range d.api.Records("example.com") {
		if r.Type == "A" && r.Name == name {
			req, _ := http.NewRequest(http.MethodPut, "https://api.cloudflare.com/client/v4/zones/"+r.ZoneID+"/dns_records/"+r.ID, strings.NewReader(fmt.Sprintf(`{"content":%q}`, value)))
			req.Header.Set("Authorization", "Bearer cf-token")
			response, err := d.api.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
		}
	}
}

func cloudflareRecords(api *fakecloudflare.API, zone string) map[string]string {
	out := map[string]string{}
	for _, r := range api.Records(zone) {
		out[r.Name+" "+r.Type] = r.Content
	}
	return out
}

// TestConnectingCloudflareListsItsZones: connecting an account lists its
// zones, keeps its token out of every answer, and says why a wrong token
// does not work.
func TestConnectingCloudflareListsItsZones(t *testing.T) {
	d := newDNSHarness(t, "example.com", "example.org")

	_, viewer := testutil.NewUser(t, d.app, "viewer@example.com", schema.RoleViewer)
	if status, _ := testutil.Do(t, d.handler, testutil.Request{Method: "POST", URL: "/api/pc/dns-providers", Token: viewer,
		Body: `{"name":"x","kind":"cloudflare","credentials":{"api_token":"cf-token"}}`}); status != http.StatusForbidden {
		t.Errorf("a viewer connected a provider: %d", status)
	}

	status, out := d.connect("cf-token")
	if status != http.StatusOK {
		t.Fatalf("%d %v", status, out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "cf-token") {
		t.Errorf("the token is in the answer: %s", raw)
	}
	provider := out["provider"].(map[string]any)
	if provider["status"] != schema.DNSOK {
		t.Fatalf("%v", provider)
	}
	zones := out["zones"].([]any)
	if len(zones) != 2 || zones[0].(map[string]any)["name"] != "example.com" || zones[0].(map[string]any)["managed"] != true {
		t.Fatalf("%v", zones)
	}

	// the record API does not hand out the credentials either
	_, listed := testutil.Do(t, d.handler, testutil.Request{Method: "GET", URL: "/api/collections/dns_providers/records", Token: viewer})
	if strings.Contains(listed, "cf-token") || strings.Contains(listed, "credentials\":\"") {
		t.Errorf("%s", listed)
	}

	// a zone added at Cloudflare later shows up with the next test
	d.api.AddZone("example.net")
	status, raw2 := testutil.Do(t, d.handler, testutil.Request{Method: "POST", URL: "/api/pc/dns-providers/" + provider["id"].(string) + "/test", Token: d.admin})
	if status != http.StatusOK || !strings.Contains(raw2, `"example.net"`) {
		t.Fatalf("%d %s", status, raw2)
	}

	// a wrong token: the provider is saved with the reason
	d.api.Token = "rotated"
	_, raw3 := testutil.Do(t, d.handler, testutil.Request{Method: "POST", URL: "/api/pc/dns-providers/" + provider["id"].(string) + "/test", Token: d.admin})
	if !strings.Contains(raw3, `"status":"error"`) || !strings.Contains(raw3, "refused the request") {
		t.Fatalf("%s", raw3)
	}
}

// TestADomainGetsItsRecordsAndLosesThem: a domain in a managed zone gets A
// and AAAA records for its server's public addresses, follows a change of
// address, and takes its records along when it goes. Records of others in
// the zone stay.
func TestADomainGetsItsRecordsAndLosesThem(t *testing.T) {
	d := newDNSHarness(t, "example.com")
	if status, out := d.connect("cf-token"); status != http.StatusOK {
		t.Fatalf("%d %v", status, out)
	}

	domain := d.domain("app.example.com")
	d.waitFor("the records at Cloudflare", func() bool {
		records := cloudflareRecords(d.api, "example.com")
		return records["app.example.com A"] == "203.0.114.7" && records["app.example.com AAAA"] == "2a01:4f8::7"
	})
	d.synced(domain.Id, 2)
	if fresh, _ := d.app.FindRecordById(schema.Domains, domain.Id); fresh.GetString("zone") == "" {
		t.Error("the domain does not name its zone")
	}

	// it resolves everywhere: the domain is done
	d.resolvers.set("app.example.com", "A", "203.0.114.7")
	d.resolvers.set("app.example.com", "AAAA", "2a01:4f8::7")
	d.panel.CheckDNSPropagation()
	if status := d.dnsStatus(domain.Id); status != schema.DNSOK {
		t.Fatalf("%s", status)
	}

	// the server moves to another address: the A record follows
	host := d.host
	host.Set("public_ip4", "203.0.114.8")
	if err := d.app.Save(host); err != nil {
		t.Fatal(err)
	}
	d.synced(domain.Id, 2)
	if records := cloudflareRecords(d.api, "example.com"); records["app.example.com A"] != "203.0.114.8" || records["app.example.com AAAA"] != "2a01:4f8::7" || len(records) != 2 {
		t.Errorf("the records did not follow the address: %v", records)
	}

	// a second domain, then the first one goes
	other := d.domain("www.example.com")
	d.synced(other.Id, 2)
	if status, _ := testutil.Do(t, d.handler, testutil.Request{Method: "DELETE", URL: "/api/collections/domains/records/" + domain.Id, Token: d.admin}); status != http.StatusNoContent {
		t.Fatalf("%d", status)
	}
	d.waitFor("the removed domain's records to go", func() bool {
		records := cloudflareRecords(d.api, "example.com")
		_, a := records["app.example.com A"]
		_, aaaa := records["app.example.com AAAA"]
		rows, _ := d.app.FindRecordsByFilter(schema.DNSRecords, "", "", 0, 0)
		return !a && !aaaa && len(rows) == 2
	})
	if records := cloudflareRecords(d.api, "example.com"); records["www.example.com A"] != "203.0.114.8" {
		t.Errorf("the other domain lost its records: %v", records)
	}
}

// TestCreatingOrDeletingASiteSetsOrRemovesItsRecords: the domain of a site in
// a managed zone gets its A and AAAA records, and removing the site removes
// them, leaving the zone's other records as they are.
func TestCreatingOrDeletingASiteSetsOrRemovesItsRecords(t *testing.T) {
	d := newDNSHarnessWith(t, func(config *agent.Config) { config.LocalSites = t.TempDir() }, "example.com")
	if status, out := d.connect("cf-token"); status != http.StatusOK {
		t.Fatalf("%d %v", status, out)
	}
	other := d.domain("www.example.com")
	d.synced(other.Id, 2)

	// a site with its placeholder page, served at a name of the zone
	status, raw := testutil.Do(t, d.handler, testutil.Request{Method: "POST", URL: "/api/collections/sites/records", Token: d.admin,
		Body: fmt.Sprintf(`{"name":"blog","host":%q,"source":"empty"}`, d.host.Id)})
	if status != http.StatusOK {
		t.Fatalf("a new site: %d %s", status, raw)
	}
	var site struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(raw), &site)
	status, raw = addDomain(t, d.harness, d.admin, fmt.Sprintf(`{"hostname":"blog.example.com","host":%q,"target_kind":"site","site":%q}`, d.host.Id, site.ID))
	if status != http.StatusOK {
		t.Fatalf("the site's domain: %d %s", status, raw)
	}
	var domain struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(raw), &domain)

	d.waitFor("the site's records at Cloudflare", func() bool {
		records := cloudflareRecords(d.api, "example.com")
		return records["blog.example.com A"] == "203.0.114.7" && records["blog.example.com AAAA"] == "2a01:4f8::7"
	})
	d.synced(domain.ID, 2)

	// the site goes, and its domain's records with it
	if status, raw := testutil.Do(t, d.handler, testutil.Request{Method: "DELETE", URL: "/api/collections/sites/records/" + site.ID, Token: d.admin}); status != http.StatusNoContent {
		t.Fatalf("removing the site: %d %s", status, raw)
	}
	d.waitFor("the site's records to go", func() bool {
		records := cloudflareRecords(d.api, "example.com")
		_, a := records["blog.example.com A"]
		_, aaaa := records["blog.example.com AAAA"]
		rows, _ := d.app.FindRecordsByFilter(schema.DNSRecords, "", "", 0, 0)
		return !a && !aaaa && len(rows) == 2
	})
	if records := cloudflareRecords(d.api, "example.com"); records["www.example.com A"] != "203.0.114.7" || records["www.example.com AAAA"] != "2a01:4f8::7" || len(records) != 2 {
		t.Errorf("the zone's other records changed: %v", records)
	}
}

// TestAnAliasGetsACNAMEAndGoesWithItsDomain: a www name that redirects to
// its site is an alias: the zone gets a CNAME to the site's name, not
// addresses of its own. It must point at a domain of the same server, and it
// goes when that domain goes.
func TestAnAliasGetsACNAMEAndGoesWithItsDomain(t *testing.T) {
	d := newDNSHarness(t, "example.com")
	if status, out := d.connect("cf-token"); status != http.StatusOK {
		t.Fatalf("%d %v", status, out)
	}
	apex := d.domain("shop.example.com")
	d.synced(apex.Id, 2)

	newAlias := func(hostname, of string) (int, string) {
		return addDomain(t, d.harness, d.admin, fmt.Sprintf(`{"hostname":%q,"host":%q,"target_kind":"redirect","redirect_to":"https://shop.example.com","redirect_permanent":true,"dns_alias":%q}`, hostname, d.host.Id, of))
	}
	for _, refused := range []struct{ hostname, of string }{
		{"www.shop.example.com", "nowhere.example.com"},
		{"www.shop.example.com", "www.shop.example.com"},
	} {
		if status, raw := newAlias(refused.hostname, refused.of); status != http.StatusBadRequest || !strings.Contains(raw, "dns_alias") {
			t.Errorf("an alias of %s: %d %s", refused.of, status, raw)
		}
	}

	status, raw := newAlias("www.shop.example.com", "Shop.example.com")
	if status != http.StatusOK {
		t.Fatalf("the alias: %d %s", status, raw)
	}
	var alias struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(raw), &alias)
	d.waitFor("the alias's CNAME at Cloudflare", func() bool {
		records := cloudflareRecords(d.api, "example.com")
		return records["www.shop.example.com CNAME"] == "shop.example.com" || records["www.shop.example.com CNAME"] == "shop.example.com."
	})
	d.synced(alias.ID, 1)
	records := cloudflareRecords(d.api, "example.com")
	if _, a := records["www.shop.example.com A"]; a || len(records) != 3 {
		t.Errorf("the zone: %v", records)
	}

	// the alias resolves once its CNAME does
	d.resolvers.set("www.shop.example.com", "CNAME", "shop.example.com.")
	d.resolvers.set("shop.example.com", "A", "203.0.114.7")
	d.resolvers.set("shop.example.com", "AAAA", "2a01:4f8::7")
	d.panel.CheckDNSPropagation()
	if status := d.dnsStatus(alias.ID); status != schema.DNSOK {
		t.Errorf("the alias's DNS: %s", status)
	}

	// the name it is an alias of keeps its name and its server, is no alias
	// itself, and takes the alias with it
	apex.Set("hostname", "store.example.com")
	if err := d.app.Save(apex); err == nil || !strings.Contains(err.Error(), "aliases") {
		t.Errorf("renaming the aliased domain: %v", err)
	}
	other := testutil.NewHost(t, d.app, "other-edge")
	other.Set("roles", []string{schema.HostRoleDocker, schema.HostRoleEdge})
	if err := d.app.Save(other); err != nil {
		t.Fatal(err)
	}
	apex, _ = d.app.FindRecordById(schema.Domains, apex.Id)
	apex.Set("host", other.Id)
	if err := d.app.Save(apex); err == nil || !strings.Contains(err.Error(), "aliases") {
		t.Errorf("moving the aliased domain: %v", err)
	}
	third := d.domain("third.example.com")
	apex, _ = d.app.FindRecordById(schema.Domains, apex.Id)
	apex.Set("dns_alias", third.GetString("hostname"))
	if err := d.app.Save(apex); err == nil || !strings.Contains(err.Error(), "alias") {
		t.Errorf("an aliased domain became an alias: %v", err)
	}
	apex, _ = d.app.FindRecordById(schema.Domains, apex.Id)
	if status, _ := testutil.Do(t, d.handler, testutil.Request{Method: "DELETE", URL: "/api/collections/domains/records/" + apex.Id, Token: d.admin}); status != http.StatusNoContent {
		t.Fatalf("removing the domain: %d", status)
	}
	d.waitFor("both names to go", func() bool {
		left, _ := d.app.FindRecordsByFilter(schema.Domains, "hostname!='third.example.com'", "", 0, 0)
		records := cloudflareRecords(d.api, "example.com")
		_, a := records["shop.example.com A"]
		_, cname := records["www.shop.example.com CNAME"]
		return len(left) == 0 && !a && !cname
	})
}

// TestManualModeShowsTheRecordsAndChecksUntilTheyResolve: a domain in no
// connected zone shows exactly the records to create, and is done once
// they resolve.
func TestManualModeShowsTheRecordsAndChecksUntilTheyResolve(t *testing.T) {
	d := newDNSHarness(t)

	domain := d.domain("shop.example.cz")
	d.waitFor("manual mode", func() bool { return d.dnsStatus(domain.Id) == schema.DNSManual })

	status, raw := testutil.Do(t, d.handler, testutil.Request{Method: "GET", URL: "/api/pc/domains/" + domain.Id + "/dns?check=1", Token: d.admin})
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, raw)
	}
	var answer struct {
		Status  string `json:"status"`
		Manual  bool   `json:"manual"`
		Records []struct {
			Hostname string `json:"hostname"`
			Type     string `json:"type"`
			Value    string `json:"value"`
		} `json:"records"`
		Propagation []dns.Propagation `json:"propagation"`
	}
	if err := json.Unmarshal([]byte(raw), &answer); err != nil {
		t.Fatal(err)
	}
	if !answer.Manual || answer.Status != schema.DNSManual || len(answer.Records) != 2 ||
		answer.Records[0].Hostname != "shop.example.cz" || answer.Records[0].Type != "A" || answer.Records[0].Value != "203.0.114.7" ||
		answer.Records[1].Type != "AAAA" || answer.Records[1].Value != "2a01:4f8::7" {
		t.Fatalf("%s", raw)
	}
	if len(answer.Propagation) != 2 || answer.Propagation[0].Resolved {
		t.Fatalf("%+v", answer.Propagation)
	}

	// only the A record so far: still waiting
	d.resolvers.set("shop.example.cz", "A", "203.0.114.7")
	d.panel.CheckDNSPropagation()
	if status := d.dnsStatus(domain.Id); status != schema.DNSManual {
		t.Fatalf("%s", status)
	}

	d.resolvers.set("shop.example.cz", "AAAA", "2a01:4f8::7")
	d.panel.CheckDNSPropagation()
	if status := d.dnsStatus(domain.Id); status != schema.DNSOK {
		t.Fatalf("%s", status)
	}

	// a server without a public address says what to do
	d.host.Set("public_ip4", "")
	d.host.Set("public_ip6", "")
	if err := d.app.Save(d.host); err != nil {
		t.Fatal(err)
	}
	d.waitFor("the missing address", func() bool { return d.dnsStatus(domain.Id) == schema.DNSError })
	if fresh, _ := d.app.FindRecordById(schema.Domains, domain.Id); !strings.Contains(fresh.GetString("dns_error"), "public IP address") {
		t.Errorf("%q", fresh.GetString("dns_error"))
	}
	// and an address the internet cannot reach is refused
	d.host.Set("public_ip4", "10.1.2.3")
	if err := d.app.Save(d.host); err == nil {
		t.Error("a private address was taken for a public one")
	}
}

// TestRecordsChangedOutsidePocketCloudAreDrift: someone points the name
// elsewhere at the provider; the drift check notices, and a sync puts the
// record back.
func TestRecordsChangedOutsidePocketCloudAreDrift(t *testing.T) {
	d := newDNSHarness(t, "example.com")
	if status, out := d.connect("cf-token"); status != http.StatusOK {
		t.Fatalf("%d %v", status, out)
	}
	domain := d.domain("app.example.com")
	d.synced(domain.Id, 2)

	// the A record is changed by hand at Cloudflare
	d.setCloudflareA(t, "app.example.com", "198.51.101.1")

	d.panel.CheckDNSDrift()
	if status := d.dnsStatus(domain.Id); status != schema.DNSDrift {
		t.Fatalf("%s", status)
	}
	if found, _ := d.app.FindRecordsByFilter(schema.Events, "kind='dns.drift'", "", 0, 0); len(found) != 1 {
		t.Errorf("%d drift events", len(found))
	}

	// put back by hand: the next check clears the drift by itself
	d.setCloudflareA(t, "app.example.com", "203.0.114.7")
	d.panel.CheckDNSDrift()
	if status := d.dnsStatus(domain.Id); status != schema.DNSOK {
		t.Fatalf("the drift stayed after the zone was right again: %s", status)
	}
	d.setCloudflareA(t, "app.example.com", "198.51.101.1")
	d.panel.CheckDNSDrift()
	if status := d.dnsStatus(domain.Id); status != schema.DNSDrift {
		t.Fatalf("%s", status)
	}

	status, raw := testutil.Do(t, d.handler, testutil.Request{Method: "POST", URL: "/api/pc/domains/" + domain.Id + "/dns/sync", Token: d.admin})
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, raw)
	}
	if records := cloudflareRecords(d.api, "example.com"); records["app.example.com A"] != "203.0.114.7" || len(records) != 2 {
		t.Fatalf("%v", records)
	}
	d.panel.CheckDNSDrift()
	if status := d.dnsStatus(domain.Id); status != schema.DNSPending {
		t.Fatalf("%s", status)
	}
}

// TestAZoneNotManagedAnyMoreIsLeftAlone: records of a zone PocketCloud stops
// managing stay at the provider and are forgotten; its domains go manual,
// and a provider with records cannot be removed until then.
func TestAZoneNotManagedAnyMoreIsLeftAlone(t *testing.T) {
	d := newDNSHarness(t, "example.com")
	_, out := d.connect("cf-token")
	providerID := out["provider"].(map[string]any)["id"].(string)
	zoneID := out["zones"].([]any)[0].(map[string]any)["id"].(string)

	domain := d.domain("app.example.com")
	d.synced(domain.Id, 2)

	if status, raw := testutil.Do(t, d.handler, testutil.Request{Method: "DELETE", URL: "/api/pc/dns-providers/" + providerID, Token: d.admin}); status != http.StatusBadRequest || !strings.Contains(raw, "manages 2 records") {
		t.Fatalf("%d %s", status, raw)
	}

	if status, raw := testutil.Do(t, d.handler, testutil.Request{Method: "PATCH", URL: "/api/pc/dns-zones/" + zoneID, Token: d.admin, Body: `{"managed":false}`}); status != http.StatusOK {
		t.Fatalf("%d %s", status, raw)
	}
	d.waitFor("manual mode", func() bool { return d.dnsStatus(domain.Id) == schema.DNSManual })
	if records := cloudflareRecords(d.api, "example.com"); len(records) != 2 {
		t.Errorf("the records of an unmanaged zone were touched: %v", records)
	}
	if status, raw := testutil.Do(t, d.handler, testutil.Request{Method: "DELETE", URL: "/api/pc/dns-providers/" + providerID, Token: d.admin}); status != http.StatusNoContent {
		t.Fatalf("%d %s", status, raw)
	}
}

// TestDNSMistakesAreRefusedClearly: a zone with records cannot be removed, a
// connect with a bad zone leaves no provider behind, and a private address
// for a server says why it is refused.
func TestDNSMistakesAreRefusedClearly(t *testing.T) {
	d := newDNSHarness(t, "example.com")
	_, out := d.connect("cf-token")
	zoneID := out["zones"].([]any)[0].(map[string]any)["id"].(string)
	domain := d.domain("app.example.com")
	d.synced(domain.Id, 2)

	if status, raw := testutil.Do(t, d.handler, testutil.Request{Method: "DELETE", URL: "/api/pc/dns-zones/" + zoneID, Token: d.admin}); status != http.StatusBadRequest || !strings.Contains(raw, "manages 2 records") {
		t.Fatalf("%d %s", status, raw)
	}

	status, raw := testutil.Do(t, d.handler, testutil.Request{Method: "POST", URL: "/api/pc/dns-providers", Token: d.admin,
		Body: `{"name":"WEDOS","kind":"wedos","credentials":{"username":"u","password":"p"},"zones":["not a zone"]}`})
	if status != http.StatusBadRequest {
		t.Fatalf("%d %s", status, raw)
	}
	if _, err := d.app.FindFirstRecordByData(schema.DNSProviders, "name", "WEDOS"); err == nil {
		t.Error("a provider was left behind")
	}

	status, raw = testutil.Do(t, d.handler, testutil.Request{Method: "PATCH", URL: "/api/collections/hosts/records/" + d.host.Id, Token: d.admin, Body: `{"public_ip4":"10.1.2.3"}`})
	if status != http.StatusBadRequest || !strings.Contains(raw, "not a public address") {
		t.Fatalf("%d %s", status, raw)
	}
}

// TestARemovalThatFailedIsTriedAgain: the records of a domain removed while
// the provider refused are removed by the next drift check.
func TestARemovalThatFailedIsTriedAgain(t *testing.T) {
	d := newDNSHarness(t, "example.com")
	d.connect("cf-token")
	domain := d.domain("app.example.com")
	d.synced(domain.Id, 2)

	d.api.Token = "rotated"
	if status, _ := testutil.Do(t, d.handler, testutil.Request{Method: "DELETE", URL: "/api/collections/domains/records/" + domain.Id, Token: d.admin}); status != http.StatusNoContent {
		t.Fatalf("%d", status)
	}
	d.waitFor("the failed removal", func() bool {
		found, _ := d.app.FindRecordsByFilter(schema.Events, "kind='dns.remove_failed'", "", 0, 0)
		return len(found) == 1
	})
	if records := cloudflareRecords(d.api, "example.com"); len(records) != 2 {
		t.Fatalf("%v", records)
	}

	d.api.Token = "cf-token"
	d.panel.CheckDNSDrift()
	if records := cloudflareRecords(d.api, "example.com"); len(records) != 0 {
		t.Fatalf("the records stayed: %v", records)
	}
	if rows, _ := d.app.FindRecordsByFilter(schema.DNSRecords, "", "", 0, 0); len(rows) != 0 {
		t.Errorf("%d rows", len(rows))
	}
}
