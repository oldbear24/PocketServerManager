package dns

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// PublicResolvers are asked whether a record has propagated.
var PublicResolvers = []string{"1.1.1.1:53", "8.8.8.8:53"}

// LookupFunc asks one DNS server for the values of a name and type; the
// propagation check uses it, and tests replace it.
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
	// Resolved is true when every server answers exactly the values
	// PocketCloud wants for the record's name and type.
	Resolved bool `json:"resolved"`
}

// Checker checks records against DNS servers.
type Checker struct {
	Resolvers []string
	Lookup    LookupFunc
	// NameServers finds the authoritative servers, as host:53, of the zone
	// that holds a name.
	NameServers func(ctx context.Context, name string) ([]string, error)
}

// NewChecker asks the public resolvers and the zone's own name servers.
func NewChecker() *Checker {
	return &Checker{Resolvers: PublicResolvers, Lookup: lookupAt, NameServers: nameServers}
}

// Check asks every server for the wanted records of a zone: per record, every
// server must answer the values wanted for its name and type, and no others.
func (c *Checker) Check(ctx context.Context, zone string, wanted []Record) []Propagation {
	servers := slices.Clone(c.Resolvers)
	var nsError error
	if c.NameServers != nil {
		authoritative, err := c.NameServers(ctx, zone)
		if err != nil {
			nsError = err
		}
		servers = append(servers, authoritative...)
	}

	sets := map[string][]string{}
	for _, r := range wanted {
		key := r.set().key()
		sets[key] = append(sets[key], normalizeValue(r.Type, r.Value))
	}

	results := make([]Propagation, 0, len(wanted))
	for _, record := range wanted {
		fqdn := zone
		if record.Name != "@" {
			fqdn = record.Name + "." + zone
		}
		want := slices.Clone(sets[record.set().key()])
		slices.Sort(want)

		result := Propagation{Record: record, Resolved: len(servers) > 0 && nsError == nil}
		for _, server := range servers {
			answer := Answer{Server: server}
			values, err := c.Lookup(ctx, server, fqdn, record.Type)
			if err != nil {
				answer.Error = err.Error()
			}
			answer.Values = values
			got := make([]string, 0, len(values))
			for _, v := range values {
				got = append(got, normalizeValue(record.Type, v))
			}
			slices.Sort(got)
			got = slices.Compact(got)
			answer.OK = err == nil && slices.Equal(got, want)
			result.Resolved = result.Resolved && answer.OK
			result.Answers = append(result.Answers, answer)
		}
		if nsError != nil {
			result.Answers = append(result.Answers, Answer{Server: "the name servers of " + zone, Error: nsError.Error()})
		}
		results = append(results, result)
	}
	return results
}

// lookupAt asks one server directly, with a raw DNS query for the fully
// qualified name: not through the system's resolver, which answers from
// /etc/hosts and tries the search domains, and refuses a wildcard name.
func lookupAt(ctx context.Context, server, name, kind string) ([]string, error) {
	qtype, ok := map[string]dnsmessage.Type{
		"A": dnsmessage.TypeA, "AAAA": dnsmessage.TypeAAAA, "CNAME": dnsmessage.TypeCNAME,
		"TXT": dnsmessage.TypeTXT, "NS": dnsmessage.TypeNS,
	}[strings.ToUpper(kind)]
	if !ok {
		return nil, fmt.Errorf("cannot check %s records", kind)
	}
	answers, err := Query(ctx, server, name, qtype)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, rr := range answers {
		if rr.Header.Type != qtype {
			continue
		}
		switch body := rr.Body.(type) {
		case *dnsmessage.AResource:
			out = append(out, net.IP(body.A[:]).String())
		case *dnsmessage.AAAAResource:
			out = append(out, net.IP(body.AAAA[:]).String())
		case *dnsmessage.CNAMEResource:
			out = append(out, body.CNAME.String())
		case *dnsmessage.NSResource:
			out = append(out, body.NS.String())
		case *dnsmessage.TXTResource:
			out = append(out, strings.Join(body.TXT, ""))
		}
	}
	return out, nil
}

// Query sends one question to a DNS server, over UDP and again over TCP when
// the answer was cut short, and returns the answer section. A name that does
// not exist is no error: it has no answers.
func Query(ctx context.Context, server, name string, qtype dnsmessage.Type) ([]dnsmessage.Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	question, err := dnsmessage.NewName(name)
	if err != nil {
		return nil, err
	}
	var id [2]byte
	binary.BigEndian.PutUint16(id[:], uint16(time.Now().UnixNano())) //nolint:gosec // a query id, not a secret
	message := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: binary.BigEndian.Uint16(id[:]), RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: question, Type: qtype, Class: dnsmessage.ClassINET}},
	}
	packed, err := message.Pack()
	if err != nil {
		return nil, err
	}

	answer, err := exchange(ctx, "udp", server, packed)
	if err == nil && answer.Truncated {
		answer, err = exchange(ctx, "tcp", server, packed)
	}
	if err != nil {
		return nil, err
	}
	if answer.ID != message.ID {
		return nil, errors.New("the answer is for another question")
	}
	switch answer.RCode {
	case dnsmessage.RCodeSuccess, dnsmessage.RCodeNameError:
		return answer.Answers, nil
	}
	return nil, fmt.Errorf("the server answered %s", answer.RCode)
}

func exchange(ctx context.Context, network, server string, packed []byte) (*dnsmessage.Message, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, server)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	var raw []byte
	if network == "tcp" {
		framed := binary.BigEndian.AppendUint16(nil, uint16(len(packed))) //nolint:gosec // a DNS message is far below 64 KiB
		if _, err := conn.Write(append(framed, packed...)); err != nil {
			return nil, err
		}
		var size [2]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return nil, err
		}
		raw = make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, err := io.ReadFull(conn, raw); err != nil {
			return nil, err
		}
	} else {
		if _, err := conn.Write(packed); err != nil {
			return nil, err
		}
		raw = make([]byte, 65535)
		n, err := conn.Read(raw)
		if err != nil {
			return nil, err
		}
		raw = raw[:n]
	}

	var answer dnsmessage.Message
	if err := answer.Unpack(raw); err != nil {
		return nil, err
	}
	return &answer, nil
}

// nameServers finds the authoritative servers of the zone that holds a name:
// the nearest of the name and its parents that has NS records, as a public
// resolver answers them.
func nameServers(ctx context.Context, name string) ([]string, error) {
	labels := strings.Split(NormalizeZone(name), ".")
	for i := 0; i < len(labels)-1; i++ {
		candidate := strings.Join(labels[i:], ".")
		hosts, err := lookupAt(ctx, PublicResolvers[0], candidate, "NS")
		if err != nil {
			return nil, fmt.Errorf("asking for the name servers of %s: %w", candidate, err)
		}
		if len(hosts) == 0 {
			continue
		}
		out := make([]string, 0, len(hosts))
		for _, host := range hosts {
			out = append(out, net.JoinHostPort(strings.TrimSuffix(host, "."), "53"))
		}
		slices.Sort(out)
		return out, nil
	}
	return nil, fmt.Errorf("no zone above %s has name servers", name)
}
