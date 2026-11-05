package dns

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/libdns/libdns"
)

func ttl(seconds int) time.Duration {
	if seconds <= 0 {
		seconds = DefaultTTL
	}
	return time.Duration(seconds) * time.Second
}

// Key names a record by its name, type and value, which is how PocketCloud
// tells its records apart: libdns does not expose the providers' record ids.
func (r Record) Key() string {
	return strings.ToLower(r.Name) + " " + strings.ToUpper(r.Type) + " " + normalizeValue(r.Type, r.Value)
}

func normalizeValue(kind, value string) string {
	value = strings.TrimSpace(value)
	switch strings.ToUpper(kind) {
	case "CNAME", "NS", "MX":
		return strings.ToLower(strings.TrimSuffix(value, ".")) + "."
	case "TXT":
		return strings.Trim(value, `"`)
	}
	return strings.ToLower(value)
}

// Set makes the records the only members of their name and type in the
// zone: an A record that pointed elsewhere is replaced, other names and types
// are left alone.
func Set(ctx context.Context, p Provider, zone string, records []Record) error {
	if len(records) == 0 {
		return nil
	}
	converted, err := convert(records)
	if err != nil {
		return err
	}
	_, err = p.SetRecords(ctx, zone+".", converted)
	return err
}

// Append adds records next to the ones of the same name and type, as a
// DNS-01 challenge needs when two certificates are validated at once.
func Append(ctx context.Context, p Provider, zone string, records []Record) error {
	if len(records) == 0 {
		return nil
	}
	converted, err := convert(records)
	if err != nil {
		return err
	}
	_, err = p.AppendRecords(ctx, zone+".", converted)
	return err
}

// Delete removes exactly these records; one that is already gone is fine.
func Delete(ctx context.Context, p Provider, zone string, records []Record) error {
	if len(records) == 0 {
		return nil
	}
	// only what is there, as some providers fail on a record they do not have
	present, err := Get(ctx, p, zone)
	if err != nil {
		return err
	}
	keys := map[string]bool{}
	for _, r := range present {
		keys[r.Key()] = true
	}
	var gone []Record
	for _, r := range records {
		if keys[r.Key()] {
			gone = append(gone, r)
		}
	}
	if len(gone) == 0 {
		return nil
	}
	converted, err := convert(gone)
	if err != nil {
		return err
	}
	_, err = p.DeleteRecords(ctx, zone+".", converted)
	return err
}

// Get reads every record of a zone.
func Get(ctx context.Context, p Provider, zone string) ([]Record, error) {
	records, err := p.GetRecords(ctx, zone+".")
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(records))
	for _, r := range records {
		out = append(out, fromLibdns(r))
	}
	return out, nil
}

// Drift compares records PocketCloud created with what the zone holds now.
type Drift struct {
	// Missing records were removed or changed outside PocketCloud.
	Missing []Record `json:"missing,omitempty"`
	// Foreign records share a name and type with one of PocketCloud's, with
	// another value: someone pointed the name elsewhere.
	Foreign []Record `json:"foreign,omitempty"`
}

// None tells whether the zone holds what PocketCloud created, and nothing in its way.
func (d Drift) None() bool {
	return len(d.Missing) == 0 && len(d.Foreign) == 0
}

// Compare finds the drift of wanted records within a zone's records.
func Compare(wanted, zone []Record) Drift {
	present := map[string]bool{}
	for _, r := range zone {
		present[r.Key()] = true
	}
	wantedKeys := map[string]bool{}
	sets := map[string]bool{}
	for _, r := range wanted {
		wantedKeys[r.Key()] = true
		sets[strings.ToLower(r.Name)+" "+strings.ToUpper(r.Type)] = true
	}

	var d Drift
	for _, r := range wanted {
		if !present[r.Key()] {
			d.Missing = append(d.Missing, r)
		}
	}
	for _, r := range zone {
		if sets[strings.ToLower(r.Name)+" "+strings.ToUpper(r.Type)] && !wantedKeys[r.Key()] {
			d.Foreign = append(d.Foreign, r)
		}
	}
	sortRecords(d.Missing)
	sortRecords(d.Foreign)
	return d
}

func sortRecords(records []Record) {
	slices.SortFunc(records, func(a, b Record) int { return strings.Compare(a.Key(), b.Key()) })
}

func convert(records []Record) ([]libdns.Record, error) {
	out := make([]libdns.Record, 0, len(records))
	for _, r := range records {
		converted, err := r.libdns()
		if err != nil {
			return nil, fmt.Errorf("the record %s %s %s: %w", r.Name, r.Type, r.Value, err)
		}
		out = append(out, converted)
	}
	return out, nil
}

// Check reads a zone's records, which tells whether the credentials may
// change it: the check of a zone the API does not list.
func Check(ctx context.Context, p Provider, zone string) error {
	_, err := p.GetRecords(ctx, zone+".")
	return err
}
