package e2e

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/sites"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// TestAPHPSitesMailGoesThroughThePanelsRelay: the SMTP relay of the
// settings (#51) reaches every PHP site's PHP-FPM, its password as a secret
// that no record shows; the site page counts the mail sent and lists the
// failures, from msmtp's lines in the container's log.
func TestAPHPSitesMailGoesThroughThePanelsRelay(t *testing.T) {
	s := newSitesHarness(t)
	_, admin := testutil.NewUser(t, s.app, "admin@example.com", schema.RoleAdmin)
	siteID := s.newSite(map[string]any{"name": "shop", "type": schema.SitePHPApp, "source": sites.SourceUpload})
	status, out := s.request(http.MethodPost, "/api/pc/sites/"+siteID+"/php/apply", s.operator, nil)
	if status != http.StatusOK {
		t.Fatalf("apply: %d %v", status, out)
	}
	s.waitForStatus(out["command"].(string), schema.CommandSucceeded, schema.CommandFailed)
	// without a relay, the site page says there is none
	if status, out := s.request(http.MethodGet, "/api/pc/sites/"+siteID+"/php/mail", s.viewer, nil); status != http.StatusOK || out["relay"] != nil {
		t.Errorf("no relay: %d %v", status, out)
	}

	const password = "relay-sekrit-4711"
	relay := map[string]any{"host": "relay.example.org", "port": 2525, "tls": "off", "user": "shop", "password": password, "from": "noreply@example.org"}
	if status, _ := s.request(http.MethodPut, "/api/pc/settings/smtp", s.operator, relay); status != http.StatusForbidden {
		t.Errorf("an operator set the relay: %d", status)
	}
	for _, bad := range []map[string]any{{"host": "relay example"}, {"port": 70000}, {"tls": "maybe"}, {"user": `sh"op`},
		// msmtp needs an envelope sender
		{"host": "relay.example.org"}, {"host": "relay.example.org", "from": ""}} {
		if status, _ := s.request(http.MethodPut, "/api/pc/settings/smtp", admin, bad); status != http.StatusBadRequest {
			t.Errorf("%v was taken: %d", bad, status)
		}
	}
	status, out = s.request(http.MethodPut, "/api/pc/settings/smtp", admin, relay)
	if status != http.StatusOK || out["password_set"] != true || out["applied"] != float64(1) {
		t.Fatalf("the relay: %d %v", status, out)
	}
	// the running PHP-FPM takes it
	s.waitFor("PHP-FPM to have the relay", func() bool {
		container := s.docker.Find("shop-php")
		if container == nil {
			return false
		}
		env, _ := container.Config["Env"].([]any)
		var have []string
		for _, item := range env {
			have = append(have, item.(string))
		}
		return slices.Contains(have, "POCKETCLOUD_SMTP_HOST=relay.example.org") && slices.Contains(have, "POCKETCLOUD_SMTP_PORT=2525") &&
			slices.Contains(have, "POCKETCLOUD_SMTP_TLS=off") && slices.Contains(have, "POCKETCLOUD_SMTP_USER=shop") &&
			slices.Contains(have, "POCKETCLOUD_SMTP_FROM=noreply@example.org") && slices.Contains(have, "POCKETCLOUD_SMTP_PASSWORD="+password)
	})
	// no record shows the password
	for _, url := range []string{"/api/collections/settings/records", "/api/collections/commands/records?perPage=200"} {
		if _, raw := testutil.Do(t, s.handler, testutil.Request{URL: url, Token: admin}); strings.Contains(raw, password) {
			t.Errorf("%s shows the password", url)
		}
	}

	// msmtp's lines of the day: one sent, two failed
	s.docker.WriteLog("shop-php", "stderr", "Nov 07 18:32:24 host=relay.example.org tls=off auth=on user=shop from=noreply@example.org recipients=a@example.org errormsg='cannot connect to relay.example.org, port 2525: Connection refused' exitcode=EX_TEMPFAIL")
	s.docker.WriteLog("shop-php", "stderr", "Nov 07 18:32:42 host=relay.example.org tls=off auth=on user=shop from=noreply@example.org recipients=b@example.org mailsize=163 smtpstatus=250 smtpmsg='250 2.0.0 Ok: queued' exitcode=EX_OK")
	s.docker.WriteLog("shop-php", "stderr", "Nov 07 18:33:00 host=relay.example.org tls=off auth=on user=shop from=noreply@example.org recipients=c@example.org smtpstatus=550 smtpmsg='550 5.7.1 Relaying denied' errormsg='recipient address c@example.org not accepted by the server' exitcode=EX_DATAERR")
	status, out = s.request(http.MethodGet, "/api/pc/sites/"+siteID+"/php/mail", s.viewer, nil)
	failures, _ := out["failures"].([]any)
	if status != http.StatusOK || out["sent"] != float64(1) || out["failed"] != float64(2) || len(failures) != 2 ||
		failures[0].(map[string]any)["recipients"] != "c@example.org" || out["relay"].(map[string]any)["host"] != "relay.example.org" {
		t.Fatalf("the mail: %d %v", status, out)
	}
	if reads, _ := s.app.FindRecordsByFilter(schema.Commands, "type = 'site.php.mail'", "", 0, 0); len(reads) != 0 {
		t.Errorf("%d reads of the mail are kept", len(reads))
	}

	// no relay: mail is off, and PHP-FPM is told so
	status, out = s.request(http.MethodPut, "/api/pc/settings/smtp", admin, map[string]any{"host": ""})
	if status != http.StatusOK {
		t.Fatalf("no relay: %d %v", status, out)
	}
	s.waitFor("PHP-FPM to lose the relay", func() bool {
		container := s.docker.Find("shop-php")
		if container == nil {
			return false
		}
		env, _ := container.Config["Env"].([]any)
		for _, item := range env {
			if strings.HasPrefix(item.(string), "POCKETCLOUD_SMTP_") {
				return false
			}
		}
		return true
	})
}
