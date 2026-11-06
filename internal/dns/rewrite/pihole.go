package rewrite

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// piHole holds the local DNS records of Pi-hole v6: dns.hosts ("IP name")
// and dns.cnameRecords ("alias,target") of its configuration, changed one
// entry at a time through /api/config. A session is opened with the
// password for each change and closed after it, as Pi-hole has few seats.
type piHole struct {
	api
	password string
	// restartWait is how long a change of dns.cnameRecords may keep the
	// API away: Pi-hole restarts its DNS server for it
	restartWait time.Duration
}

type piHoleConfig struct {
	Config struct {
		DNS struct {
			Hosts        []string `json:"hosts"`
			CNAMERecords []string `json:"cnameRecords"`
		} `json:"dns"`
	} `json:"config"`
}

// session signs in, and returns how a request carries the session, and its
// end.
func (p *piHole) session(ctx context.Context) (func(*http.Request), func(), error) {
	var answer struct {
		Session struct {
			Valid   bool   `json:"valid"`
			SID     string `json:"sid"`
			Message string `json:"message"`
		} `json:"session"`
	}
	if err := p.call(ctx, "POST", "/api/auth", map[string]string{"password": p.password}, nil, &answer); err != nil {
		return nil, nil, err
	}
	if !answer.Session.Valid {
		return nil, nil, errors.Join(ErrUnauthorized, errors.New(answer.Session.Message))
	}
	if answer.Session.SID == "" {
		// no password is set: nothing to sign in to
		return nil, func() {}, nil
	}
	sid := answer.Session.SID
	auth := func(request *http.Request) { request.Header.Set("X-FTL-SID", sid) }
	return auth, func() {
		_ = p.call(context.WithoutCancel(ctx), "DELETE", "/api/auth", nil, auth, nil)
	}, nil
}

func (p *piHole) config(ctx context.Context, auth func(*http.Request)) (piHoleConfig, error) {
	var hosts, cnames piHoleConfig
	if err := p.call(ctx, "GET", "/api/config/dns/hosts", nil, auth, &hosts); err != nil {
		return hosts, err
	}
	if err := p.call(ctx, "GET", "/api/config/dns/cnameRecords", nil, auth, &cnames); err != nil {
		return hosts, err
	}
	hosts.Config.DNS.CNAMERecords = cnames.Config.DNS.CNAMERecords
	return hosts, nil
}

// hostLine reads an entry of dns.hosts: an address and its names.
func hostLine(line string) (string, []string) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", nil
	}
	return fields[0], fields[1:]
}

// cnameLine reads an entry of dns.cnameRecords: aliases, the target and
// perhaps a TTL, as dnsmasq's cname= takes them.
func cnameLine(line string) ([]string, string) {
	parts := strings.Split(line, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	if len(parts) > 2 {
		if _, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
			parts = parts[:len(parts)-1]
		}
	}
	if len(parts) < 2 {
		return nil, ""
	}
	return parts[:len(parts)-1], parts[len(parts)-1]
}

func recordsOf(config piHoleConfig) []Record {
	records := []Record{}
	for _, line := range config.Config.DNS.Hosts {
		addr, names := hostLine(line)
		for _, name := range names {
			if record, err := check(Record{Name: name, Type: typeOf(addr), Value: addr}); err == nil {
				records = append(records, record)
			}
		}
	}
	for _, line := range config.Config.DNS.CNAMERecords {
		aliases, target := cnameLine(line)
		for _, alias := range aliases {
			if record, err := check(Record{Name: alias, Type: "CNAME", Value: target}); err == nil {
				records = append(records, record)
			}
		}
	}
	return records
}

func (p *piHole) List(ctx context.Context) ([]Record, error) {
	auth, end, err := p.session(ctx)
	if err != nil {
		return nil, err
	}
	defer end()
	config, err := p.config(ctx, auth)
	if err != nil {
		return nil, err
	}
	return recordsOf(config), nil
}

