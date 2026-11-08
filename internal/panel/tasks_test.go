package panel

import (
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/cron"
)

// TestATaskIsDueAsCronSaysSo: a schedule that names both a day of the month
// and a weekday runs on either, as cron runs it (#51).
func TestATaskIsDueAsCronSaysSo(t *testing.T) {
	at := func(text string) *cron.Moment {
		moment, err := time.Parse(time.DateTime, text)
		if err != nil {
			t.Fatal(err)
		}
		return cron.NewMoment(moment)
	}
	for _, test := range []struct {
		schedule, at string
		due          bool
	}{
		{"*/5 * * * *", "2026-11-07 12:05:00", true},
		{"*/5 * * * *", "2026-11-07 12:03:00", false},
		{"@daily", "2026-11-07 00:00:00", true},
		// the 1st and the 15th, and every Friday: 2026-11-06 is a Friday
		{"0 4 1,15 * 5", "2026-11-06 04:00:00", true},
		{"0 4 1,15 * 5", "2026-11-15 04:00:00", true},
		{"0 4 1,15 * 5", "2026-11-07 04:00:00", false},
		{"0 4 1,15 * 5", "2026-11-06 05:00:00", false},
		// a weekday alone, or a day alone, is as it says
		{"0 4 * * 5", "2026-11-06 04:00:00", true},
		{"0 4 * * 5", "2026-11-07 04:00:00", false},
		{"0 4 7 * *", "2026-11-07 04:00:00", true},
		{"not a schedule", "2026-11-07 04:00:00", false},
	} {
		if due := taskDue(test.schedule, at(test.at)); due != test.due {
			t.Errorf("%q at %s: due %v", test.schedule, test.at, due)
		}
	}
}

// TestTheRelaysTLSFollowsItsPort: port 465 speaks TLS from the start, the
// others STARTTLS, unless the relay says otherwise (#51).
func TestTheRelaysTLSFollowsItsPort(t *testing.T) {
	settings := core.NewRecord(core.NewBaseCollection("settings"))
	if got := smtpTLS(settings); got != "starttls" || smtpPort(settings) != 587 {
		t.Errorf("the defaults: %s %d", got, smtpPort(settings))
	}
	settings.Set("smtp_port", 465)
	if got := smtpTLS(settings); got != "tls" {
		t.Errorf("port 465: %s", got)
	}
	settings.Set("smtp_tls", "off")
	if got := smtpTLS(settings); got != "off" {
		t.Errorf("TLS off: %s", got)
	}
}
