package php

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func date(t *testing.T, value string) time.Time {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func find(t *testing.T, name string) Version {
	t.Helper()

	v, ok := Find(name)
	if !ok {
		t.Fatalf("PHP %s is not in the manifest", name)
	}
	return v
}

// The dates of php.net/supported-versions.php, as of November 2026.
func TestManifestHasTheDatesOfPHP(t *testing.T) {
	want := map[string][3]string{
		"8.5": {"2025-11-20", "2027-12-31", "2029-12-31"},
		"8.4": {"2024-11-21", "2026-12-31", "2028-12-31"},
		"8.3": {"2023-11-23", "2025-12-31", "2027-12-31"},
		"8.2": {"2022-12-08", "2024-12-31", "2026-12-31"},
	}

	var names []string
	for _, v := range Versions() {
		names = append(names, v.Version)

		dates, ok := want[v.Version]
		if !ok {
			t.Errorf("PHP %s is new to this test", v.Version)
			continue
		}
		got := [3]string{
			v.Released.Format(time.DateOnly),
			v.ActiveSupportUntil.Format(time.DateOnly),
			v.SecuritySupportUntil.Format(time.DateOnly),
		}
		if got != dates {
			t.Errorf("PHP %s: %v, want %v", v.Version, got, dates)
		}
	}

	if strings.Join(names, " ") != "8.5 8.4 8.3 8.2" {
		t.Errorf("versions %v", names)
	}
	if versions.Debian != "trixie" {
		t.Errorf("Debian %q", versions.Debian)
	}
}

func TestStates(t *testing.T) {
	for _, tc := range []struct {
		version string
		at      string
		state   State
		left    int
	}{
		// the day of the issue: PHP 8.2 has 57 days of security fixes left
		{"8.2", "2026-11-04T12:00:00Z", Ending, 57},
		{"8.3", "2026-11-04T12:00:00Z", SecurityOnly, 422},
		{"8.4", "2026-11-04T12:00:00Z", Supported, 788},
		{"8.5", "2026-11-04T12:00:00Z", Supported, 1153},

		// a warning from 90 days before the last day of security support
		{"8.2", "2026-10-01T23:59:59Z", SecurityOnly, 91},
		{"8.2", "2026-10-02T00:00:00Z", Ending, 90},
		// support lasts through the day named, in UTC
		{"8.2", "2026-12-31T23:59:59Z", Ending, 0},
		{"8.2", "2027-01-01T00:00:00Z", Ended, -1},
		{"8.2", "2026-12-31T20:00:00-05:00", Ended, -1},
		{"8.2", "2030-01-01T00:00:00Z", Ended, -1097},

		// active support ends, security fixes go on (2028 is a leap year)
		{"8.4", "2026-12-31T23:59:59Z", Supported, 731},
		{"8.4", "2027-01-01T00:00:00Z", SecurityOnly, 730},
		{"8.4", "2028-12-31T00:00:00Z", Ending, 0},
	} {
		v := find(t, tc.version)
		at := date(t, tc.at)

		if got := v.State(at, WarnDays); got != tc.state {
			t.Errorf("PHP %s on %s: %s, want %s", tc.version, tc.at, got, tc.state)
		}
		if got := v.DaysLeft(at); got != tc.left {
			t.Errorf("PHP %s on %s: %d days left, want %d", tc.version, tc.at, got, tc.left)
		}
	}
}

func TestWarningDaysAreAParameter(t *testing.T) {
	v := find(t, "8.3")
	at := date(t, "2026-11-04T12:00:00Z")

	if got := v.State(at, 500); got != Ending {
		t.Errorf("with 500 days: %s", got)
	}
	if got := v.State(at, 0); got != SecurityOnly {
		t.Errorf("with no days: %s", got)
	}
}

func TestFind(t *testing.T) {
	if v, ok := Find("8.4"); !ok || v.Tag() != "8.4-fpm" {
		t.Errorf("8.4: %v %v", v, ok)
	}
	for _, name := range []string{"7.4", "8", "8.4.1", ""} {
		if _, ok := Find(name); ok {
			t.Errorf("%q was found", name)
		}
	}
}

func TestVersionsIsACopy(t *testing.T) {
	list := Versions()
	list[0].Version = "changed"

	if Versions()[0].Version == "changed" {
		t.Fatal("a caller changed the manifest")
	}
}

func TestParseRejectsBadManifests(t *testing.T) {
	entry := func(version, released, active, security string) string {
		return `{"version":"` + version + `","released":"` + released + `","active_support_until":"` + active + `","security_support_until":"` + security + `"}`
	}
	good := entry("8.5", "2025-11-20", "2027-12-31", "2029-12-31")

	for name, content := range map[string]string{
		"no Debian":      `{"versions":[` + good + `]}`,
		"no version":     `{"debian":"trixie","versions":[]}`,
		"unknown field":  `{"debian":"trixie","versions":[` + good + `],"extra":1}`,
		"bad version":    `{"debian":"trixie","versions":[` + entry("8", "2025-11-20", "2027-12-31", "2029-12-31") + `]}`,
		"bad date":       `{"debian":"trixie","versions":[` + entry("8.5", "2025-11-20", "31 Dec 2027", "2029-12-31") + `]}`,
		"dates reversed": `{"debian":"trixie","versions":[` + entry("8.5", "2025-11-20", "2029-12-31", "2027-12-31") + `]}`,
		"oldest first":   `{"debian":"trixie","versions":[` + entry("8.4", "2024-11-21", "2026-12-31", "2028-12-31") + `,` + good + `]}`,
		"twice":          `{"debian":"trixie","versions":[` + good + `,` + good + `]}`,
	} {
		if _, err := parse([]byte(content)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	if _, err := parse([]byte(`{"debian":"trixie","versions":[` + good + `]}`)); err != nil {
		t.Errorf("a good manifest: %v", err)
	}
}

// The Dockerfile builds the newest version on the manifest's Debian release
// when CI does not say otherwise, as local builds do.
func TestDockerfileDefaultsFollowTheManifest(t *testing.T) {
	dockerfile, err := os.ReadFile("../../images/php/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}

	defaults := map[string]string{}
	for _, match := range regexp.MustCompile(`(?m)^ARG (PHP_VERSION|DEBIAN_RELEASE)=(\S+)$`).FindAllStringSubmatch(string(dockerfile), -1) {
		defaults[match[1]] = match[2]
	}

	if defaults["PHP_VERSION"] != Versions()[0].Version {
		t.Errorf("the Dockerfile builds PHP %q by default, the newest is %s", defaults["PHP_VERSION"], Versions()[0].Version)
	}
	if defaults["DEBIAN_RELEASE"] != versions.Debian {
		t.Errorf("the Dockerfile starts from Debian %q by default, the manifest says %s", defaults["DEBIAN_RELEASE"], versions.Debian)
	}
}

func TestStatuses(t *testing.T) {
	statuses := Statuses(date(t, "2026-11-04T12:00:00Z"), WarnDays)
	if len(statuses) != 4 {
		t.Fatalf("%d statuses", len(statuses))
	}

	oldest := statuses[3]
	want := Status{
		Version:              "8.2",
		Image:                "ghcr.io/oldbear24/pocketcloud-php:8.2-fpm",
		Released:             "2022-12-08",
		ActiveSupportUntil:   "2024-12-31",
		SecuritySupportUntil: "2026-12-31",
		State:                Ending,
		DaysLeft:             57,
	}
	if oldest != want {
		t.Errorf("%+v, want %+v", oldest, want)
	}
	if statuses[0].Version != "8.5" || statuses[0].State != Supported {
		t.Errorf("newest %+v", statuses[0])
	}
}

func TestVersionsAreComparedByTheirNumbers(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{{"8.3", "8.4", -1}, {"8.4", "8.3", 1}, {"8.10", "8.9", 1}, {"8.4", "8.4", 0}, {"9.0", "8.5", 1}} {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%s, %s) = %d", c.a, c.b, got)
		}
	}
}
