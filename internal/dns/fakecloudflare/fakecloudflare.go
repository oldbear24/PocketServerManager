// Package fakecloudflare answers like Cloudflare's API v4 for zones and DNS
// records, so tests run libdns/cloudflare's own code against it. Its client
// sends requests for api.cloudflare.com to the fake.
package fakecloudflare

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Record is a DNS record as Cloudflare stores it: the name is the full name.
type Record struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	ZoneID  string `json:"zone_id"`
}

type zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// API is the fake. Token is the only API token it takes.
type API struct {
	Token string

	mu      sync.Mutex
	zones   []zone
	records map[string][]Record
	next    int
	server  *httptest.Server
}

// New starts a fake with these zones and a token.
func New(token string, zones ...string) *API {
	a := &API{Token: token, records: map[string][]Record{}}
	for _, name := range zones {
		a.AddZone(name)
	}
	a.server = httptest.NewServer(a)
	return a
}

// Close stops the fake.
func (a *API) Close() { a.server.Close() }

// AddZone adds a zone to the account.
func (a *API) AddZone(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.next++
	a.zones = append(a.zones, zone{ID: "zone" + strconv.Itoa(a.next), Name: name})
}

// Records returns the records of a zone.
func (a *API) Records(name string) []Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, z := range a.zones {
		if z.Name == name {
			return slices.Clone(a.records[z.ID])
		}
	}
	return nil
}

// Client sends requests for api.cloudflare.com to the fake.
func (a *API) Client() *http.Client {
	target, _ := url.Parse(a.server.URL)
	return &http.Client{Transport: rewrite{target: target, next: a.server.Client().Transport}}
}

type rewrite struct {
	target *url.URL
	next   http.RoundTripper
}

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "api.cloudflare.com" {
		req = req.Clone(req.Context())
		req.URL.Scheme, req.URL.Host = r.target.Scheme, r.target.Host
	}
	return r.next.RoundTrip(req)
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	fail := func(status, code int, message string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{{"code": code, "message": message}}, "result": nil})
	}
	if r.Header.Get("Authorization") != "Bearer "+a.Token {
		fail(http.StatusForbidden, 10000, "Authentication error")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/client/v4")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	query := r.URL.Query()

	switch {
	case len(parts) == 1 && parts[0] == "zones" && r.Method == http.MethodGet:
		var list []zone
		for _, z := range a.zones {
			if name := query.Get("name"); name == "" || name == z.Name {
				list = append(list, z)
			}
		}
		a.page(w, query, list)
	case len(parts) == 3 && parts[0] == "zones" && parts[2] == "dns_records" && r.Method == http.MethodGet:
		var list []Record
		for _, rec := range a.records[parts[1]] {
			if (query.Get("type") == "" || query.Get("type") == rec.Type) &&
				(query.Get("name") == "" || strings.TrimSuffix(query.Get("name"), ".") == rec.Name) &&
				(query.Get("content.exact") == "" || query.Get("content.exact") == rec.Content) {
				list = append(list, rec)
			}
		}
		a.page(w, query, list)
	case len(parts) == 3 && parts[0] == "zones" && parts[2] == "dns_records" && r.Method == http.MethodPost:
		var rec Record
		if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
			fail(http.StatusBadRequest, 9207, "bad request")
			return
		}
		a.next++
		rec.ID = "rec" + strconv.Itoa(a.next)
		rec.ZoneID = parts[1]
		rec.Name = a.fullName(parts[1], rec.Name)
		a.records[parts[1]] = append(a.records[parts[1]], rec)
		a.result(w, rec, nil)
	case len(parts) == 4 && parts[0] == "zones" && parts[2] == "dns_records" && (r.Method == http.MethodPut || r.Method == http.MethodPatch):
		var change Record
		if err := json.NewDecoder(r.Body).Decode(&change); err != nil {
			fail(http.StatusBadRequest, 9207, "bad request")
			return
		}
		for i, rec := range a.records[parts[1]] {
			if rec.ID == parts[3] {
				if change.Content != "" {
					rec.Content = change.Content
				}
				if change.TTL != 0 {
					rec.TTL = change.TTL
				}
				if change.Name != "" {
					rec.Name = a.fullName(parts[1], change.Name)
				}
				a.records[parts[1]][i] = rec
				a.result(w, rec, nil)
				return
			}
		}
		fail(http.StatusNotFound, 81044, "Record does not exist.")
	case len(parts) == 4 && parts[0] == "zones" && parts[2] == "dns_records" && r.Method == http.MethodDelete:
		before := len(a.records[parts[1]])
		a.records[parts[1]] = slices.DeleteFunc(a.records[parts[1]], func(rec Record) bool { return rec.ID == parts[3] })
		if len(a.records[parts[1]]) == before {
			fail(http.StatusNotFound, 81044, "Record does not exist.")
			return
		}
		a.result(w, map[string]string{"id": parts[3]}, nil)
	default:
		fail(http.StatusNotFound, 7003, fmt.Sprintf("no route for %s %s", r.Method, r.URL.Path))
	}
}

// fullName is a record's name as Cloudflare stores it: it takes "@", a name
// relative to the zone, or the full name.
func (a *API) fullName(zoneID, name string) string {
	zoneName := ""
	for _, z := range a.zones {
		if z.ID == zoneID {
			zoneName = z.Name
		}
	}
	name = strings.TrimSuffix(name, ".")
	switch {
	case name == "@" || name == zoneName:
		return zoneName
	case strings.HasSuffix(name, "."+zoneName):
		return name
	}
	return name + "." + zoneName
}

func (a *API) result(w http.ResponseWriter, result any, info map[string]int) {
	body := map[string]any{"success": true, "errors": []any{}, "messages": []any{}, "result": result}
	if info != nil {
		body["result_info"] = info
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (a *API) page(w http.ResponseWriter, query url.Values, list any) {
	raw, _ := json.Marshal(list)
	var items []json.RawMessage
	_ = json.Unmarshal(raw, &items)

	perPage, _ := strconv.Atoi(query.Get("per_page"))
	if perPage <= 0 {
		perPage = 20 // Cloudflare's default
	}
	page, _ := strconv.Atoi(query.Get("page"))
	if page <= 0 {
		page = 1
	}
	start := min((page-1)*perPage, len(items))
	end := min(start+perPage, len(items))
	pages := (len(items) + perPage - 1) / perPage
	a.result(w, items[start:end], map[string]int{"page": page, "per_page": perPage, "count": end - start, "total_count": len(items), "total_pages": pages})
}
