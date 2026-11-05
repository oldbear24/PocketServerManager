package dns

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/libdns/libdns"

	"github.com/oldbear24/PocketServerManager/internal/dns/fakedns"
)

func TestZoneForTakesTheLongestSuffix(t *testing.T) {
	zones := []string{"example.com", "shop.example.com.", "example.org", "ample.com"}
	for hostname, want := range map[string]string{
		"example.com":          "example.com",
		"www.example.com":      "example.com",
		"WWW.Example.COM.":     "example.com",
		"shop.example.com":     "shop.example.com",
		"cdn.shop.example.com": "shop.example.com",
		"notexample.com":       "",
		"example.net":          "",
		"xample.com":           "",
	} {
		got, ok := ZoneFor(hostname, zones)
		if got != want || ok != (want != "") {
			t.Errorf("%s: %q %v, want %q", hostname, got, ok, want)
		}
	}
}

func TestRelativeName(t *testing.T) {
	for _, tc := range [][3]string{
		{"example.com", "example.com", "@"},
		{"www.example.com", "example.com", "www"},
		{"a.b.example.com.", "Example.com", "a.b"},
	} {
		if got := RelativeName(tc[0], tc[1]); got != tc[2] {
			t.Errorf("%s in %s: %q, want %q", tc[0], tc[1], got, tc[2])
		}
	}
}