// entry is the configuration entry of one record.
func entry(r Record) (string, string) {
	if r.Type == "CNAME" {
		return "cnameRecords", r.Name + "," + r.Value
	}
	return "hosts", r.Value + " " + r.Name
}

// change sends one change of the configuration. A change of dns.cnameRecords
// restarts Pi-hole's DNS server, which keeps the API away for a moment, even
// with ?restart=false: the next request waits until it is back.
func (p *piHole) change(ctx context.Context, method, list, item string, auth func(*http.Request)) error {
	changed := time.Now()
	if err := p.call(ctx, method, "/api/config/dns/"+list+"/"+url.PathEscape(item), nil, auth, nil); err != nil {
		return err
	}
	if list == "cnameRecords" {
		p.restarted(ctx, auth, changed)
	}
	return nil
}

// restarted waits until Pi-hole's DNS server started after a change, as its
// uptime tells, or restartWait passed: one that does not restart goes on.
func (p *piHole) restarted(ctx context.Context, auth func(*http.Request), since time.Time) {
	deadline := time.Now().Add(p.restartWait)
	for time.Now().Before(deadline) {
		var info struct {
			FTL struct {
				// milliseconds
				Uptime float64 `json:"uptime"`
			} `json:"ftl"`
		}
		if err := p.call(ctx, "GET", "/api/info/ftl", nil, auth, &info); err == nil &&
			time.Duration(info.FTL.Uptime)*time.Millisecond < time.Since(since) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func (p *piHole) Append(ctx context.Context, records []Record) error {
	auth, end, err := p.session(ctx)
	if err != nil {
		return err
	}
	defer end()
	for _, r := range records {
		r, err := check(r)
		if err != nil {
			return err
		}
		list, item := entry(r)
		if err := p.change(ctx, "PUT", list, item, auth); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes the entries that hold the records; an entry with other
// names too is put back without the record's.
func (p *piHole) Delete(ctx context.Context, records []Record) error {
	auth, end, err := p.session(ctx)
	if err != nil {
		return err
	}
	defer end()
	config, err := p.config(ctx, auth)
	if err != nil {
		return err
	}
	for _, r := range records {
		r, err := check(r)
		if err != nil {
			return err
		}
		lines := &config.Config.DNS.Hosts
		list := "hosts"
		if r.Type == "CNAME" {
			lines, list = &config.Config.DNS.CNAMERecords, "cnameRecords"
		}
		for i := 0; i < len(*lines); i++ {
			line := (*lines)[i]
			rest, holds := without(line, r)
			if !holds {
				continue
			}
			if err := p.change(ctx, "DELETE", list, line, auth); err != nil {
				return err
			}
			if rest != "" {
				if err := p.change(ctx, "PUT", list, rest, auth); err != nil {
					return err
				}
			}
			// the entries as they are now, for the next record of the same one
			if rest == "" {
				*lines = slices.Delete(*lines, i, i+1)
				i--
			} else {
				(*lines)[i] = rest
			}
		}
	}
	return nil
}

// without tells whether an entry holds a record, and what is left of it
// without the record's name.
func without(line string, r Record) (string, bool) {
	if r.Type == "CNAME" {
		aliases, target := cnameLine(line)
		if !strings.EqualFold(strings.TrimSuffix(target, "."), r.Value) {
			return "", false
		}
		left := slices.DeleteFunc(slices.Clone(aliases), func(a string) bool { return strings.EqualFold(strings.TrimSuffix(a, "."), r.Name) })
		if len(left) == len(aliases) {
			return "", false
		}
		if len(left) == 0 {
			return "", true
		}
		return strings.Join(append(left, target), ","), true
	}
	addr, names := hostLine(line)
	if parsed, err := check(Record{Name: "x", Type: typeOf(addr), Value: addr}); err != nil || parsed.Value != r.Value {
		return "", false
	}
	left := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return strings.EqualFold(strings.TrimSuffix(n, "."), r.Name) })
	if len(left) == len(names) {
		return "", false
	}
	if len(left) == 0 {
		return "", true
	}
	return addr + " " + strings.Join(left, " "), true
}
