// Package fakedns is an in-memory DNS provider for tests: zones, their
// records and failures, behind the same libdns interfaces as a real provider.
package fakedns

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/libdns/libdns"
)

// Provider holds zones in memory. The zero value has none.
type Provider struct {
	mu    sync.Mutex
	zones map[string][]libdns.RR
	// Fail, when set, is returned by every call.
	fail  error
	calls int
}

// ErrUnknownZone is returned for a zone the provider does not hold.
var ErrUnknownZone = errors.New("zone not found")

// New returns a provider with these zones, empty.
func New(zones ...string) *Provider {
	p := &Provider{zones: map[string][]libdns.RR{}}
	for _, z := range zones {
		p.zones[norm(z)] = nil
	}
	return p
}

func norm(zone string) string {
	return strings.TrimSuffix(strings.ToLower(zone), ".")
}

// Fail makes every call return err, or work again with nil.
func (p *Provider) Fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fail = err
}

// Calls counts the calls so far.
func (p *Provider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// Records returns a zone's records as RRs, sorted.
func (p *Provider) Records(zone string) []libdns.RR {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := slices.Clone(p.zones[norm(zone)])
	slices.SortFunc(out, func(a, b libdns.RR) int {
		return strings.Compare(a.Name+a.Type+a.Data, b.Name+b.Type+b.Data)
	})
	return out
}

// Put adds a record outside PocketCloud, as someone editing the zone would.
func (p *Provider) Put(zone string, rr libdns.RR) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.zones[norm(zone)] = append(p.zones[norm(zone)], rr)
}

// Remove takes records of a name and type away outside PocketCloud.
func (p *Provider) Remove(zone, name, kind string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.zones[norm(zone)] = slices.DeleteFunc(p.zones[norm(zone)], func(rr libdns.RR) bool {
		return rr.Name == name && strings.EqualFold(rr.Type, kind)
	})
}

func (p *Provider) begin(zone string) ([]libdns.RR, error) {
	p.calls++
	if p.fail != nil {
		return nil, p.fail
	}
	records, ok := p.zones[norm(zone)]
	if !ok {
		return nil, ErrUnknownZone
	}
	return records, nil
}

func same(a, b libdns.RR) bool {
	return a.Name == b.Name && strings.EqualFold(a.Type, b.Type) && strings.TrimSuffix(a.Data, ".") == strings.TrimSuffix(b.Data, ".")
}

// ListZones lists the zones.
func (p *Provider) ListZones(context.Context) ([]libdns.Zone, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.fail != nil {
		return nil, p.fail
	}
	var out []libdns.Zone
	for z := range p.zones {
		out = append(out, libdns.Zone{Name: z + "."})
	}
	return out, nil
}

// GetRecords returns a zone's records.
func (p *Provider) GetRecords(_ context.Context, zone string) ([]libdns.Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	records, err := p.begin(zone)
	if err != nil {
		return nil, err
	}
	out := make([]libdns.Record, 0, len(records))
	for _, rr := range records {
		parsed, err := rr.Parse()
		if err != nil {
			return nil, err
		}
		out = append(out, parsed)
	}
	return out, nil
}

// AppendRecords adds records.
func (p *Provider) AppendRecords(_ context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	records, err := p.begin(zone)
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		records = append(records, r.RR())
	}
	p.zones[norm(zone)] = records
	return recs, nil
}

// SetRecords replaces the records of each name and type given.
func (p *Provider) SetRecords(_ context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	records, err := p.begin(zone)
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		rr := r.RR()
		records = slices.DeleteFunc(records, func(have libdns.RR) bool {
			return have.Name == rr.Name && strings.EqualFold(have.Type, rr.Type)
		})
	}
	for _, r := range recs {
		records = append(records, r.RR())
	}
	p.zones[norm(zone)] = records
	return recs, nil
}

// DeleteRecords removes the records given.
func (p *Provider) DeleteRecords(_ context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	records, err := p.begin(zone)
	if err != nil {
		return nil, err
	}
	var deleted []libdns.Record
	for _, r := range recs {
		before := len(records)
		records = slices.DeleteFunc(records, func(have libdns.RR) bool { return same(have, r.RR()) })
		if len(records) < before {
			deleted = append(deleted, r)
		}
	}
	p.zones[norm(zone)] = records
	return deleted, nil
}
