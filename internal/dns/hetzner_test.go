package dns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeHetzner answers like the Hetzner DNS API for one zone.
type fakeHetzner struct {
	mu      sync.Mutex
	records []hetznerRecord
	next    int
	deletes []string
}

func (f *fakeHetzner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Auth-API-Token") != "h-token" {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "Invalid authentication credentials"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case path == "/zones" && r.Method == http.MethodGet:
		var zones []map[string]string
		if r.URL.Query().Get("name") == "example.de" {
			zones = append(zones, map[string]string{"id": "z1", "name": "example.de"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"zones": zones})
	case path == "/records" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]any{"records": f.records})
	case path == "/records" && r.Method == http.MethodPost:
		var rec hetznerRecord
		_ = json.NewDecoder(r.Body).Decode(&rec)
		f.next++
		rec.ID = "r" + strconv.Itoa(f.next)
		f.records = append(f.records, rec)
		_ = json.NewEncoder(w).Encode(map[string]any{"record": rec})
	case strings.HasPrefix(path, "/records/") && r.Method == http.MethodDelete:
		id, _ := url.PathUnescape(strings.TrimPrefix(path, "/records/"))
		f.deletes = append(f.deletes, id)
		for i, rec := range f.records {
			if rec.ID == id {
				f.records = append(f.records[:i], f.records[i+1:]...)
				break
			}
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

type rewriteTo struct{ base *url.URL }

func (r rewriteTo) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = r.base.Scheme, r.base.Host
	return http.DefaultTransport.RoundTrip(req)
}

// TestHetznerDeletesTheRecordOfItsValue: libdns/hetzner deleted the first
// record of a name and type, which may be someone's own; the adapter deletes
// the one of PocketCloud's value, by id.
func TestHetznerDeletesTheRecordOfItsValue(t *testing.T) {
	fake := &fakeHetzner{records: []hetznerRecord{
		{ID: "own", ZoneID: "z1", Type: "A", Name: "www", Value: "198.51.101.20", TTL: 300},
	}, next: 1}
	server := httptest.NewServer(fake)
	defer server.Close()
	base, _ := url.Parse(server.URL)
	p := newHetzner("h-token", &http.Client{Transport: rewriteTo{base: base}})
	ctx := context.Background()

	mine := []Record{{Name: "app", Type: "A", Value: "203.0.114.7", TTL: 300}}
	if err := Set(ctx, p, "example.de", mine); err != nil {
		t.Fatal(err)
	}
	theirs := Record{Name: "www", Type: "A", Value: "203.0.114.7", TTL: 300}
	// PocketCloud adds its own www record next to someone's, then removes it
	if err := Append(ctx, p, "example.de", []Record{theirs}); err != nil {
		t.Fatal(err)
	}
	if err := Delete(ctx, p, "example.de", []Record{theirs}); err != nil {
		t.Fatal(err)
	}
	zone, _ := Get(ctx, p, "example.de")
	keys := map[string]bool{}
	for _, r := range zone {
		keys[r.Key()] = true
	}
	if !keys[(Record{Name: "www", Type: "A", Value: "198.51.101.20"}).Key()] || keys[theirs.Key()] || !keys[mine[0].Key()] {
		t.Fatalf("%+v, deleted %v", zone, fake.deletes)
	}

	// a wrong token is said in words
	bad := newHetzner("wrong", &http.Client{Transport: rewriteTo{base: base}})
	err := Check(ctx, bad, "example.de")
	driver, _ := Lookup(Hetzner)
	if err == nil || !strings.Contains(driver.Explain(err), "refused") {
		t.Fatalf("%v", err)
	}
	if err := Check(ctx, p, "other.de"); err == nil || !strings.Contains(driver.Explain(err), "does not know this zone") {
		t.Fatalf("%v", err)
	}
}
