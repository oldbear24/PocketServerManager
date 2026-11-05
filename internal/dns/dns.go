// Package dns is the DNS layer of the panel (#22, ADR 0001): one interface
// over the libdns packages of the providers PocketCloud supports, the zone a
// hostname belongs to, the records a domain needs, and checks of what public
// resolvers and a zone's own name servers answer.
//
// A provider is connected with credentials; the panel keeps them sealed and
// opens them only to build a Provider here. The "manual" kind has no API: the
// panel shows the records to create and checks until they resolve.
package dns

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"github.com/libdns/libdns"
)

// Kinds of providers.
const (
	Cloudflare = "cloudflare"
	Hetzner    = "hetzner"
	WEDOS      = "wedos"
	Manual     = "manual"
)

// Provider is what the panel needs of a provider's API.
type Provider interface {
	libdns.RecordGetter
	libdns.RecordAppender
	libdns.RecordSetter
	libdns.RecordDeleter
}

// Field is one credential a kind asks for.
type Field struct {
	Name   string `json:"name"`
	Secret bool   `json:"secret"`
	// Optional fields may stay empty.
	Optional bool `json:"optional,omitempty"`
}

// Driver describes a kind of provider.
type Driver struct {
	Kind   string  `json:"kind"`
	Label  string  `json:"label"`
	Fields []Field `json:"fields"`
	// ListsZones tells whether the API lists the account's zones; for the
	// others the zones are named by hand and checked by reading their records.
	ListsZones bool `json:"lists_zones"`
	// Manual kinds have no API at all.
	Manual bool `json:"manual"`
	// Note says what to know before connecting, such as an allow-list.
	Note string `json:"note,omitempty"`

	open    func(credentials map[string]string) (Provider, error)
	explain func(error) string
}

var (
	driversMu sync.RWMutex
	drivers   = map[string]Driver{}
)

func init() {
	Register(Driver{
		Kind:       Cloudflare,
		Label:      "Cloudflare",
		Fields:     []Field{{Name: "api_token", Secret: true}},
		ListsZones: true,
		Note:       "An API token with the Zone.DNS:Edit and Zone:Read permissions, for all zones or the ones PocketCloud may change.",
		open: func(c map[string]string) (Provider, error) {
			return newCloudflare(c["api_token"]), nil
		},
		explain: explainHTTP,
	})
	Register(Driver{
		Kind:   Hetzner,
		Label:  "Hetzner DNS",
		Fields: []Field{{Name: "api_token", Secret: true}},
		Note:   "An API token of the Hetzner DNS Console. Name the zones it may change: its API does not list them here.",
		open: func(c map[string]string) (Provider, error) {
			return newHetzner(c["api_token"], httpClient.Load()), nil
		},
		explain: explainHTTP,
	})
	Register(Driver{
		Kind:   WEDOS,
		Label:  "WEDOS",
		Fields: []Field{{Name: "username"}, {Name: "password", Secret: true}},
		Note:   "WEDOS's WAPI answers only the IP addresses on its allow-list: add the panel's public address in the WEDOS administration first. Name the zones it may change.",
		open: func(c map[string]string) (Provider, error) {
			return newWEDOS(c["username"], c["password"], "", httpClient.Load()), nil
		},
		explain: explainWEDOS,
	})
	Register(Driver{
		Kind:   Manual,
		Label:  "Manual",
		Fields: []Field{},
		Manual: true,
		Note:   "No API: PocketCloud shows the records to create at your DNS host and checks until they resolve.",
	})
}

// Register adds a kind of provider; tests register fakes.
func Register(d Driver) {
	driversMu.Lock()
	defer driversMu.Unlock()
	drivers[d.Kind] = d
}

// RegisterFake adds a kind backed by an in-memory or test provider, which the
// open function returns for any credentials.
func RegisterFake(kind string, listsZones bool, open func(map[string]string) (Provider, error)) {
	Register(Driver{Kind: kind, Label: kind, Fields: []Field{{Name: "token", Secret: true}}, ListsZones: listsZones, open: open})
}

// Lookup returns a kind's driver.
func Lookup(kind string) (Driver, bool) {
	driversMu.RLock()
	defer driversMu.RUnlock()
	d, ok := drivers[kind]
	return d, ok
}

// Drivers lists the kinds, sorted by label, for the connect dialog.
func Drivers() []Driver {
	driversMu.RLock()
	defer driversMu.RUnlock()
	out := make([]Driver, 0, len(drivers))
	for _, d := range drivers {
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b Driver) int { return strings.Compare(a.Label, b.Label) })
	return out
}

// ErrManual is returned when an API is asked of a manual provider.
var ErrManual = errors.New("a manual provider has no API")

