package rewrite

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// refused reads a 403 as what it is: AdGuard Home answers wrong credentials
// with 403 Forbidden, not 401.
func refused(err error) error {
	var status *StatusError
	if errors.As(err, &status) && status.Code == http.StatusForbidden {
		return fmt.Errorf("%w: %s", ErrUnauthorized, status.Message)
	}
	return err
}

func (a *adGuard) List(ctx context.Context) ([]Record, error) {
	var rewrites []adGuardRewrite
	if err := a.call(ctx, "GET", "/control/rewrite/list", nil, a.auth, &rewrites); err != nil {
		return nil, refused(err)
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
			return refused(err)
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
			// compared as List reads them: fd00:0::9 is fd00::9
			listed, err := check(Record{Name: rw.Domain, Type: typeOf(rw.Answer), Value: rw.Answer})
			if err != nil || listed.Key() != r.Key() {
				continue
			}
			if err := a.call(ctx, "POST", "/control/rewrite/delete", rw, a.auth, nil); err != nil {
				return refused(err)
			}
		}
	}
	return nil
}

func (a *adGuard) listRaw(ctx context.Context) ([]adGuardRewrite, error) {
	var rewrites []adGuardRewrite
	err := a.call(ctx, "GET", "/control/rewrite/list", nil, a.auth, &rewrites)
	return rewrites, refused(err)
}
