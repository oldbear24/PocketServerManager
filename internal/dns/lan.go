package dns

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/libdns/libdns"

	"github.com/oldbear24/PocketServerManager/internal/dns/rewrite"
)

// Kinds of DNS servers of a LAN (#26), reached through the agent of a server
// on the same network: the panel usually cannot reach them.
const (
	AdGuard = rewrite.AdGuard
	PiHole  = rewrite.PiHole
)

// Operations of a Rewriter.
const (
	RewriteList   = "list"
	RewriteAppend = "append"
	RewriteDelete = "delete"
)

// ErrThroughAgent is returned when a provider that is reached through an
// agent is opened without one.
var ErrThroughAgent = errors.New("this provider is reached through the agent of a server on its network")

func init() {
	Register(Driver{
		Kind:   AdGuard,
		Label:  "AdGuard Home",
		Fields: []Field{{Name: "url"}, {Name: "username", Optional: true}, {Name: "password", Secret: true, Optional: true}},
		Agent:  true,
		Note: "PocketCloud sets DNS rewrites in AdGuard Home through the agent of a server on the same network, " +
			"with the address, username and password of its web interface. Name the zones it answers for, such as lan or home.arpa.",
		explain: explainLAN("the username or password of AdGuard Home's web interface"),
	})
	Register(Driver{
		Kind:   PiHole,
		Label:  "Pi-hole",
		Fields: []Field{{Name: "url"}, {Name: "password", Secret: true, Optional: true}},
		Agent:  true,
		Note: "Pi-hole v6. PocketCloud sets its local DNS records through the agent of a server on the same network, " +
			"with the address and password of its web interface; an app password works once webserver.api.app_sudo is on. " +
			"Name the zones it answers for, such as lan or home.arpa.",
		explain: explainLAN("the password of Pi-hole's web interface"),
	})
}

// explainLAN says what went wrong in words: the errors come from the agent
// as text.
func explainLAN(credentials string) func(error) string {
	return func(err error) string {
		text := err.Error()
		switch {
		case strings.Contains(text, rewrite.ErrUnauthorized.Error()):
			return "It refused " + credentials + "."
		case strings.Contains(text, "answered 403"):
			return text + ". An app password of Pi-hole changes records only with webserver.api.app_sudo on."
		}
		return capitalizeFirst(text)
	}
}

func capitalizeFirst(text string) string {
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// Rewriter runs one operation on a DNS server of a LAN, through the agent
// of a server on its network: list returns the records of the zone, append
// and delete change the records given. Names are absolute.
type Rewriter func(ctx context.Context, op, zone string, records []rewrite.Record) ([]rewrite.Record, error)

// OpenRewrites is the provider of a DNS server of a LAN.
func OpenRewrites(run Rewriter) Provider {
	return &rewrites{run: run}
}

type rewrites struct {
	run Rewriter
}

func toRewrites(zone string, records []libdns.Record) []rewrite.Record {
	out := make([]rewrite.Record, 0, len(records))
	for _, record := range records {
		rr := record.RR()
		out = append(out, rewrite.Record{
			Name:  libdns.AbsoluteName(rr.Name, zone),
			Type:  strings.ToUpper(rr.Type),
			Value: strings.TrimSuffix(strings.TrimSpace(rr.Data), "."),
		})
	}
	return out
}

func (r *rewrites) GetRecords(ctx context.Context, zone string) ([]libdns.Record, error) {
	found, err := r.run(ctx, RewriteList, zone, nil)
	if err != nil {
		return nil, err
	}
	out := make([]libdns.Record, 0, len(found))
	for _, record := range found {
		data := record.Value
		if strings.EqualFold(record.Type, "CNAME") {
			data += "."
		}
		parsed, err := (libdns.RR{Name: libdns.RelativeName(record.Name, zone), Type: record.Type, Data: data, TTL: ttl(0)}).Parse()
		if err != nil {
			continue
		}
		out = append(out, parsed)
	}
	return out, nil
}

func (r *rewrites) AppendRecords(ctx context.Context, zone string, records []libdns.Record) ([]libdns.Record, error) {
	if _, err := r.run(ctx, RewriteAppend, zone, toRewrites(zone, records)); err != nil {
		return nil, err
	}
	return records, nil
}

func (r *rewrites) DeleteRecords(ctx context.Context, zone string, records []libdns.Record) ([]libdns.Record, error) {
	if _, err := r.run(ctx, RewriteDelete, zone, toRewrites(zone, records)); err != nil {
		return nil, err
	}
	return records, nil
}

// SetRecords makes the records the only ones of their names and types.
func (r *rewrites) SetRecords(ctx context.Context, zone string, records []libdns.Record) ([]libdns.Record, error) {
	present, err := r.GetRecords(ctx, zone)
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	sets := map[string]bool{}
	for _, record := range records {
		rr := fromLibdns(record)
		wanted[rr.Key()] = true
		sets[strings.ToLower(rr.Name)+" "+rr.Type] = true
	}
	var stale []libdns.Record
	have := map[string]bool{}
	for _, record := range present {
		rr := fromLibdns(record)
		have[rr.Key()] = true
		if sets[strings.ToLower(rr.Name)+" "+rr.Type] && !wanted[rr.Key()] {
			stale = append(stale, record)
		}
	}
	var missing []libdns.Record
	for _, record := range records {
		if !have[fromLibdns(record).Key()] {
			missing = append(missing, record)
		}
	}
	if len(missing) > 0 {
		if _, err := r.AppendRecords(ctx, zone, missing); err != nil {
			return nil, err
		}
	}
	if len(stale) > 0 {
		if _, err := r.DeleteRecords(ctx, zone, stale); err != nil {
			return nil, err
		}
	}
	return records, nil
}

// LANAddressRecords point a name of a LAN zone at a server's address on its
// network, which may be private. An address that no other machine can use,
// such as a loopback one, is refused.
func LANAddressRecords(hostname, zone, ip4, ip6 string) ([]Record, error) {
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
		addr = addr.Unmap()
		if addr.IsLoopback() || addr.IsUnspecified() || addr.IsLinkLocalUnicast() || addr.IsMulticast() {
			return nil, fmt.Errorf("%s is not an address other machines of the network can reach %s at", addr, hostname)
		}
		kind := "A"
		if addr.Is6() {
			kind = "AAAA"
		}
		records = append(records, Record{Name: name, Type: kind, Value: addr.String(), TTL: DefaultTTL})
	}
	return records, nil
}
