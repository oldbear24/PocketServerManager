package agent

import (
	"strings"
	"testing"
)

// TestMailIsCountedFromMsmtpsLines: msmtp's line for each message, as the
// runtime image logs it, says whether the relay took it (#51).
func TestMailIsCountedFromMsmtpsLines(t *testing.T) {
	log := strings.Join([]string{
		"2026-11-07T18:30:00.000000001Z [07-Nov-2026 18:30:00] NOTICE: fpm is running, pid 1",
		"2026-11-07T18:32:24.000000001Z Nov 07 18:32:24 host=127.0.0.1 tls=off auth=off from=%F recipients=someone@example.org errormsg='cannot connect to 127.0.0.1, port 2525: Connection refused' exitcode=EX_TEMPFAIL",
		"2026-11-07T18:32:42.000000001Z Nov 07 18:32:42 host=192.0.2.2 tls=off auth=off from=%F recipients=someone@example.org mailsize=163 smtpstatus=250 smtpmsg='250 2.0.0 Ok: queued as SvxmqegvzAkEWm4WcQm3Y3' exitcode=EX_OK",
		"2026-11-07T18:33:00.000000001Z Nov 07 18:33:00 host=192.0.2.2 tls=off auth=off from=%F recipients=a@example.org,b@example.org mailsize=200 smtpstatus=550 smtpmsg='550 5.7.1 Relaying denied' errormsg='recipient address a@example.org not accepted by the server' exitcode=EX_DATAERR",
		"2026-11-07T18:34:00.000000001Z a PHP line about exitcode=1 without recipients",
	}, "\n")
	counts := countMail(strings.NewReader(log))
	if counts.Sent != 1 || counts.Failed != 2 || counts.LastSentAt != "2026-11-07T18:32:42.000000001Z" {
		t.Errorf("%+v", counts)
	}
	if len(counts.Failures) != 2 || counts.Failures[0].Recipients != "a@example.org,b@example.org" ||
		counts.Failures[0].Error != "recipient address a@example.org not accepted by the server" ||
		counts.Failures[1].Error != "cannot connect to 127.0.0.1, port 2525: Connection refused" {
		t.Errorf("failures: %+v", counts.Failures)
	}
}
