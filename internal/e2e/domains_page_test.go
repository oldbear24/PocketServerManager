package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

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