func TestAddressRecordsNeedPublicAddresses(t *testing.T) {
	records, err := AddressRecords("app.example.com", "example.com", "203.0.114.7", "2a01:4f8::1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0] != (Record{Name: "app", Type: "A", Value: "203.0.114.7", TTL: DefaultTTL}) ||
		records[1] != (Record{Name: "app", Type: "AAAA", Value: "2a01:4f8::1", TTL: DefaultTTL}) {
		t.Fatalf("%+v", records)
	}

	// no IPv6 address, no AAAA record
	if records, err := AddressRecords("example.com", "example.com", "203.0.114.7", ""); err != nil || len(records) != 1 || records[0].Name != "@" {
		t.Errorf("%+v %v", records, err)
	}

	for _, private := range []string{"10.0.0.5", "192.168.1.2", "172.16.0.1", "127.0.0.1", "100.64.1.1", "fd00::1", "fe80::1", "192.0.2.2", "not an address"} {
		if _, err := AddressRecords("app.example.com", "example.com", private, ""); err == nil {
			t.Errorf("%s was taken for a public address", private)
		}
	}
}

func TestSetDeleteAndDrift(t *testing.T) {
	ctx := context.Background()
	fake := fakedns.New("example.com")
	// someone's own record next to PocketCloud's
	fake.Put("example.com", libdns.RR{Name: "mail", Type: "A", Data: "203.0.114.20"})
	// and an old address of the name PocketCloud is about to point
	fake.Put("example.com", libdns.RR{Name: "app", Type: "A", Data: "203.0.114.99"})

	wanted, err := AddressRecords("app.example.com", "example.com", "203.0.114.7", "2a01:4f8::1")
	if err != nil {
		t.Fatal(err)
	}
	if err := Set(ctx, fake, "example.com", wanted); err != nil {
		t.Fatal(err)
	}

	zone, err := Get(ctx, fake, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if drift := Compare(wanted, zone); !drift.None() {
		t.Fatalf("drift right after a set: %+v", drift)
	}
	if len(zone) != 3 {
		t.Fatalf("the old address of app stayed, or mail went: %+v", zone)
	}

	// someone points app elsewhere
	fake.Remove("example.com", "app", "A")
	fake.Put("example.com", libdns.RR{Name: "app", Type: "A", Data: "198.51.101.1"})
	zone, _ = Get(ctx, fake, "example.com")
	drift := Compare(wanted, zone)
	if len(drift.Missing) != 1 || drift.Missing[0].Type != "A" || len(drift.Foreign) != 1 || drift.Foreign[0].Value != "198.51.101.1" {
		t.Fatalf("%+v", drift)
	}

	// a delete takes PocketCloud's records only, and one already gone is fine
	if err := Delete(ctx, fake, "example.com", wanted); err != nil {
		t.Fatal(err)
	}
	zone, _ = Get(ctx, fake, "example.com")
	if len(zone) != 2 {
		t.Fatalf("%+v", zone)
	}
	for _, r := range zone {
		if r.Type == "AAAA" {
			t.Errorf("the AAAA record stayed")
		}
	}
}

func TestCNAMEAndTXTKeys(t *testing.T) {
	a := CNAMERecord("www.example.com", "example.com", "Example.com")
	b := Record{Name: "www", Type: "cname", Value: "example.com"}
	if a.Key() != b.Key() {
		t.Errorf("%q and %q", a.Key(), b.Key())
	}
	if (Record{Name: "_acme-challenge", Type: "TXT", Value: `"abc"`}).Key() != (Record{Name: "_acme-challenge", Type: "TXT", Value: "abc"}).Key() {
		t.Error("a quoted TXT value is another record")
	}
}

func TestListZones(t *testing.T) {
	zones, err := ListZones(context.Background(), fakedns.New("b.example", "A.example."))
	if err != nil || strings.Join(zones, ",") != "a.example,b.example" {
		t.Fatalf("%v %v", zones, err)
	}
}

func TestDriversAndCredentials(t *testing.T) {
	for _, kind := range []string{Cloudflare, Hetzner, WEDOS, Manual} {
		if _, ok := Lookup(kind); !ok {
			t.Errorf("no driver for %s", kind)
		}
	}

	cf, _ := Lookup(Cloudflare)
	if err := cf.CheckCredentials(map[string]string{}); err == nil {
		t.Error("a Cloudflare provider without a token")
	}
	if err := cf.CheckCredentials(map[string]string{"api_token": "x", "other": "y"}); err == nil {
		t.Error("a credential Cloudflare does not take")
	}
	if !cf.ListsZones {
		t.Error("Cloudflare lists zones")
	}

	manual, _ := Lookup(Manual)
	if _, err := manual.Open(nil); !errors.Is(err, ErrManual) {
		t.Errorf("%v", err)
	}

	wedos, _ := Lookup(WEDOS)
	if !strings.Contains(wedos.Note, "allow-list") {
		t.Error("the WEDOS card does not explain the allow-list")
	}
}

func TestExplain(t *testing.T) {
	cf, _ := Lookup(Cloudflare)
	if got := cf.Explain(errors.New("got error status: HTTP 403: Authentication error")); !strings.Contains(got, "refused the credentials") && !strings.Contains(got, "may not") {
		t.Errorf("%q", got)
	}
	wedos, _ := Lookup(WEDOS)
	if got := wedos.Explain(&WEDOSError{Code: 2051, Result: "Access from this IP address is not allowed"}); !strings.Contains(got, "allow-list") {
		t.Errorf("%q", got)
	}
}

func TestPropagationNeedsEveryServer(t *testing.T) {
	answers := map[string][]string{
		"1.1.1.1:53":         {"203.0.114.7"},
		"8.8.8.8:53":         {"203.0.114.99"}, // still the old address
		"ns1.example.net:53": {"203.0.114.7"},
	}
	checker := &Checker{
		Resolvers: []string{"1.1.1.1:53", "8.8.8.8:53"},
		Lookup: func(_ context.Context, server, name, kind string) ([]string, error) {
			if name != "app.example.com" || kind != "A" {
				t.Errorf("asked %s for %s %s", server, name, kind)
			}
			return answers[server], nil
		},
		NameServers: func(context.Context, string) ([]string, error) { return []string{"ns1.example.net:53"}, nil },
	}
	record := Record{Name: "app", Type: "A", Value: "203.0.114.7"}

	result := checker.Check(context.Background(), "example.com", record)
	if result.Resolved || len(result.Answers) != 3 || result.Answers[1].OK {
		t.Fatalf("%+v", result)
	}

	answers["8.8.8.8:53"] = []string{"203.0.114.7"}
	if result := checker.Check(context.Background(), "example.com", record); !result.Resolved {
		t.Fatalf("%+v", result)
	}

	// the zone's apex is asked as the zone's name
	checker.Lookup = func(_ context.Context, _, name, _ string) ([]string, error) {
		if name != "example.com" {
			t.Errorf("asked for %s", name)
		}
		return nil, nil
	}
	if result := checker.Check(context.Background(), "example.com", Record{Name: "@", Type: "A", Value: "203.0.114.7"}); result.Resolved {
		t.Error("an empty answer counted as resolved")
	}
}