// CheckCredentials says which required credentials are missing.
func (d Driver) CheckCredentials(c map[string]string) error {
	for _, f := range d.Fields {
		if !f.Optional && strings.TrimSpace(c[f.Name]) == "" {
			return fmt.Errorf("%s needs %s", d.Label, f.Name)
		}
	}
	for name := range c {
		if !slices.ContainsFunc(d.Fields, func(f Field) bool { return f.Name == name }) {
			return fmt.Errorf("%s does not take %s", d.Label, name)
		}
	}
	return nil
}

// Open builds a provider's client.
func (d Driver) Open(c map[string]string) (Provider, error) {
	if d.Manual || d.open == nil {
		return nil, ErrManual
	}
	if err := d.CheckCredentials(c); err != nil {
		return nil, err
	}
	return d.open(c)
}

// Explain turns a provider's error into a sentence for people.
func (d Driver) Explain(err error) string {
	if err == nil {
		return ""
	}
	if d.explain != nil {
		if text := d.explain(err); text != "" {
			return text
		}
	}
	return d.Label + " answered: " + err.Error()
}

// ListZones lists the zones of an account whose API can; others return nil.
func ListZones(ctx context.Context, p Provider) ([]string, error) {
	lister, ok := p.(libdns.ZoneLister)
	if !ok {
		return nil, nil
	}
	zones, err := lister.ListZones(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(zones))
	for _, z := range zones {
		out = append(out, NormalizeZone(z.Name))
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// NormalizeZone writes a zone name in lower case without the final dot.
func NormalizeZone(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// ZoneFor returns the zone a hostname belongs to: the longest of the zones
// that is the hostname or one of its parents.
func ZoneFor(hostname string, zones []string) (string, bool) {
	hostname = NormalizeZone(hostname)
	best := ""
	for _, zone := range zones {
		zone = NormalizeZone(zone)
		if zone == "" {
			continue
		}
		if (hostname == zone || strings.HasSuffix(hostname, "."+zone)) && len(zone) > len(best) {
			best = zone
		}
	}
	return best, best != ""
}

// RelativeName is a hostname within its zone, "@" for the zone itself.
func RelativeName(hostname, zone string) string {
	hostname, zone = NormalizeZone(hostname), NormalizeZone(zone)
	if hostname == zone {
		return "@"
	}
	return strings.TrimSuffix(hostname, "."+zone)
}

// Record is a record PocketCloud wants in a zone, in the form it is stored
// and shown: the name relative to the zone, the type and one value.
type Record struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
	TTL   int    `json:"ttl"`
}

// DefaultTTL of the records PocketCloud creates: short, so a moved site
// follows within minutes.
const DefaultTTL = 300

// AddressRecords are the records that point a hostname at a server: A for
// its IPv4 address and AAAA for its IPv6 address. A missing address has no
// record; an address that is not public is refused, as the world could not
// reach it.
func AddressRecords(hostname, zone, ip4, ip6 string) ([]Record, error) {
	name := RelativeName(hostname, zone)
	var records []Record
	for _, value := range []string{ip4, ip6} {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		addr, err := netip.ParseAddr(value)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address", value)
		}
		if !IsPublic(addr) {
			return nil, fmt.Errorf("%s is not a public address, so the world could not reach %s", addr, hostname)
		}
		kind := "A"
		if addr.Is6() && !addr.Is4In6() {
			kind = "AAAA"
		}
		records = append(records, Record{Name: name, Type: kind, Value: addr.Unmap().String(), TTL: DefaultTTL})
	}
	return records, nil
}

// CNAMERecord points a hostname at another name.
func CNAMERecord(hostname, zone, target string) Record {
	return Record{Name: RelativeName(hostname, zone), Type: "CNAME", Value: NormalizeZone(target) + ".", TTL: DefaultTTL}
}

// IsPublic tells whether an address can be reached from the internet.
func IsPublic(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() || addr.IsInterfaceLocalMulticast() {
		return false
	}
	for _, prefix := range nonPublic {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// nonPublic are ranges that netip does not call private but that the
// internet cannot reach either.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),  // documentation
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarks
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"), // documentation
	netip.MustParsePrefix("3fff::/20"),     // documentation, RFC 9637
	netip.MustParsePrefix("100::/64"),      // discard
	netip.MustParsePrefix("fec0::/10"),     // site-local
	netip.MustParsePrefix("::/96"),         // IPv4-compatible
	netip.MustParsePrefix("64:ff9b:1::/48"),
}

// libdnsRecord turns a record into the libdns type its provider expects.
func (r Record) libdns() (libdns.Record, error) {
	rr := libdns.RR{Name: r.Name, Type: r.Type, Data: r.Value, TTL: ttl(r.TTL)}
	return rr.Parse()
}

// fromLibdns reads a provider's record in the form PocketCloud stores.
func fromLibdns(record libdns.Record) Record {
	rr := record.RR()
	return Record{Name: rr.Name, Type: strings.ToUpper(rr.Type), Value: strings.TrimSpace(rr.Data), TTL: int(rr.TTL.Seconds())}
}
