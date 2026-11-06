// Package php knows the PHP versions that PocketCloud's runtime images are
// built for, from the manifest images/php/versions.json, and where each one
// stands in its support on a given day.
package php

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	manifest "github.com/oldbear24/PocketServerManager/images/php"
)

// Image is the repository of the runtime images; Tag names a version's image.
const Image = "ghcr.io/oldbear24/pocketcloud-php"

// WarnDays is how long before its security support ends a version is warned about.
const WarnDays = 90

// State is where a version stands in its support.
type State string

const (
	// Supported versions get bug fixes and security fixes (active support).
	Supported State = "supported"
	// SecurityOnly versions get security fixes only.
	SecurityOnly State = "security"
	// Ending versions lose their security support within the days given to State.
	Ending State = "ending"
	// Ended versions get no fixes at all.
	Ended State = "ended"
)

// Version is a PHP version of the manifest. Its dates are days (at midnight
// UTC), and support lasts until the end of the day named.
type Version struct {
	Version              string
	Released             time.Time
	ActiveSupportUntil   time.Time
	SecuritySupportUntil time.Time
}

// Tag is the tag of the version's image, such as 8.4-fpm.
func (v Version) Tag() string {
	return v.Version + "-fpm"
}

// DaysLeft is the number of days from the day of t, in UTC, to the last day
// of security support: 0 on that day, and below 0 once support has ended.
func (v Version) DaysLeft(t time.Time) int {
	return int(v.SecuritySupportUntil.Sub(day(t)).Hours() / 24)
}

// State tells where the version stands on the day of t, in UTC: Ending once
// at most warnDays are left of its security support.
func (v Version) State(t time.Time, warnDays int) State {
	left := v.DaysLeft(t)

	switch {
	case left < 0:
		return Ended
	case left <= warnDays:
		return Ending
	case day(t).After(v.ActiveSupportUntil):
		return SecurityOnly
	default:
		return Supported
	}
}

func day(t time.Time) time.Time {
	year, month, d := t.UTC().Date()
	return time.Date(year, month, d, 0, 0, 0, 0, time.UTC)
}

// Versions returns the versions of the manifest, the newest first.
func Versions() []Version {
	return slices.Clone(versions.Versions)
}

// Find returns the version with the name, such as "8.4".
func Find(name string) (Version, bool) {
	for _, v := range versions.Versions {
		if v.Version == name {
			return v, true
		}
	}
	return Version{}, false
}

// manifestFile is versions.json.
type manifestFile struct {
	// Debian is the release of the official images the runtime images start from.
	Debian   string
	Versions []Version
}

var versions = mustParse(manifest.Manifest)

func mustParse(content []byte) manifestFile {
	parsed, err := parse(content)
	if err != nil {
		panic(fmt.Sprintf("images/php/versions.json: %v", err))
	}
	return parsed
}

// parse reads and checks a manifest: every version once, the newest first,
// with dates in order.
func parse(content []byte) (manifestFile, error) {
	var raw struct {
		Debian   string `json:"debian"`
		Versions []struct {
			Version              string `json:"version"`
			Released             string `json:"released"`
			ActiveSupportUntil   string `json:"active_support_until"`
			SecuritySupportUntil string `json:"security_support_until"`
		} `json:"versions"`
	}

	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return manifestFile{}, err
	}

	if raw.Debian == "" {
		return manifestFile{}, errors.New("it names no Debian release")
	}
	if len(raw.Versions) == 0 {
		return manifestFile{}, errors.New("it lists no version")
	}

	parsed := manifestFile{Debian: raw.Debian}
	for i, entry := range raw.Versions {
		if _, err := versionNumbers(entry.Version); err != nil {
			return manifestFile{}, err
		}

		v := Version{Version: entry.Version}
		for _, field := range []struct {
			name  string
			value string
			into  *time.Time
		}{
			{"released", entry.Released, &v.Released},
			{"active_support_until", entry.ActiveSupportUntil, &v.ActiveSupportUntil},
			{"security_support_until", entry.SecuritySupportUntil, &v.SecuritySupportUntil},
		} {
			date, err := time.Parse(time.DateOnly, field.value)
			if err != nil {
				return manifestFile{}, fmt.Errorf("PHP %s: %s is not a date like 2026-12-31: %q", entry.Version, field.name, field.value)
			}
			*field.into = date
		}

		if v.ActiveSupportUntil.Before(v.Released) || v.SecuritySupportUntil.Before(v.ActiveSupportUntil) {
			return manifestFile{}, fmt.Errorf("PHP %s: its dates must follow each other: released, active support, security support", v.Version)
		}

		if i > 0 && !newer(parsed.Versions[i-1].Version, v.Version) {
			return manifestFile{}, fmt.Errorf("PHP %s comes after %s: the newest version must come first, and each only once", v.Version, parsed.Versions[i-1].Version)
		}

		parsed.Versions = append(parsed.Versions, v)
	}

	return parsed, nil
}

// versionNumbers reads a release series such as 8.4.
func versionNumbers(name string) ([2]int, error) {
	major, minor, ok := strings.Cut(name, ".")
	a, errMajor := strconv.Atoi(major)
	b, errMinor := strconv.Atoi(minor)
	if !ok || errMajor != nil || errMinor != nil || a < 0 || b < 0 {
		return [2]int{}, fmt.Errorf("%q is not a PHP release series like 8.4", name)
	}
	return [2]int{a, b}, nil
}

// newer tells whether release series a comes after b.
func newer(a, b string) bool {
	x, _ := versionNumbers(a)
	y, _ := versionNumbers(b)
	return x[0] > y[0] || x[0] == y[0] && x[1] > y[1]
}

// Status is a version as the panel shows it on a given day: its image, the
// dates of its support, where it stands and how many days of security fixes
// are left.
type Status struct {
	Version              string `json:"version"`
	Image                string `json:"image"`
	Released             string `json:"released"`
	ActiveSupportUntil   string `json:"active_support_until"`
	SecuritySupportUntil string `json:"security_support_until"`
	State                State  `json:"state"`
	DaysLeft             int    `json:"days_left"`
}

// Statuses returns every version of the manifest as it stands on the day of
// t, the newest first, warned about from warnDays before its support ends.
func Statuses(t time.Time, warnDays int) []Status {
	out := make([]Status, 0, len(versions.Versions))
	for _, v := range versions.Versions {
		out = append(out, Status{
			Version:              v.Version,
			Image:                Image + ":" + v.Tag(),
			Released:             v.Released.Format(time.DateOnly),
			ActiveSupportUntil:   v.ActiveSupportUntil.Format(time.DateOnly),
			SecuritySupportUntil: v.SecuritySupportUntil.Format(time.DateOnly),
			State:                v.State(t, warnDays),
			DaysLeft:             v.DaysLeft(t),
		})
	}
	return out
}

// Compare orders two versions such as 8.3 and 8.10: -1, 0 or 1.
func Compare(a, b string) int {
	parse := func(text string) []int {
		parts := []int{}
		for _, part := range strings.Split(text, ".") {
			n, _ := strconv.Atoi(part)
			parts = append(parts, n)
		}
		return parts
	}
	x, y := parse(a), parse(b)
	for i := range max(len(x), len(y)) {
		var p, q int
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		if p != q {
			if p < q {
				return -1
			}
			return 1
		}
	}
	return 0
}
