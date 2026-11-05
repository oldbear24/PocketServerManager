package dns

import (
	"context"
	"crypto/sha1" //nolint:gosec // WAPI's definition
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeWAPI answers like WEDOS's WAPI for one zone: rows, adds, deletes and
// commits, and refuses requests with a wrong auth or from a refused address.
type fakeWAPI struct {
	mu        sync.Mutex
	zone      string
	rows      []wedosRow
	next      int
	commits   int
	refuseIP  bool
	wantAuth  string
	lastZones []string
}

func (f *fakeWAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	answer := func(code int, result string, data any) {
		raw, _ := json.Marshal(data)
		_ = json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"code": code, "result": result, "data": json.RawMessage(raw)}})
	}

	var envelope struct {
		Request struct {
			User    string            `json:"user"`
			Auth    string            `json:"auth"`
			Command string            `json:"command"`
			Data    map[string]string `json:"data"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(r.FormValue("request")), &envelope); err != nil {
		answer(2000, "bad request", nil)
		return
	}
	req := envelope.Request
	if f.refuseIP {
		answer(2051, "Access not allowed from this IP address", nil)
		return
	}
	if req.Auth != f.wantAuth {
		answer(2050, "Authentication failed", nil)
		return
	}
	domain := req.Data["domain"]
	if req.Command == "dns-domain-commit" {
		domain = req.Data["name"]
	}
	f.lastZones = append(f.lastZones, domain)
	if domain != f.zone {
		answer(2250, "Domain not found", nil)
		return
	}

	switch req.Command {
	case "dns-rows-list":
		answer(wedosOK, "OK", map[string]any{"row": f.rows})
	case "dns-row-add":
		f.next++
		f.rows = append(f.rows, wedosRow{ID: strconv.Itoa(f.next), Name: req.Data["name"], TTL: req.Data["ttl"], Rdtype: req.Data["type"], Rdata: req.Data["rdata"]})
		answer(wedosOK, "OK", nil)
	case "dns-row-delete":
		for i, row := range f.rows {
			if row.ID == req.Data["row_id"] {
				f.rows = append(f.rows[:i], f.rows[i+1:]...)
				break
			}
		}
		answer(wedosOK, "OK", nil)
	case "dns-domain-commit":
		f.commits++
		answer(wedosOK, "OK", nil)
	default:
		answer(2000, "unknown command", nil)
	}
}

func TestWEDOSSetsRecordsAndSaysWhyItFails(t *testing.T) {
	fake := &fakeWAPI{zone: "example.cz", rows: []wedosRow{{ID: "1", Name: "", TTL: "300", Rdtype: "A", Rdata: "203.0.114.1"}, {ID: "2", Name: "app", TTL: "300", Rdtype: "A", Rdata: "203.0.114.99"}}, next: 2}
	server := httptest.NewServer(fake)
	defer server.Close()

	p := newWEDOS("user", "secret", server.URL, server.Client())
	// 07:30 in Prague: WAPI wants the hour as "07"
	p.now = func() time.Time { return time.Date(2026, 11, 5, 6, 30, 0, 0, time.UTC) }
	password := sha1.Sum([]byte("secret"))
	want := sha1.Sum([]byte("user" + hex.EncodeToString(password[:]) + "07"))
	fake.wantAuth = hex.EncodeToString(want[:])
	ctx := context.Background()

	// the zone goes without its final dot
	wanted := []Record{{Name: "app", Type: "A", Value: "203.0.114.7", TTL: 300}}
	if err := Set(ctx, p, "example.cz", wanted); err != nil {
		t.Fatal(err)
	}
	for _, zone := range fake.lastZones {
		if zone != "example.cz" {
			t.Fatalf("WAPI was asked for %q", zone)
		}
	}
	zone, err := Get(ctx, p, "example.cz")
	if err != nil {
		t.Fatal(err)
	}
	if drift := Compare(wanted, zone); !drift.None() || len(zone) != 2 {
		t.Fatalf("%+v %+v", zone, drift)
	}
	// one commit after the new record was added, one after the old went
	if fake.commits != 2 {
		t.Errorf("%d commits", fake.commits)
	}

	// setting what is there changes nothing, and commits nothing
	if err := Set(ctx, p, "example.cz", wanted); err != nil || fake.commits != 2 {
		t.Errorf("%v, %d commits", err, fake.commits)
	}

	if err := Delete(ctx, p, "example.cz", wanted); err != nil {
		t.Fatal(err)
	}
	if zone, _ := Get(ctx, p, "example.cz"); len(zone) != 1 || zone[0].Name != "@" {
		t.Fatalf("%+v", zone)
	}

	// a refusal is an error, not an empty zone
	fake.refuseIP = true
	_, err = Get(ctx, p, "example.cz")
	var refused *WEDOSError
	if !errors.As(err, &refused) || refused.Code != 2051 {
		t.Fatalf("%v", err)
	}
	driver, _ := Lookup(WEDOS)
	if text := driver.Explain(err); text == "" || !strings.Contains(text, "allow-list") {
		t.Errorf("%q", text)
	}

	fake.refuseIP = false
	if err := Check(ctx, p, "other.cz"); err == nil {
		t.Error("a zone WEDOS does not know passed the check")
	}

	p.password = "wrong"
	if err := Check(ctx, p, "example.cz"); !errors.As(err, &refused) || refused.Code != 2050 {
		t.Errorf("%v", err)
	}
}
