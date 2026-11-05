package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/libdns/libdns"
)

// hetznerAPI is the base of the Hetzner DNS Console's API.
const hetznerAPI = "https://dns.hetzner.com/api/v1"

// hetznerProvider talks to the Hetzner DNS API. It replaces libdns/hetzner,
// which deletes and updates records by name and type alone, so it could
// remove someone's own record of the same name instead of PocketCloud's.
type hetznerProvider struct {
	token  string
	client *http.Client

	mu    sync.Mutex
	zones map[string]string
}

func newHetzner(token string, client *http.Client) *hetznerProvider {
	return &hetznerProvider{token: token, client: client, zones: map[string]string{}}
}

type hetznerRecord struct {
	ID     string `json:"id,omitempty"`
	ZoneID string `json:"zone_id"`
	Type   string `json:"type"`
	Name   string `json:"name"`
	Value  string `json:"value"`
	TTL    int    `json:"ttl,omitempty"`
}

func (p *hetznerProvider) do(ctx context.Context, method, path string, body, into any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, hetznerAPI+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Auth-API-Token", p.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode >= 300 {
		var problem struct {
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &problem)
		message := strings.TrimSpace(problem.Message + " " + problem.Error.Message)
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		return fmt.Errorf("HTTP %d: %s", response.StatusCode, message)
	}
	if into != nil {
		return json.Unmarshal(raw, into)
	}
	return nil
}

func (p *hetznerProvider) zoneID(ctx context.Context, zone string) (string, error) {
	zone = NormalizeZone(zone)
	p.mu.Lock()
	id, ok := p.zones[zone]
	p.mu.Unlock()
	if ok {
		return id, nil
	}
	var answer struct {
		Zones []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"zones"`
	}
	if err := p.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(zone), nil, &answer); err != nil {
		return "", err
	}
	for _, z := range answer.Zones {
		if NormalizeZone(z.Name) == zone {
			p.mu.Lock()
			p.zones[zone] = z.ID
			p.mu.Unlock()
			return z.ID, nil
		}
	}
	return "", fmt.Errorf("HTTP 404: zone %s not found", zone)
}

func (p *hetznerProvider) records(ctx context.Context, zone string) (string, []hetznerRecord, error) {
	id, err := p.zoneID(ctx, zone)
	if err != nil {
		return "", nil, err
	}
	var answer struct {
		Records []hetznerRecord `json:"records"`
	}
	if err := p.do(ctx, http.MethodGet, "/records?zone_id="+url.QueryEscape(id), nil, &answer); err != nil {
		return "", nil, err
	}
	return id, answer.Records, nil
}

func (r hetznerRecord) record() (libdns.Record, error) {
	return libdns.RR{Name: r.Name, Type: strings.ToUpper(r.Type), Data: r.Value, TTL: time.Duration(r.TTL) * time.Second}.Parse()
}

func (p *hetznerProvider) GetRecords(ctx context.Context, zone string) ([]libdns.Record, error) {
	_, records, err := p.records(ctx, zone)
	if err != nil {
		return nil, err
	}
	out := make([]libdns.Record, 0, len(records))
	for _, r := range records {
		if parsed, err := r.record(); err == nil {
			out = append(out, parsed)
		}
	}
	return out, nil
}

func (p *hetznerProvider) AppendRecords(ctx context.Context, zone string, records []libdns.Record) ([]libdns.Record, error) {
	id, err := p.zoneID(ctx, zone)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		rr := record.RR()
		body := hetznerRecord{ZoneID: id, Type: rr.Type, Name: rr.Name, Value: rr.Data, TTL: int(ttl(int(rr.TTL.Seconds())).Seconds())}
		if err := p.do(ctx, http.MethodPost, "/records", body, nil); err != nil {
			return nil, err
		}
	}
	return records, nil
}

// DeleteRecords deletes the records of the same name, type and value, by id.
func (p *hetznerProvider) DeleteRecords(ctx context.Context, zone string, records []libdns.Record) ([]libdns.Record, error) {
	_, present, err := p.records(ctx, zone)
	if err != nil {
		return nil, err
	}
	var deleted []libdns.Record
	for _, record := range records {
		want := fromLibdns(record).Key()
		for _, r := range present {
			parsed, err := r.record()
			if err != nil || fromLibdns(parsed).Key() != want {
				continue
			}
			if err := p.do(ctx, http.MethodDelete, "/records/"+url.PathEscape(r.ID), nil, nil); err != nil {
				return deleted, err
			}
			deleted = append(deleted, record)
		}
	}
	return deleted, nil
}

// SetRecords makes the records the only ones of their name and type.
func (p *hetznerProvider) SetRecords(ctx context.Context, zone string, records []libdns.Record) ([]libdns.Record, error) {
	wanted := make([]Record, 0, len(records))
	for _, r := range records {
		wanted = append(wanted, fromLibdns(r))
	}
	return records, Set(ctx, p, zone, wanted)
}
