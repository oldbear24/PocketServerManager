package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/dns"
	"github.com/oldbear24/PocketServerManager/internal/dns/fakedns"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// TestACertificateAskedOfOneCAGoesToItOnly: the issue dialog of Domains &
// TLS (#27) can ask one ACME account for a certificate; the other is no
// fallback for it, and an account that is not set is refused.
func TestACertificateAskedOfOneCAGoesToItOnly(t *testing.T) {
	a := newACMEHarness(t, acmeOptions{}, "example.com")
	if status, out := a.connect("cf-token"); status != http.StatusOK {
		t.Fatalf("%d %v", status, out)
	}
	a.mustSetAccount()
	ask := func(body string) (int, string) {
		return testutil.Do(t, a.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/certificates/acme", Token: a.admin, Body: body})
	}

	if status, raw := ask(`{"names":["*.example.com"],"account":"fallback"}`); status != http.StatusBadRequest || !strings.Contains(raw, "There is no fallback ACME account") {
		t.Errorf("an account that is not set: %d %s", status, raw)
	}
	if status, raw := ask(`{"names":["wiki.lan"],"issuer":"internal","account":"primary"}`); status != http.StatusBadRequest {
		t.Errorf("an account for the internal CA: %d %s", status, raw)
	}

	other := startCA(t, a.dnsHarness, acmeOptions{})
	if status, raw := a.setAccount(schema.ACMEFallback, other.DirectoryURL); status != http.StatusOK {
		t.Fatalf("fallback: %d %s", status, raw)
	}
	status, raw := ask(`{"names":["*.example.com"],"account":"fallback","challenge":"dns-01"}`)
	if status != http.StatusCreated {
		t.Fatalf("%d %s", status, raw)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(raw), &created)
	cert := a.waitCertificate(created.ID, "to be issued", issued)
	if attempt := lastAttempt(t, cert); attempt["account"] != schema.ACMEFallback {
		t.Errorf("history: %v", attempt)
	}
	if err := other.Verify([]byte(cert.GetString("chain_pem")), "docs.example.com"); err != nil {
		t.Errorf("not from the fallback CA: %v", err)
	}
	if cert.GetString("account") != schema.ACMEFallback {
		t.Errorf("account %q", cert.GetString("account"))
	}

	// the account goes some other way than through the panel's API: the
	// renewal fails, says which account it needs, and raises the alert
	fallback, err := a.app.FindFirstRecordByData(schema.ACMEAccounts, "role", schema.ACMEFallback)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.app.Delete(fallback); err != nil {
		t.Fatal(err)
	}
	a.setCertField(cert.Id, "renew_after", types.NowDateTime().String())
	a.panel.RunACME()
	cert = a.waitCertificate(cert.Id, "to fail", func(r *core.Record) bool { return r.GetInt("attempts") > 0 })
	if !strings.Contains(cert.GetString("last_error"), "asked of the fallback ACME account alone") {
		t.Errorf("last error: %q", cert.GetString("last_error"))
	}
	// the alert follows the failed attempt
	a.waitFor("the certificate's alert", func() bool {
		alerts, _ := a.app.FindRecordsByFilter(schema.Alerts, "rule='certificate_failed' && target={:id} && status='open'", "", 0, 0, dbx.Params{"id": cert.Id})
		return len(alerts) == 1
	})

	// removed through Settings, an account leaves its certificates to the
	// accounts there are, in turn
	if status, raw := a.setAccount(schema.ACMEFallback, other.DirectoryURL); status != http.StatusOK {
		t.Fatalf("fallback: %d %s", status, raw)
	}
	if status, raw := testutil.Do(t, a.handler, testutil.Request{Method: http.MethodDelete, URL: "/api/pc/acme-accounts/fallback", Token: a.admin}); status != http.StatusNoContent {
		t.Fatalf("%d %s", status, raw)
	}
	if cert, err = a.app.FindRecordById(schema.Certificates, cert.Id); err != nil || cert.GetString("account") != "" {
		t.Errorf("still asked of the removed account: %v %q", err, cert.GetString("account"))
	}
}

// TestTheAlertOfAProviderFollowsItsManagedZones: for a kind that cannot list
// its zones, the API is asked about each; a zone that fails but is not
// managed, or is removed, no longer fails the provider (#27).
func TestTheAlertOfAProviderFollowsItsManagedZones(t *testing.T) {
	d := newDNSHarness(t, "example.com")
	fake := fakedns.New("good.com")
	dns.RegisterFake("fake27-zones", false, func(map[string]string) (dns.Provider, error) { return fake, nil })
	status, raw := testutil.Do(t, d.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/dns-providers", Token: d.admin,
		Body: `{"name":"Fake","kind":"fake27-zones","credentials":{"token":"x"},"zones":["good.com","gone.com"]}`})
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, raw)
	}
	provider, err := d.app.FindFirstRecordByData(schema.DNSProviders, "name", "Fake")
	if err != nil {
		t.Fatal(err)
	}
	if alert := openProviderAlert(t, d.app, provider.Id); alert == nil || !strings.HasPrefix(alert.GetString("message"), "Fake: gone.com: ") {
		t.Fatalf("the alert: %v", alert)
	}
	gone, err := d.app.FindFirstRecordByData(schema.DNSZones, "name", "gone.com")
	if err != nil {
		t.Fatal(err)
	}
	change := func(managed bool) {
		t.Helper()
		body := fmt.Sprintf(`{"managed":%t}`, managed)
		if status, raw := testutil.Do(t, d.handler, testutil.Request{Method: http.MethodPatch, URL: "/api/pc/dns-zones/" + gone.Id, Token: d.admin, Body: body}); status != http.StatusOK {
			t.Fatalf("%d %s", status, raw)
		}
	}

	// not managed: the provider answers for what it manages
	change(false)
	if provider, _ = d.app.FindRecordById(schema.DNSProviders, provider.Id); provider.GetString("status") != schema.DNSOK {
		t.Errorf("the provider: %s %q", provider.GetString("status"), provider.GetString("last_error"))
	}
	if alert := openProviderAlert(t, d.app, provider.Id); alert != nil {
		t.Errorf("still open: %q", alert.GetString("message"))
	}

	// managed again, it fails the provider again, until it is removed
	change(true)
	if alert := openProviderAlert(t, d.app, provider.Id); alert == nil {
		t.Error("no alert for a managed zone that fails")
	}
	if status, raw := testutil.Do(t, d.handler, testutil.Request{Method: http.MethodDelete, URL: "/api/pc/dns-zones/" + gone.Id, Token: d.admin}); status != http.StatusNoContent {
		t.Fatalf("%d %s", status, raw)
	}
	if alert := openProviderAlert(t, d.app, provider.Id); alert != nil {
		t.Errorf("open after the removal: %q", alert.GetString("message"))
	}
}

