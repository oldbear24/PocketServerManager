package dns

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/oldbear24/PocketServerManager/internal/dns/rewrite"
)

// memoryRewrites is a DNS server of a LAN in memory, as an agent would reach it.
type memoryRewrites struct {
	records []rewrite.Record
	ops     []string
}

func (m *memoryRewrites) run(_ context.Context, op, zone string, records []rewrite.Record) ([]rewrite.Record, error) {
	m.ops = append(m.ops, op)
	switch op {
	case RewriteList:
		return rewrite.InZone(m.records, zone), nil
	case RewriteAppend:
		m.records = append(m.records, records...)
	case RewriteDelete:
		for _, gone := range records {
			m.records = slices.DeleteFunc(m.records, func(r rewrite.Record) bool { return r.Key() == gone.Key() })
		}
	}
	return nil, nil
}

func TestALANNameGetsItsRecordsAtItsDNSServer(t *testing.T) {
	ctx := context.Background()
	memory := &memoryRewrites{records: []rewrite.Record{
		{Name: "router.lan", Type: "A", Value: "192.168.1.1"},
		{Name: "wiki.lan", Type: "A", Value: "192.168.1.9"},
		{Name: "nas.home.arpa", Type: "A", Value: "192.168.1.5"},
	}}
	provider := OpenRewrites(memory.run)

	records, err := LANAddressRecords("wiki.lan", "lan", "192.168.1.10", "fd00::10")
	if err != nil {
		t.Fatal(err)
	}
	if err := Set(ctx, provider, "lan", records, AddressSets("wiki")...); err != nil {
		t.Fatal(err)
	}
	present, err := Get(ctx, provider, "lan")
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for _, r := range present {
		keys = append(keys, r.Key())
	}
	slices.Sort(keys)
	if got := strings.Join(keys, "; "); got != "router A 192.168.1.1; wiki A 192.168.1.10; wiki AAAA fd00::10" {
		t.Fatalf("the zone: %s", got)
	}
	// the other zone of the same server is left alone
	if !slices.ContainsFunc(memory.records, func(r rewrite.Record) bool { return r.Name == "nas.home.arpa" }) {
		t.Fatal("a record of another zone went")
	}

	// an alias of a LAN name
	if err := Set(ctx, provider, "lan", []Record{CNAMERecord("www.wiki.lan", "lan", "wiki.lan")}, AddressSets("www.wiki")...); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(memory.records, func(r rewrite.Record) bool {
		return r.Name == "www.wiki.lan" && r.Type == "CNAME" && r.Value == "wiki.lan"
	}) {
		t.Fatalf("no CNAME: %+v", memory.records)
	}

	if _, err := LANAddressRecords("wiki.lan", "lan", "127.0.0.1", ""); err == nil {
		t.Error("a loopback address was taken")
	}
	if _, err := LANAddressRecords("wiki.lan", "lan", "fe80::1", ""); err == nil {
		t.Error("a link-local address was taken")
	}
	if driver, _ := Lookup(AdGuard); !driver.Agent || driver.ListsZones {
		t.Errorf("AdGuard Home: %+v", driver)
	}
	if _, err := (Driver{Kind: PiHole, Agent: true}).Open(map[string]string{"url": "http://pi.hole"}); !errors.Is(err, ErrThroughAgent) {
		t.Errorf("opened without an agent: %v", err)
	}
}
