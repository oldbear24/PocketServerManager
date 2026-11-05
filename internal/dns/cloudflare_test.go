package dns_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/oldbear24/PocketServerManager/internal/dns"
	"github.com/oldbear24/PocketServerManager/internal/dns/fakecloudflare"
)

// TestCloudflareThroughItsAPI runs libdns/cloudflare against a fake of
// Cloudflare's API: an account with more zones than one page holds, and the
// records of a domain set, changed and deleted.
func TestCloudflareThroughItsAPI(t *testing.T) {
	var zones []string
	for i := range 23 {
		zones = append(zones, fmt.Sprintf("zone%02d.example", i))
	}
	api := fakecloudflare.New("good-token", zones...)
	defer api.Close()
	dns.SetHTTPClient(api.Client())
	defer dns.SetHTTPClient(http.DefaultClient)

	driver, _ := dns.Lookup(dns.Cloudflare)
	ctx := context.Background()

	client, err := driver.Open(map[string]string{"api_token": "good-token"})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := dns.ListZones(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 23 || listed[0] != "zone00.example" || listed[22] != "zone22.example" {
		t.Fatalf("%d zones: %v", len(listed), listed)
	}

	wanted, err := dns.AddressRecords("app.zone03.example", "zone03.example", "203.0.114.7", "2a01:4f8::7")
	if err != nil {
		t.Fatal(err)
	}
	if err := dns.Set(ctx, client, "zone03.example", wanted); err != nil {
		t.Fatal(err)
	}
	records := api.Records("zone03.example")
	if len(records) != 2 || records[0].Name != "app.zone03.example" {
		t.Fatalf("%+v", records)
	}

	// a move to another address replaces the A record
	moved, _ := dns.AddressRecords("app.zone03.example", "zone03.example", "203.0.114.8", "2a01:4f8::7")
	if err := dns.Set(ctx, client, "zone03.example", moved); err != nil {
		t.Fatal(err)
	}
	present, err := dns.Get(ctx, client, "zone03.example")
	if err != nil {
		t.Fatal(err)
	}
	if drift := dns.Compare(moved, present); !drift.None() || len(present) != 2 {
		t.Fatalf("%+v %+v", present, drift)
	}

	if err := dns.Delete(ctx, client, "zone03.example", moved); err != nil {
		t.Fatal(err)
	}
	if records := api.Records("zone03.example"); len(records) != 0 {
		t.Fatalf("%+v", records)
	}

	// a wrong token says so in words
	bad, _ := driver.Open(map[string]string{"api_token": "wrong"})
	_, err = dns.ListZones(ctx, bad)
	if err == nil {
		t.Fatal("a wrong token listed zones")
	}
	if text := driver.Explain(err); text == "" || text == err.Error() {
		t.Errorf("%q", text)
	}
}
