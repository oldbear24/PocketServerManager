// Package rewrite talks to the DNS servers of a LAN that answer names of their
// own (#26): the DNS rewrites of AdGuard Home and the local DNS records of
// Pi-hole. The agent of a server on that LAN runs it, as the panel usually
// cannot reach them.
//
// A record has an absolute name, such as wiki.lan, a type (A, AAAA or CNAME)
// and one value: an address, or the target of a CNAME without its final dot.
package rewrite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Kinds of servers.
const (
	AdGuard = "adguard"
	PiHole  = "pihole"
)

// Record is one answer the server gives for a name of its own.
type Record struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Key identifies a record.
func (r Record) Key() string {
	return strings.ToLower(r.Name) + " " + strings.ToUpper(r.Type) + " " + strings.ToLower(r.Value)
}

// Client reads and changes the records of one server.
type Client interface {
	List(ctx context.Context) ([]Record, error)
	Append(ctx context.Context, records []Record) error
	Delete(ctx context.Context, records []Record) error
}

// PiHoleRestartWait is how long Pi-hole may take to restart its DNS server
// after a change of its CNAME records; tests shorten it.
var PiHoleRestartWait = 10 * time.Second

// ErrUnauthorized is returned when the server refuses the credentials.
var ErrUnauthorized = errors.New("the server refused the credentials")

// New opens a client for a kind of server at its base URL, the address of its
// web interface such as http://192.168.1.2:3000.
func New(kind, baseURL, username, password string, client *http.Client) (Client, error) {
	base, err := ParseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	switch kind {
	case AdGuard:
		return &adGuard{api: api{base: base, client: client, label: "AdGuard Home"}, username: username, password: password}, nil
	case PiHole:
		// the address of its web interface ends in /admin; its API is at /api
		base.Path = strings.TrimSuffix(base.Path, "/admin")
		return &piHole{api: api{base: base, client: client, label: "Pi-hole"}, password: password, restartWait: PiHoleRestartWait}, nil
	}
	return nil, fmt.Errorf("unknown kind %q", kind)
}

// ParseURL checks the base URL of a server's web interface.
func ParseURL(raw string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("%q is not the address of a web interface, such as http://192.168.1.2:3000", raw)
	}
	base.Path = strings.TrimSuffix(base.Path, "/")
	return base, nil
}

// typeOf says which record an answer is: an IPv4 or IPv6 address, or a name.
func typeOf(value string) string {
	addr, err := netip.ParseAddr(value)
	switch {
	case err != nil:
		return "CNAME"
	case addr.Is4() || addr.Is4In6():
		return "A"
	default:
		return "AAAA"
	}
}

// check refuses a record a server cannot hold.
func check(r Record) (Record, error) {
	r.Name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(r.Name)), ".")
	r.Type = strings.ToUpper(r.Type)
	r.Value = strings.TrimSuffix(strings.TrimSpace(r.Value), ".")
	if r.Name == "" || strings.ContainsAny(r.Name, " ,/") {
		return r, fmt.Errorf("%q is not a name", r.Name)
	}
	switch r.Type {
	case "A", "AAAA":
		addr, err := netip.ParseAddr(r.Value)
		if err != nil || typeOf(r.Value) != r.Type {
			return r, fmt.Errorf("%q is not an address of an %s record", r.Value, r.Type)
		}
		r.Value = addr.Unmap().String()
	case "CNAME":
		if r.Value == "" || strings.ContainsAny(r.Value, " ,/") || typeOf(r.Value) != "CNAME" {
			return r, fmt.Errorf("%q is not the target of a CNAME", r.Value)
		}
		r.Value = strings.ToLower(r.Value)
	default:
		return r, fmt.Errorf("a %s record cannot be a rewrite: only A, AAAA and CNAME", r.Type)
	}
	return r, nil
}

// api is the HTTP side both kinds share.
type api struct {
	base   *url.URL
	client *http.Client
	label  string
}

// call sends a request and decodes the answer into out, when there is one.
// The path is escaped already; auth, when set, signs the request in.
func (a *api) call(ctx context.Context, method, path string, body any, auth func(*http.Request), out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(raw))
	}
	request, err := http.NewRequestWithContext(ctx, method, a.base.String()+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if auth != nil {
		auth(request)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return fmt.Errorf("%s at %s cannot be reached: %w", a.label, a.base.Host, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	switch {
	case response.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("%w: %s", ErrUnauthorized, answerOf(raw, response.Status))
	case response.StatusCode >= 300:
		return &StatusError{Server: a.label, Code: response.StatusCode, Message: answerOf(raw, response.Status)}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s answered something else than its API: %.120s", a.label, raw)
	}
	return nil
}

// StatusError is an answer of the server that is not a success.
type StatusError struct {
	Server  string
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s answered %d: %s", e.Server, e.Code, e.Message)
}

// answerOf is the message of an error answer, or its status.
func answerOf(raw []byte, status string) string {
	var structured struct {
		Message string `json:"message"`
		Error   struct {
			Key     string `json:"key"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &structured) == nil {
		if text := strings.TrimSpace(structured.Error.Message + " " + structured.Error.Hint); text != "" {
			return text
		}
		if structured.Message != "" {
			return structured.Message
		}
	}
	if text := strings.TrimSpace(string(raw)); text != "" && len(text) < 300 && !strings.Contains(text, "<") {
		return text
	}
	return status
}

// InZone keeps the records of names in a zone.
func InZone(records []Record, zone string) []Record {
	zone = strings.TrimSuffix(strings.ToLower(zone), ".")
	if zone == "" {
		return records
	}
	return slices.DeleteFunc(slices.Clone(records), func(r Record) bool {
		name := strings.ToLower(r.Name)
		return name != zone && !strings.HasSuffix(name, "."+zone)
	})
}
