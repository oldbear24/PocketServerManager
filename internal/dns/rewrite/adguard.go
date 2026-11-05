package rewrite

import (
	"context"
	"net/http"
	"strings"
)

// adGuard holds the DNS rewrites of AdGuard Home: /control/rewrite/list, add
// and delete, with the username and password of its web interface.
type adGuard struct {
	api
	username, password string
}

type adGuardRewrite struct {
	Domain string `json:"domain"`
	Answer string `json:"answer"`
	// Enabled is there from v0.107.57; older versions leave it out, and
	// every rewrite of theirs answers
	Enabled *bool `json:"enabled,omitempty"`
}

// auth signs a request in with the web interface's username and password.
func (a *adGuard) auth(request *http.Request) {
	if a.username != "" || a.password != "" {
		request.SetBasicAuth(a.username, a.password)
	}
}

func (a *adGuard) List(ctx context.Context) ([]Record, error) {
	var rewrites []adGuardRewrite
	if err := a.call(ctx, "GET", "/control/rewrite/list", nil, a.auth, &rewrites); err != nil {
		return nil, err
	}
	records := []Record{}
	for _, rw := range rewrites {
		// "A" and "AAAA" keep the upstream answers of that type: no record
		if rw.Enabled != nil && !*rw.Enabled || rw.Answer == "A" || rw.Answer == "AAAA" {
			continue
		}
		record, err := check(Record{Name: rw.Domain, Type: typeOf(rw.Answer), Value: rw.Answer})
		if err != nil {
			continue
		}
		records = append(records, record)
	}
	return records, nil
}

func (a *adGuard) Append(ctx context.Context, records []Record) error {
	enabled := true
	for _, r := range records {
		r, err := check(r)
		if err != nil {
			return err
		}
		if err := a.call(ctx, "POST", "/control/rewrite/add", adGuardRewrite{Domain: r.Name, Answer: r.Value, Enabled: &enabled}, a.auth, nil); err != nil {
			return err
		}
	}
	return nil
}

func (a *adGuard) Delete(ctx context.Context, records []Record) error {
	// a rewrite is deleted as it is listed: the domain as written, the answer
	present, err := a.listRaw(ctx)
	if err != nil {
		return err
	}
	for _, r := range records {
		r, err := check(r)
		if err != nil {
			return err
		}
		for _, rw := range present {
			if strings.EqualFold(strings.TrimSuffix(rw.Domain, "."), r.Name) && strings.EqualFold(strings.TrimSuffix(rw.Answer, "."), r.Value) {
				if err := a.call(ctx, "POST", "/control/rewrite/delete", rw, a.auth, nil); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (a *adGuard) listRaw(ctx context.Context) ([]adGuardRewrite, error) {
	var rewrites []adGuardRewrite
	err := a.call(ctx, "GET", "/control/rewrite/list", nil, a.auth, &rewrites)
	return rewrites, err
}