// TestAProviderThatAnswersAgainRecovers: a provider whose API failed is asked
// again by the drift check, so that it and its alert recover without anyone
// pressing Test (#27).
func TestAProviderThatAnswersAgainRecovers(t *testing.T) {
	d := newDNSHarness(t, "example.com")
	fake := fakedns.New("example.org")
	fake.Fail(errors.New("503 Service Unavailable"))
	dns.RegisterFake("fake27-down", true, func(map[string]string) (dns.Provider, error) { return fake, nil })
	status, raw := testutil.Do(t, d.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/dns-providers", Token: d.admin,
		Body: `{"name":"Down","kind":"fake27-down","credentials":{"token":"x"}}`})
	if status != http.StatusOK && status != http.StatusBadRequest {
		t.Fatalf("%d %s", status, raw)
	}
	provider, err := d.app.FindFirstRecordByData(schema.DNSProviders, "name", "Down")
	if err != nil {
		t.Fatalf("not kept: %d %s", status, raw)
	}
	if openProviderAlert(t, d.app, provider.Id) == nil {
		t.Fatal("no alert while its API fails")
	}

	// still down: the drift check asks again, and it stays
	d.panel.CheckDNSDrift()
	if openProviderAlert(t, d.app, provider.Id) == nil {
		t.Fatal("resolved while its API still fails")
	}

	fake.Fail(nil)
	d.panel.CheckDNSDrift()
	if provider, _ = d.app.FindRecordById(schema.DNSProviders, provider.Id); provider.GetString("status") != schema.DNSOK {
		t.Errorf("the provider: %s %q", provider.GetString("status"), provider.GetString("last_error"))
	}
	if alert := openProviderAlert(t, d.app, provider.Id); alert != nil {
		t.Errorf("still open: %q", alert.GetString("message"))
	}
}

func openProviderAlert(t *testing.T, app core.App, providerID string) *core.Record {
	t.Helper()
	alerts, err := app.FindRecordsByFilter(schema.Alerts, "rule='dns_provider_failed' && target={:t} && status!='resolved'", "", 0, 0, dbx.Params{"t": providerID})
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) == 0 {
		return nil
	}
	return alerts[0]
}

// TestAFailingProviderHasAnAlertUntilItAnswers: a provider whose API refuses
// it has an open alert naming it, which the Overview links to its card
// (#27); it goes once the API answers, and with the provider.
func TestAFailingProviderHasAnAlertUntilItAnswers(t *testing.T) {
	d := newDNSHarness(t, "example.com")
	status, out := d.connect("a-wrong-token")
	if status != http.StatusOK {
		t.Fatalf("%d %v", status, out)
	}
	provider := out["provider"].(map[string]any)
	id := provider["id"].(string)
	if provider["status"] != schema.DNSError {
		t.Fatalf("the provider: %v", provider)
	}
	alert := openProviderAlert(t, d.app, id)
	if alert == nil || !strings.HasPrefix(alert.GetString("message"), "Cloudflare: ") || alert.GetString("host") != "" {
		t.Fatalf("the alert: %v", alert)
	}

	// the right token: the alert is resolved
	status, raw := testutil.Do(t, d.handler, testutil.Request{Method: http.MethodPatch, URL: "/api/pc/dns-providers/" + id, Token: d.admin, Body: `{"credentials":{"api_token":"cf-token"}}`})
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, raw)
	}
	if alert := openProviderAlert(t, d.app, id); alert != nil {
		t.Errorf("still open: %v", alert.PublicExport())
	}

	// a provider removed while it fails takes its alert along
	status, out = d.connectNamed("Broken", "another-wrong-token")
	if status != http.StatusBadRequest && status != http.StatusOK {
		t.Fatalf("%d %v", status, out)
	}
	if status == http.StatusOK {
		broken := out["provider"].(map[string]any)["id"].(string)
		if openProviderAlert(t, d.app, broken) == nil {
			t.Fatal("no alert for the second provider")
		}
		if status, raw := testutil.Do(t, d.handler, testutil.Request{Method: http.MethodDelete, URL: "/api/pc/dns-providers/" + broken, Token: d.admin}); status != http.StatusNoContent {
			t.Fatalf("%d %s", status, raw)
		}
		if openProviderAlert(t, d.app, broken) != nil {
			t.Error("the removed provider's alert is still open")
		}
	}
}
