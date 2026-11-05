package dns

import (
	"context"
	"crypto/sha1" //nolint:gosec // WAPI's authentication is defined with SHA-1
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/libdns/libdns"
)

// wedosProvider talks to WEDOS's WAPI. It replaces libdns/wedos, which sends
// the zone with its final dot and returns no error when WAPI refuses a
// request (code other than 1000), so a refused IP address looked like an
// empty zone.
type wedosProvider struct {
	username string
	password string
	endpoint string
	client   *http.Client
	now      func() time.Time
}

const wedosEndpoint = "https://api.wedos.com/wapi/json"

// wedosOK is WAPI's code of a request done.
const wedosOK = 1000

// WEDOSError is a request WAPI refused.
type WEDOSError struct {
	Code   int
	Result string
}

func (e *WEDOSError) Error() string {
	return fmt.Sprintf("WAPI %d: %s", e.Code, e.Result)
}

func newWEDOS(username, password, endpoint string, client *http.Client) *wedosProvider {
	if endpoint == "" {
		endpoint = wedosEndpoint
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &wedosProvider{username: username, password: password, endpoint: endpoint, client: client, now: time.Now}
}

// auth is sha1(user + sha1(password) + the hour in Prague), as WAPI defines it.
func (p *wedosProvider) auth() string {
	location, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		location = time.FixedZone("CET", 3600)
	}
	password := sha1.Sum([]byte(p.password))                                                                          //nolint:gosec // WAPI's definition
	sum := sha1.Sum([]byte(p.username + hex.EncodeToString(password[:]) + strconv.Itoa(p.now().In(location).Hour()))) //nolint:gosec // WAPI's definition
	return hex.EncodeToString(sum[:])
}

func (p *wedosProvider) call(ctx context.Context, command string, data any, into any) error {
	request := map[string]any{"request": map[string]any{
		"user": p.username, "auth": p.auth(), "command": command, "clTRID": "pocketcloud", "data": data,
	}}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	form := url.Values{"request": {string(raw)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("WAPI answered HTTP %d", response.StatusCode)
	}

	var envelope struct {
		Response struct {
			Code   int             `json:"code"`
			Result string          `json:"result"`
			Data   json.RawMessage `json:"data"`
		} `json:"response"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("WAPI's answer is not JSON: %w", err)
	}
	if envelope.Response.Code != wedosOK {
		return &WEDOSError{Code: envelope.Response.Code, Result: envelope.Response.Result}
	}
	if into != nil && len(envelope.Response.Data) > 0 {
		return json.Unmarshal(envelope.Response.Data, into)
	}
	return nil
}

type wedosRow struct {
	ID     string `json:"ID"`
	Name   string `json:"name"`
	TTL    string `json:"ttl"`
	Rdtype string `json:"rdtype"`
	Rdata  string `json:"rdata"`
}

func (p *wedosProvider) rows(ctx context.Context, zone string) ([]wedosRow, error) {
	var data struct {
		Row []wedosRow `json:"row"`
	}
	if err := p.call(ctx, "dns-rows-list", map[string]string{"domain": NormalizeZone(zone)}, &data); err != nil {
		return nil, err
	}
	return data.Row, nil
}

func (r wedosRow) record() (libdns.Record, error) {
	name := strings.TrimSpace(r.Name)
	if name == "" {
		name = "@"
	}
	seconds, _ := strconv.Atoi(r.TTL)
	return libdns.RR{Name: name, Type: strings.ToUpper(r.Rdtype), Data: r.Rdata, TTL: time.Duration(seconds) * time.Second}.Parse()
}

func wedosName(name string) string {
	if name == "@" {
		return ""
	}
	return name
}

func (p *wedosProvider) GetRecords(ctx context.Context, zone string) ([]libdns.Record, error) {
	rows, err := p.rows(ctx, zone)
	if err != nil {
		return nil, err
	}
	out := make([]libdns.Record, 0, len(rows))
	for _, row := range rows {
		record, err := row.record()
		if err != nil {
			// a type libdns cannot read is no record PocketCloud made
			continue
		}
		out = append(out, record)
	}
	return out, nil
}

func (p *wedosProvider) add(ctx context.Context, zone string, rr libdns.RR) error {
	return p.call(ctx, "dns-row-add", map[string]string{
		"domain": NormalizeZone(zone), "name": wedosName(rr.Name), "ttl": strconv.Itoa(int(ttl(int(rr.TTL.Seconds())).Seconds())),
		"type": rr.Type, "rdata": rr.Data,
	}, nil)
}

// commit publishes the zone's changes at once instead of within minutes.
func (p *wedosProvider) commit(ctx context.Context, zone string) error {
	return p.call(ctx, "dns-domain-commit", map[string]string{"name": NormalizeZone(zone)}, nil)
}

func (p *wedosProvider) AppendRecords(ctx context.Context, zone string, records []libdns.Record) ([]libdns.Record, error) {
	for _, record := range records {
		if err := p.add(ctx, zone, record.RR()); err != nil {
			return nil, err
		}
	}
	return records, p.commit(ctx, zone)
}

func (p *wedosProvider) DeleteRecords(ctx context.Context, zone string, records []libdns.Record) ([]libdns.Record, error) {
	rows, err := p.rows(ctx, zone)
	if err != nil {
		return nil, err
	}
	var deleted []libdns.Record
	for _, record := range records {
		want := fromLibdns(record)
		for _, row := range rows {
			parsed, err := row.record()
			if err != nil || fromLibdns(parsed).Key() != want.Key() {
				continue
			}
			if err := p.call(ctx, "dns-row-delete", map[string]string{"domain": NormalizeZone(zone), "row_id": row.ID}, nil); err != nil {
				return deleted, err
			}
			deleted = append(deleted, record)
		}
	}
	if len(deleted) == 0 {
		return nil, nil
	}
	return deleted, p.commit(ctx, zone)
}

func (p *wedosProvider) SetRecords(ctx context.Context, zone string, records []libdns.Record) ([]libdns.Record, error) {
	rows, err := p.rows(ctx, zone)
	if err != nil {
		return nil, err
	}
	sets := map[string]bool{}
	wanted := map[string]bool{}
	for _, record := range records {
		r := fromLibdns(record)
		sets[strings.ToLower(r.Name)+" "+r.Type] = true
		wanted[r.Key()] = true
	}
	present := map[string]bool{}
	changed := false
	for _, row := range rows {
		parsed, err := row.record()
		if err != nil {
			continue
		}
		r := fromLibdns(parsed)
		if !sets[strings.ToLower(r.Name)+" "+r.Type] {
			continue
		}
		if wanted[r.Key()] {
			present[r.Key()] = true
			continue
		}
		if err := p.call(ctx, "dns-row-delete", map[string]string{"domain": NormalizeZone(zone), "row_id": row.ID}, nil); err != nil {
			return nil, err
		}
		changed = true
	}
	for _, record := range records {
		if present[fromLibdns(record).Key()] {
			continue
		}
		if err := p.add(ctx, zone, record.RR()); err != nil {
			return nil, err
		}
		changed = true
	}
	if !changed {
		return records, nil
	}
	return records, p.commit(ctx, zone)
}
