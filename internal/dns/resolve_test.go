package dns

import (
	"context"
	"net"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// serveDNS answers questions on a local UDP port from a map of
// "name type" to values; it returns the server's address.
func serveDNS(t *testing.T, answers map[string][]string) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			var query dnsmessage.Message
			if err := query.Unpack(buf[:n]); err != nil || len(query.Questions) != 1 {
				continue
			}
			q := query.Questions[0]
			reply := dnsmessage.Message{Header: dnsmessage.Header{ID: query.ID, Response: true}, Questions: query.Questions}
			values, ok := answers[strings.ToLower(q.Name.String())+" "+strings.TrimPrefix(q.Type.String(), "Type")]
			if !ok {
				reply.RCode = dnsmessage.RCodeNameError
			}
			for _, v := range values {
				header := dnsmessage.ResourceHeader{Name: q.Name, Type: q.Type, Class: dnsmessage.ClassINET, TTL: 300}
				switch q.Type {
				case dnsmessage.TypeA:
					var a [4]byte
					copy(a[:], net.ParseIP(v).To4())
					reply.Answers = append(reply.Answers, dnsmessage.Resource{Header: header, Body: &dnsmessage.AResource{A: a}})
				case dnsmessage.TypeTXT:
					reply.Answers = append(reply.Answers, dnsmessage.Resource{Header: header, Body: &dnsmessage.TXTResource{TXT: []string{v}}})
				}
			}
			packed, err := reply.Pack()
			if err == nil {
				_, _ = conn.WriteTo(packed, addr)
			}
		}
	}()
	return conn.LocalAddr().String()
}

// TestLookupAsksTheServerItself: the answer comes from the server asked, for
// the name as given, wildcards included; not from /etc/hosts.
func TestLookupAsksTheServerItself(t *testing.T) {
	server := serveDNS(t, map[string][]string{
		"app.example.com. A":     {"203.0.114.7"},
		"*.example.com. A":       {"203.0.114.8"},
		"localhost. A":           {"203.0.114.9"},
		"_acme.example.com. TXT": {"token-value"},
	})
	ctx := context.Background()

	for _, tc := range []struct{ name, kind, want string }{
		{"app.example.com", "A", "203.0.114.7"},
		{"*.example.com", "A", "203.0.114.8"},
		// /etc/hosts says 127.0.0.1; the server says otherwise
		{"localhost", "A", "203.0.114.9"},
		{"_acme.example.com", "TXT", "token-value"},
	} {
		values, err := lookupAt(ctx, server, tc.name, tc.kind)
		if err != nil || len(values) != 1 || values[0] != tc.want {
			t.Errorf("%s %s: %v %v", tc.name, tc.kind, values, err)
		}
	}

	// a name that does not exist has no values, and is no error
	if values, err := lookupAt(ctx, server, "nothing.example.com", "A"); err != nil || len(values) != 0 {
		t.Errorf("%v %v", values, err)
	}

	checker := &Checker{Resolvers: []string{server}, Lookup: lookupAt}
	result := checker.Check(ctx, "example.com", []Record{{Name: "*", Type: "A", Value: "203.0.114.8"}})
	if !result[0].Resolved {
		t.Errorf("a wildcard does not resolve: %+v", result)
	}
}

// TestAnACMEAnswerIsVisibleOnEveryNameServer: the answer may sit next to
// other values, as a name and its wildcard share one, but every name server
// must have it.
func TestAnACMEAnswerIsVisibleOnEveryNameServer(t *testing.T) {
	ready := serveDNS(t, map[string][]string{"_acme-challenge.example.com. TXT": {"other", "answer"}})
	behind := serveDNS(t, map[string][]string{})
	servers := []string{ready}
	checker := &Checker{Lookup: lookupAt, NameServers: func(_ context.Context, zone string) ([]string, error) {
		if zone != "example.com" {
			t.Errorf("asked for the name servers of %q", zone)
		}
		return servers, nil
	}}
	ctx := context.Background()

	if ok, err := checker.Visible(ctx, "example.com", "_acme-challenge.example.com.", "TXT", `"answer"`); !ok || err != nil {
		t.Errorf("one server with the answer: %v %v", ok, err)
	}
	servers = []string{ready, behind}
	if ok, err := checker.Visible(ctx, "example.com", "_acme-challenge.example.com.", "TXT", "answer"); ok || err != nil {
		t.Errorf("a server without it: %v %v", ok, err)
	}
	servers = nil
	if ok, err := checker.Visible(ctx, "example.com", "_acme-challenge.example.com.", "TXT", "answer"); ok || err == nil {
		t.Errorf("no name servers: %v %v", ok, err)
	}
}
