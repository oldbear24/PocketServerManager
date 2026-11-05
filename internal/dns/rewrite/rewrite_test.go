package rewrite_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/oldbear24/PocketServerManager/internal/dns/rewrite"
	"github.com/oldbear24/PocketServerManager/internal/dns/rewrite/rewritetest"
)

func keys(records []rewrite.Record) string {
	out := []string{}
	for _, r := range records {
		out = append(out, r.Key())
	}
	slices.Sort(out)
	return strings.Join(out, "; ")
}

func TestAdGuardRewritesAreListedAddedAndDeleted(t *testing.T) {
	ctx := context.Background()
	off := false
	server := rewritetest.NewAdGuard(t, "admin", "secret",
		rewritetest.Rewrite{Domain: "router.lan", Answer: "192.168.1.1"},
		rewritetest.Rewrite{Domain: "old.lan", Answer: "192.168.1.9", Enabled: &off},
		rewritetest.Rewrite{Domain: "ads.example.com", Answer: "A"},
	)
	client, err := rewrite.New(rewrite.AdGuard, server.URL+"/", "admin", "secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}

	records, err := client.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// a disabled rewrite does not answer, nor does one that keeps upstream answers
	if got := keys(records); got != "router.lan A 192.168.1.1" {
		t.Fatalf("listed %s", got)
	}

	add := []rewrite.Record{
		{Name: "Wiki.lan.", Type: "A", Value: "192.168.1.10"},
		{Name: "wiki.lan", Type: "AAAA", Value: "fd00::10"},
		{Name: "www.wiki.lan", Type: "CNAME", Value: "wiki.lan."},
	}
	if err := client.Append(ctx, add); err != nil {
		t.Fatal(err)
	}
	records, _ = client.List(ctx)
	if got := keys(records); got != "router.lan A 192.168.1.1; wiki.lan A 192.168.1.10; wiki.lan AAAA fd00::10; www.wiki.lan CNAME wiki.lan" {
		t.Fatalf("after adding: %s", got)
	}
	if got := keys(rewrite.InZone(records, "wiki.lan")); got != "wiki.lan A 192.168.1.10; wiki.lan AAAA fd00::10; www.wiki.lan CNAME wiki.lan" {
		t.Fatalf("in the zone: %s", got)
	}

	if err := client.Delete(ctx, add[:1]); err != nil {
		t.Fatal(err)
	}
	records, _ = client.List(ctx)
	if got := keys(records); got != "router.lan A 192.168.1.1; wiki.lan AAAA fd00::10; www.wiki.lan CNAME wiki.lan" {
		t.Fatalf("after deleting: %s", got)
	}

	wrong, _ := rewrite.New(rewrite.AdGuard, server.URL, "admin", "nope", server.Client())
	if _, err := wrong.List(ctx); !errors.Is(err, rewrite.ErrUnauthorized) {
		t.Fatalf("a wrong password: %v", err)
	}
	if err := client.Append(ctx, []rewrite.Record{{Name: "mx.lan", Type: "MX", Value: "10 mail.lan"}}); err == nil || !strings.Contains(err.Error(), "only A, AAAA and CNAME") {
		t.Fatalf("an MX record: %v", err)
	}
}

func TestPiHoleRecordsAreChangedEntryByEntryAndEverySessionEnds(t *testing.T) {
	ctx := context.Background()
	server := rewritetest.NewPiHole(t, "secret", "192.168.1.1 router.lan", "192.168.1.5 nas.lan files.lan")
	// the address of its web interface, as people copy it
	client, err := rewrite.New(rewrite.PiHole, server.URL+"/admin/", "", "secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}

	// more changes than the server has seats: each one ends its session
	for range 3 {
		if _, err := client.List(ctx); err != nil {
			t.Fatal(err)
		}
	}
	add := []rewrite.Record{
		{Name: "wiki.lan", Type: "A", Value: "192.168.1.10"},
		{Name: "www.wiki.lan", Type: "CNAME", Value: "wiki.lan."},
	}
	if err := client.Append(ctx, add); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(server.Hosts(), "; "); got != "192.168.1.1 router.lan; 192.168.1.5 nas.lan files.lan; 192.168.1.10 wiki.lan" {
		t.Fatalf("dns.hosts: %s", got)
	}
	if got := strings.Join(server.CNAMEs(), "; "); got != "www.wiki.lan,wiki.lan" {
		t.Fatalf("dns.cnameRecords: %s", got)
	}
	records, err := client.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := keys(records); got != "files.lan A 192.168.1.5; nas.lan A 192.168.1.5; router.lan A 192.168.1.1; wiki.lan A 192.168.1.10; www.wiki.lan CNAME wiki.lan" {
		t.Fatalf("listed %s", got)
	}

	// one name of an entry with two goes, the other stays
	if err := client.Delete(ctx, []rewrite.Record{{Name: "files.lan", Type: "A", Value: "192.168.1.5"}, add[1]}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(server.Hosts(), "; "); got != "192.168.1.1 router.lan; 192.168.1.10 wiki.lan; 192.168.1.5 nas.lan" {
		t.Fatalf("dns.hosts after deleting: %s", got)
	}
	if len(server.CNAMEs()) != 0 {
		t.Fatalf("dns.cnameRecords after deleting: %v", server.CNAMEs())
	}
	if server.Open() != 0 {
		t.Fatalf("%d sessions are still open", server.Open())
	}

	wrong, _ := rewrite.New(rewrite.PiHole, server.URL, "", "nope", server.Client())
	if _, err := wrong.List(ctx); !errors.Is(err, rewrite.ErrUnauthorized) {
		t.Fatalf("a wrong password: %v", err)
	}

	// a Pi-hole without a password needs no session
	open := rewritetest.NewPiHole(t, "")
	client, _ = rewrite.New(rewrite.PiHole, open.URL, "", "", open.Client())
	if err := client.Append(ctx, add[:1]); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(open.Hosts(), "; "); got != "192.168.1.10 wiki.lan" {
		t.Fatalf("dns.hosts: %s", got)
	}
}

func TestOnlyTheAddressOfAWebInterfaceIsTaken(t *testing.T) {
	for _, bad := range []string{"", "192.168.1.2", "ftp://192.168.1.2", "http://user:pw@192.168.1.2", "http://192.168.1.2/?x=1", "http:///path"} {
		if _, err := rewrite.ParseURL(bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	for raw, want := range map[string]string{"http://192.168.1.2:3000/": "http://192.168.1.2:3000", "https://pi.hole/admin/": "https://pi.hole/admin"} {
		if got, err := rewrite.ParseURL(raw); err != nil || got.String() != want {
			t.Errorf("%q: %v %v", raw, got, err)
		}
	}
}
