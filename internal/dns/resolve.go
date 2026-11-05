package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"
)

// PublicResolvers are asked whether a record has propagated.
var PublicResolvers = []string{"1.1.1.1:53", "8.8.8.8:53"}

// LookupFunc asks one DNS server for the values of a name and type; the
// Propagation check uses it, and tests replace it.
type LookupFunc func(ctx context.Context, server, name, kind string) ([]string, error)

// Answer is what one server said of a record.
type Answer struct {
	Server string   `json:"server"`
	Values []string `json:"values,omitempty"`
	Error  string   `json:"error,omitempty"`
	OK     bool     `json:"ok"`
}

// Propagation is what the public resolvers and the zone's name servers say.
type Propagation struct {
	Record  Record   `json:"record"`
	Answers []Answer `json:"answers"`
	// Resolved is true when every server answers the value PocketCloud wants.
	Resolved bool `json:"resolved"`
}

// Checker checks records against DNS servers.
type Checker struct {
	Resolvers []string
	Lookup    LookupFunc
	// NameServers finds a zone's authoritative servers, as host:53.
	NameServers func(ctx context.Context, zone string) ([]string, error)
}

// NewChecker asks the public resolvers and the zone's own name servers.
func NewChecker() *Checker {
	return &Checker{Resolvers: PublicResolvers, Lookup: lookupAt, NameServers: nameServers}
}

// Check asks every server for a record of a zone.
func (c *Checker) Check(ctx context.Context, zone string, record Record) Propagation {
	servers := slices.Clone(c.Resolvers)
	if c.NameServers != nil {
		if authoritative, err := c.NameServers(ctx, zone); err == nil {
			servers = append(servers, authoritative...)
		}
	}

	fqdn := zone
	if record.Name != "@" {
		fqdn = record.Name + "." + zone
	}
	result := Propagation{Record: record, Resolved: len(servers) > 0}
	for _, server := range servers {
		answer := Answer{Server: server}
		values, err := c.Lookup(ctx, server, fqdn, record.Type)
		if err != nil {
			answer.Error = err.Error()
		}
		answer.Values = values
		for _, v := range values {
			if normalizeValue(record.Type, v) == normalizeValue(record.Type, record.Value) {
				answer.OK = true
			}
		}
		result.Resolved = result.Resolved && answer.OK
		result.Answers = append(result.Answers, answer)
	}
	return result
}

func resolverAt(server string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, server)
		},
	}
}

// lookupAt asks one server for a name's values of a type.
func lookupAt(ctx context.Context, server, name, kind string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r := resolverAt(server)

	switch strings.ToUpper(kind) {
	case "A", "AAAA":
		network := "ip4"
		if strings.EqualFold(kind, "AAAA") {
			network = "ip6"
		}
		addrs, err := r.LookupNetIP(ctx, network, name)
		if err != nil {
			return nil, notFoundIsEmpty(err)
		}
		out := make([]string, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, a.Unmap().String())
		}
		return out, nil
	case "CNAME":
		target, err := r.LookupCNAME(ctx, name)
		if err != nil {
			return nil, notFoundIsEmpty(err)
		}
		// a name without a CNAME answers with itself
		if strings.EqualFold(strings.TrimSuffix(target, "."), strings.TrimSuffix(name, ".")) {
			return nil, nil
		}
		return []string{target}, nil
	case "TXT":
		values, err := r.LookupTXT(ctx, name)
		if err != nil {
			return nil, notFoundIsEmpty(err)
		}
		return values, nil
	}
	return nil, fmt.Errorf("cannot check %s records", kind)
}

func notFoundIsEmpty(err error) error {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return nil
	}
	return err
}

// nameServers finds a zone's authoritative servers through the public resolvers.
func nameServers(ctx context.Context, zone string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	records, err := resolverAt(PublicResolvers[0]).LookupNS(ctx, zone)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(records))
	for _, ns := range records {
		out = append(out, net.JoinHostPort(strings.TrimSuffix(ns.Host, "."), "53"))
	}
	slices.Sort(out)
	return out, nil
}
